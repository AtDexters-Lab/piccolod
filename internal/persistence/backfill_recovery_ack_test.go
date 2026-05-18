package persistence

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"piccolod/internal/state/paths"
)

// openMigratedAuthState opens an in-memory SQLite DB and creates the
// auth_state table at the schema version *before* recovery_ack_at was added.
// Mimics a pre-Apr-28 device entering its first post-upgrade applyMigrations.
func openMigratedAuthState(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	stmts := []string{
		`CREATE TABLE auth_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			initialized INTEGER NOT NULL DEFAULT 0,
			password_hash TEXT,
			updated_at TEXT NOT NULL DEFAULT '',
			password_stale INTEGER NOT NULL DEFAULT 0,
			password_stale_at TEXT NOT NULL DEFAULT '',
			password_ack_at TEXT NOT NULL DEFAULT '',
			recovery_stale INTEGER NOT NULL DEFAULT 0,
			recovery_stale_at TEXT NOT NULL DEFAULT ''
		);`,
		`INSERT INTO auth_state (id, initialized) VALUES (1, 1);`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema setup: %v: %v", s, err)
		}
	}
	return db
}

func setAuthHash(t *testing.T, db *sql.DB, hash string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE auth_state SET password_hash=? WHERE id=1;`, hash); err != nil {
		t.Fatalf("set password_hash: %v", err)
	}
}

func readRecoveryAckAt(t *testing.T, db *sql.DB) string {
	t.Helper()
	var v sql.NullString
	if err := db.QueryRow(`SELECT recovery_ack_at FROM auth_state WHERE id=1;`).Scan(&v); err != nil {
		t.Fatalf("read recovery_ack_at: %v", err)
	}
	return v.String
}

// validSDEKRK is a 48-byte zero-filled ciphertext, base64-RawStd-encoded
// (codex iter-6 P2 #2 — minimum AES-GCM(SDEK=32)+tag(16) bytes).
const validSDEKRK = "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE"

// writeKeyset writes a keyset.json with a STRUCTURALLY-VALID recovery
// wrapper so positive-path tests don't trip the wrapper-validation gate.
// To test a corrupt wrapper specifically, use writeKeysetCustom.
//
// The sdekrk argument is ignored when empty — caller signals "want valid
// wrapper" by passing the validSDEKRK constant OR any non-empty string
// that meets the 48-byte-decoded minimum. For the explicit "empty SDEKRK"
// test case (case v), pass "" — the writer emits an empty rk field.
func writeKeyset(t *testing.T, coreRoot, sdekrk string) string {
	t.Helper()
	// 12-byte AES-GCM nonce, base64-RawStd-encoded.
	const validNonce = "AAAAAAAAAAAAAAAA"    // 12 zero bytes → 16-char base64
	const validSalt = "QUFBQUFBQUFBQUFBQUFB" // 16 'A' bytes
	return writeKeysetCustom(t, coreRoot, sdekrk, validSalt, validNonce)
}

func writeKeysetCustom(t *testing.T, coreRoot, sdekrk, rkSalt, rkNonce string) string {
	t.Helper()
	dir := filepath.Join(coreRoot, "crypto")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir keyset dir: %v", err)
	}
	p := filepath.Join(dir, "keyset.json")
	body := `{"sdek":"a","salt":"b","nonce":"c","sdek_rk":"` + sdekrk + `","rk_salt":"` + rkSalt + `","rk_nonce":"` + rkNonce + `"}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write keyset.json: %v", err)
	}
	return p
}

// runEnsureAuthStateColumns runs the migration with oldVersion=0 — exercises
// the backfill code path. Tests that want to assert "backfill does NOT
// re-fire after upgrade" should call runEnsureAuthStateColumnsAt instead.
func runEnsureAuthStateColumns(t *testing.T, db *sql.DB) error {
	return runEnsureAuthStateColumnsAt(t, db, 0)
}

func runEnsureAuthStateColumnsAt(t *testing.T, db *sql.DB, oldVersion int) error {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := ensureAuthStateColumns(tx, oldVersion); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return nil
}

// (i) password_hash set + empty recovery_ack_at + valid keyset.json → seeded.
func TestBackfillRecoveryAckAt_HappyPath(t *testing.T) {
	core := t.TempDir()
	paths.SetCoreRootForTest(t, core)
	writeKeyset(t, core, validSDEKRK)

	db := openMigratedAuthState(t)
	setAuthHash(t, db, "argon2id$dummy")

	if err := runEnsureAuthStateColumns(t, db); err != nil {
		t.Fatalf("ensureAuthStateColumns: %v", err)
	}
	got := readRecoveryAckAt(t, db)
	if got == "" {
		t.Fatalf("expected recovery_ack_at to be backfilled, got empty")
	}
}

// (ii) recovery_ack_at already non-empty → unchanged.
func TestBackfillRecoveryAckAt_AlreadyAcked(t *testing.T) {
	core := t.TempDir()
	paths.SetCoreRootForTest(t, core)
	writeKeyset(t, core, validSDEKRK)

	db := openMigratedAuthState(t)
	setAuthHash(t, db, "argon2id$dummy")
	// Simulate the column already existing AND populated (a device that
	// migrated once, got backfilled, and re-runs the migration).
	if _, err := db.Exec(`ALTER TABLE auth_state ADD COLUMN recovery_ack_at TEXT NOT NULL DEFAULT '';`); err != nil {
		t.Fatalf("pre-add column: %v", err)
	}
	const existing = "2026-04-28T00:00:00Z"
	if _, err := db.Exec(`UPDATE auth_state SET recovery_ack_at=? WHERE id=1;`, existing); err != nil {
		t.Fatalf("pre-populate ack: %v", err)
	}

	if err := runEnsureAuthStateColumns(t, db); err != nil {
		t.Fatalf("ensureAuthStateColumns: %v", err)
	}
	if got := readRecoveryAckAt(t, db); got != existing {
		t.Fatalf("expected recovery_ack_at unchanged %q, got %q", existing, got)
	}
}

// (iii) empty password_hash → unchanged (fresh device, no setup yet).
func TestBackfillRecoveryAckAt_EmptyPasswordHash(t *testing.T) {
	core := t.TempDir()
	paths.SetCoreRootForTest(t, core)
	writeKeyset(t, core, validSDEKRK)

	db := openMigratedAuthState(t)
	// password_hash left NULL by openMigratedAuthState

	if err := runEnsureAuthStateColumns(t, db); err != nil {
		t.Fatalf("ensureAuthStateColumns: %v", err)
	}
	if got := readRecoveryAckAt(t, db); got != "" {
		t.Fatalf("expected recovery_ack_at empty on fresh device, got %q", got)
	}
}

// (iv) password_hash set + no keyset.json → skip (genuinely no key to ack).
func TestBackfillRecoveryAckAt_NoKeyset(t *testing.T) {
	core := t.TempDir()
	paths.SetCoreRootForTest(t, core)
	// no keyset.json written

	db := openMigratedAuthState(t)
	setAuthHash(t, db, "argon2id$dummy")

	if err := runEnsureAuthStateColumns(t, db); err != nil {
		t.Fatalf("ensureAuthStateColumns: %v", err)
	}
	if got := readRecoveryAckAt(t, db); got != "" {
		t.Fatalf("expected recovery_ack_at empty when keyset missing, got %q", got)
	}
}

// (v) password_hash set + keyset.json present but SDEKRK empty → skip + WARN.
func TestBackfillRecoveryAckAt_EmptySDEKRK(t *testing.T) {
	core := t.TempDir()
	paths.SetCoreRootForTest(t, core)
	writeKeyset(t, core, "") // empty sdek_rk

	db := openMigratedAuthState(t)
	setAuthHash(t, db, "argon2id$dummy")

	if err := runEnsureAuthStateColumns(t, db); err != nil {
		t.Fatalf("ensureAuthStateColumns: %v", err)
	}
	if got := readRecoveryAckAt(t, db); got != "" {
		t.Fatalf("expected recovery_ack_at empty when SDEKRK empty, got %q", got)
	}
}

// (vi-a) password_hash set + unparseable keyset.json → abort with typed error.
func TestBackfillRecoveryAckAt_UnparseableKeyset_Aborts(t *testing.T) {
	core := t.TempDir()
	paths.SetCoreRootForTest(t, core)
	dir := filepath.Join(core, "crypto")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir keyset dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keyset.json"), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write garbage keyset.json: %v", err)
	}

	db := openMigratedAuthState(t)
	setAuthHash(t, db, "argon2id$dummy")

	err := runEnsureAuthStateColumns(t, db)
	if !errors.Is(err, ErrAuthMigrationDegradedStorage) {
		t.Fatalf("expected ErrAuthMigrationDegradedStorage, got %v", err)
	}
}

// (vi-b) password_hash set + keyset.json with invalid-base64 SDEKRK → abort.
func TestBackfillRecoveryAckAt_InvalidBase64SDEKRK_Aborts(t *testing.T) {
	core := t.TempDir()
	paths.SetCoreRootForTest(t, core)
	writeKeyset(t, core, "!!!not-base64!!!") // present but un-decodable

	db := openMigratedAuthState(t)
	setAuthHash(t, db, "argon2id$dummy")

	err := runEnsureAuthStateColumns(t, db)
	if !errors.Is(err, ErrAuthMigrationDegradedStorage) {
		t.Fatalf("expected ErrAuthMigrationDegradedStorage, got %v", err)
	}
}

// (vi-c) password_hash set + unreadable keyset.json (directory in place of
// file) → abort after retries with typed error.
func TestBackfillRecoveryAckAt_UnreadableKeyset_Aborts(t *testing.T) {
	core := t.TempDir()
	paths.SetCoreRootForTest(t, core)
	dir := filepath.Join(core, "crypto")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir keyset dir: %v", err)
	}
	// Plant a DIRECTORY at the keyset.json path so os.ReadFile errors with
	// EISDIR — simulates a corrupt mount / wrong fs entry. Not os.ErrNotExist
	// so it goes through the retry+abort path rather than the skip path.
	if err := os.Mkdir(filepath.Join(dir, "keyset.json"), 0o755); err != nil {
		t.Fatalf("plant directory at keyset path: %v", err)
	}

	db := openMigratedAuthState(t)
	setAuthHash(t, db, "argon2id$dummy")

	err := runEnsureAuthStateColumns(t, db)
	if !errors.Is(err, ErrAuthMigrationDegradedStorage) {
		t.Fatalf("expected ErrAuthMigrationDegradedStorage, got %v", err)
	}
}

// Idempotency: device that already ran the v5 migration (oldVersion >= 5)
// has the backfill suppressed even when recovery_ack_at is empty (e.g.,
// after /reset-password legitimately cleared it). The version gate
// distinguishes "first upgrade to v5" from "later boot post-rotation."
func TestBackfillRecoveryAckAt_DoesNotReFireAfterUpgrade(t *testing.T) {
	core := t.TempDir()
	paths.SetCoreRootForTest(t, core)
	writeKeyset(t, core, validSDEKRK)

	db := openMigratedAuthState(t)
	setAuthHash(t, db, "argon2id$dummy")
	if _, err := db.Exec(`ALTER TABLE auth_state ADD COLUMN recovery_ack_at TEXT NOT NULL DEFAULT '';`); err != nil {
		t.Fatalf("pre-add column: %v", err)
	}

	// Simulate a device that already ran v5 migration once. Backfill
	// must NOT re-fire — preserves /reset-password's clear of ack_at.
	if err := runEnsureAuthStateColumnsAt(t, db, recoveryAckBackfillVersion); err != nil {
		t.Fatalf("ensureAuthStateColumns: %v", err)
	}
	if got := readRecoveryAckAt(t, db); got != "" {
		t.Fatalf("expected recovery_ack_at empty (post-/reset-password state), got %q", got)
	}
}

// Codex iter-5 P2 #1: keyset.json has non-empty sdek_rk but missing rk_salt
// or rk_nonce — the wrapper is structurally unusable for
// UnlockWithRecoveryKey. Backfill must NOT seed ack_at because the saved
// paper words couldn't actually reset the password.
func TestBackfillRecoveryAckAt_RejectsIncompleteRecoveryWrapper(t *testing.T) {
	core := t.TempDir()
	paths.SetCoreRootForTest(t, core)
	// SDEKRK present but rk_salt empty.
	writeKeysetCustom(t, core, "QUFB", "", "AAAAAAAAAAAAAAAA")

	db := openMigratedAuthState(t)
	setAuthHash(t, db, "argon2id$dummy")
	err := runEnsureAuthStateColumns(t, db)
	if !errors.Is(err, ErrAuthMigrationDegradedStorage) {
		t.Fatalf("expected ErrAuthMigrationDegradedStorage (incomplete wrapper), got %v", err)
	}
}

// Codex iter-6 P2 #2: SDEKRK is valid base64 but ciphertext too short
// (less than nonce + AEAD tag) — UnlockWithRecoveryKey would fail at
// AEAD-Open. Backfill must reject.
func TestBackfillRecoveryAckAt_RejectsTruncatedSDEKRK(t *testing.T) {
	core := t.TempDir()
	paths.SetCoreRootForTest(t, core)
	// SDEKRK = "QUFB" decodes to 3 bytes, far below the 48-byte minimum.
	writeKeysetCustom(t, core, "QUFB", "QUFBQUFBQUFBQUFBQUFB", "AAAAAAAAAAAAAAAA")

	db := openMigratedAuthState(t)
	setAuthHash(t, db, "argon2id$dummy")
	err := runEnsureAuthStateColumns(t, db)
	if !errors.Is(err, ErrAuthMigrationDegradedStorage) {
		t.Fatalf("expected ErrAuthMigrationDegradedStorage (truncated SDEKRK), got %v", err)
	}
}

func TestBackfillRecoveryAckAt_RejectsBadNonceLength(t *testing.T) {
	core := t.TempDir()
	paths.SetCoreRootForTest(t, core)
	// SDEKRK + rk_salt valid, rk_nonce is too short (3 bytes base64).
	writeKeysetCustom(t, core, "QUFB", "QUFBQUFBQUFBQUFBQUFB", "QUE")

	db := openMigratedAuthState(t)
	setAuthHash(t, db, "argon2id$dummy")
	err := runEnsureAuthStateColumns(t, db)
	if !errors.Is(err, ErrAuthMigrationDegradedStorage) {
		t.Fatalf("expected ErrAuthMigrationDegradedStorage (bad nonce length), got %v", err)
	}
}

// Codex P1: pre-Apr-28 device that already had recovery_ack_at column from
// commit 10daa79 (with empty default). MUST backfill on first upgrade to v5
// even though the column is not "newly added" in this run. Without the
// version gate, the column-just-added gate would skip these devices and
// they'd stay in the regenerate-loop forever.
func TestBackfillRecoveryAckAt_FiresWhenColumnPreExistsButOldVersion(t *testing.T) {
	core := t.TempDir()
	paths.SetCoreRootForTest(t, core)
	writeKeyset(t, core, validSDEKRK)

	db := openMigratedAuthState(t)
	setAuthHash(t, db, "argon2id$dummy")
	if _, err := db.Exec(`ALTER TABLE auth_state ADD COLUMN recovery_ack_at TEXT NOT NULL DEFAULT '';`); err != nil {
		t.Fatalf("pre-add column: %v", err)
	}

	// Simulate pre-Apr-28 device with v4 schema (column added by 10daa79,
	// never backfilled). Migration to v5 must backfill recovery_ack_at.
	if err := runEnsureAuthStateColumnsAt(t, db, 4); err != nil {
		t.Fatalf("ensureAuthStateColumns: %v", err)
	}
	if got := readRecoveryAckAt(t, db); got == "" {
		t.Fatalf("expected recovery_ack_at backfilled on v4→v5 upgrade, got empty")
	}
}

// readKeysetForBackfill retry behavior: a transient I/O blip on the first
// attempt followed by a successful read does not abort.
func TestReadKeysetForBackfill_TransientThenSuccess(t *testing.T) {
	core := t.TempDir()
	paths.SetCoreRootForTest(t, core)
	dir := filepath.Join(core, "crypto")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	keysetPath := filepath.Join(dir, "keyset.json")
	// Plant a directory first; concurrent test goroutine swaps it for a
	// valid file after 100 ms. The first attempt fails with EISDIR; the
	// 250 ms backoff lets the swap land before retry.
	if err := os.Mkdir(keysetPath, 0o755); err != nil {
		t.Fatalf("plant dir: %v", err)
	}
	go func() {
		// Match: first backoff = 250ms; we want the swap to land mid-backoff.
		// 100ms is comfortably inside that window. Write a full valid
		// wrapper (codex iter-5 P2 #1 + iter-6 P2 #2 require rk_salt,
		// rk_nonce, AND ≥48-byte SDEKRK ciphertext).
		<-time.After(100 * time.Millisecond)
		_ = os.Remove(keysetPath)
		body := `{"sdek_rk":"` + validSDEKRK + `","rk_salt":"QUFBQUFBQUFBQUFBQUFB","rk_nonce":"AAAAAAAAAAAAAAAA"}`
		_ = os.WriteFile(keysetPath, []byte(body), 0o600)
	}()
	probe, err := readKeysetForBackfill(keysetPath)
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if probe == nil || probe.SDEKRK == "" {
		t.Fatalf("expected non-empty SDEKRK probe, got %+v", probe)
	}
}

// Sanity: the typed error wraps the underlying cause so operators can see
// what specifically went wrong in the audit/log.
func TestBackfillRecoveryAckAt_AbortErrorIncludesCause(t *testing.T) {
	core := t.TempDir()
	paths.SetCoreRootForTest(t, core)
	dir := filepath.Join(core, "crypto")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keyset.json"), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	db := openMigratedAuthState(t)
	setAuthHash(t, db, "argon2id$dummy")
	err := runEnsureAuthStateColumns(t, db)
	if err == nil || !strings.Contains(err.Error(), "parse keyset.json") {
		t.Fatalf("expected cause text in error, got %v", err)
	}
}
