package persistence

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"

	"piccolod/internal/state/paths"
)

const (
	sqliteSchemaVersion       = 4 // Incremented for WebAuthn + invite tokens
	controlPayloadVersion     = 1
	controlVolumeMetadataName = "piccolo.volume.json"
)

const defaultCheckpointInterval = time.Minute

var defaultCheckpointFn = func(db *sql.DB) error {
	_, err := db.Exec(`PRAGMA wal_checkpoint(PASSIVE);`)
	return err
}

type controlPayload struct {
	Version         int           `json:"version"`
	AuthInitialized bool          `json:"auth_initialized"`
	Remote          *RemoteConfig `json:"remote,omitempty"`
	Apps            []AppRecord   `json:"apps,omitempty"`
	PasswordHash    string        `json:"password_hash,omitempty"`
	PasswordStale   bool          `json:"password_stale,omitempty"`
	PasswordStaleAt string        `json:"password_stale_at,omitempty"`
	PasswordAckAt   string        `json:"password_ack_at,omitempty"`
	RecoveryStale   bool          `json:"recovery_stale,omitempty"`
	RecoveryStaleAt string        `json:"recovery_stale_at,omitempty"`
	RecoveryAckAt   string        `json:"recovery_ack_at,omitempty"`
	Revision        uint64        `json:"revision"`
	Checksum        string        `json:"checksum"`
}

type sqliteControlStore struct {
	mu                 sync.RWMutex
	db                 *sql.DB
	path               string
	mountDir           string
	metaDir            string
	keySource          keyProvider
	loaded             bool
	readOnly           bool
	checkpointFn       func(*sql.DB) error
	checkpointInterval time.Duration
	lastCheckpoint     time.Time
	state              controlState
}

type keyProvider interface {
	WithSDEK(func([]byte) error) error
}

type controlState struct {
	authInitialized bool
	remoteConfig    *RemoteConfig
	apps            map[string]AppRecord
	passwordHash    string
	passwordStale   bool
	passwordStaleAt time.Time
	passwordAckAt   time.Time
	recoveryStale   bool
	recoveryStaleAt time.Time
	recoveryAckAt   time.Time
	revision        uint64
	checksum        string
}

var detectReadOnlyMount = defaultReadOnlyDetector

func defaultReadOnlyDetector(path string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false, err
	}
	return st.Flags&unix.ST_RDONLY != 0, nil
}

func newControlStore(stateDir string, kp keyProvider) (ControlStore, error) {
	return newSQLiteControlStore(stateDir, kp)
}

func newSQLiteControlStore(stateDir string, kp keyProvider) (*sqliteControlStore, error) {
	if kp == nil {
		return nil, ErrCryptoUnavailable
	}
	base := stateDir
	if base == "" {
		base = paths.CoreRoot()
	}
	mountDir := filepath.Join(base, "mounts", "control-plane")
	metaDir := filepath.Join(base, "volumes", "control-plane")
	// metaDir is created on-demand by ensureMetadata / writeVolumeState
	// (via EnsureVolume). Pre-creating it would cause
	// reconcileAllVolumeStates to pre-register the control-plane entry,
	// making EnsureVolume skip ciphertext subvolume creation.
	store := &sqliteControlStore{
		path:               filepath.Join(mountDir, "control.db"),
		mountDir:           mountDir,
		metaDir:            metaDir,
		keySource:          kp,
		checkpointFn:       defaultCheckpointFn,
		checkpointInterval: defaultCheckpointInterval,
		state: controlState{
			apps: make(map[string]AppRecord),
		},
	}
	return store, nil
}

func configureSQLite(db *sql.DB, readOnly bool) error {
	if _, err := db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		return fmt.Errorf("set busy timeout: %w", err)
	}
	if readOnly {
		if _, err := db.Exec(`PRAGMA query_only=1;`); err != nil {
			return fmt.Errorf("set query_only: %w", err)
		}
		return nil
	}
	// EXCLUSIVE locking stores the WAL index in heap memory instead of
	// memory-mapping a .db-shm file. If the FUSE mount disappears, SQLite
	// returns SQLITE_IOERR (catchable) instead of SIGBUS from stale mmap pages.
	if _, err := db.Exec(`PRAGMA locking_mode=EXCLUSIVE;`); err != nil {
		return fmt.Errorf("set locking mode: %w", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		return fmt.Errorf("set journal mode: %w", err)
	}
	if _, err := db.Exec(`PRAGMA synchronous=FULL;`); err != nil {
		return fmt.Errorf("set synchronous: %w", err)
	}
	return nil
}

func ensureControlVolumeMetadata(metaDir string) error {
	metaPath := filepath.Join(metaDir, controlVolumeMetadataName)
	if _, err := os.Stat(metaPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrLocked
		}
		return err
	}
	return nil
}

func applyMigrations(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			revision INTEGER NOT NULL DEFAULT 0,
			checksum TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE TABLE IF NOT EXISTS auth_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			initialized INTEGER NOT NULL DEFAULT 0,
			password_hash TEXT,
			updated_at TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE TABLE IF NOT EXISTS remote_config (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			payload BLOB NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS apps (
			name TEXT PRIMARY KEY,
			payload BLOB NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		// OIDC tables (v2 schema)
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			email TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL CHECK(role IN ('admin', 'standard')),
			allowed_apps TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS oidc_clients (
			id TEXT PRIMARY KEY,
			secret TEXT NOT NULL,
			app_id TEXT NOT NULL,
			created_at TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_oidc_clients_app_id ON oidc_clients(app_id);`,
		`CREATE TABLE IF NOT EXISTS oidc_keys (
			kid TEXT PRIMARY KEY,
			alg TEXT NOT NULL DEFAULT 'RS256',
			private_key BLOB NOT NULL,
			created_at TEXT NOT NULL,
			retired_at TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS oidc_auth_codes (
			code TEXT PRIMARY KEY,
			client_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			redirect_uri TEXT NOT NULL,
			scope TEXT NOT NULL,
			nonce TEXT,
			code_challenge TEXT,
			code_challenge_method TEXT,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_oidc_auth_codes_expires ON oidc_auth_codes(expires_at);`,
		`CREATE TABLE IF NOT EXISTS oidc_refresh_tokens (
			token TEXT PRIMARY KEY,
			client_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			scope TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_oidc_refresh_tokens_expires ON oidc_refresh_tokens(expires_at);`,
		`CREATE INDEX IF NOT EXISTS idx_oidc_refresh_tokens_user_client ON oidc_refresh_tokens(user_id, client_id);`,
		`CREATE TABLE IF NOT EXISTS oidc_config (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			encryption_key BLOB NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		// WebAuthn + invite tables (v4 schema)
		`CREATE TABLE IF NOT EXISTS webauthn_credentials (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			public_key BLOB NOT NULL,
			attestation_type TEXT NOT NULL DEFAULT 'none',
			transports TEXT,
			sign_count INTEGER NOT NULL DEFAULT 0,
			rp_id TEXT NOT NULL,
			aaguid BLOB,
			friendly_name TEXT DEFAULT '',
			backup_eligible INTEGER NOT NULL DEFAULT 0,
			backup_state INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			last_used_at TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_webauthn_creds_user_rp ON webauthn_credentials(user_id, rp_id);`,
		`CREATE TABLE IF NOT EXISTS invite_tokens (
			token TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			created_by TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			consumed_at TEXT,
			created_at TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_invite_tokens_user ON invite_tokens(user_id);`,
		`INSERT INTO meta (id, revision, checksum, updated_at)
			VALUES (1, 0, '', '')
			ON CONFLICT(id) DO NOTHING;`,
		`INSERT INTO auth_state (id, initialized, password_hash, updated_at)
			VALUES (1, 0, NULL, '')
			ON CONFLICT(id) DO NOTHING;`,
		`PRAGMA user_version=` + fmt.Sprint(sqliteSchemaVersion) + `;`,
	}
	for _, stmt := range stmts {
		if _, err = tx.Exec(stmt); err != nil {
			return err
		}
	}
	if err := ensureAuthStateColumns(tx); err != nil {
		return err
	}
	if err := ensureOIDCSchemaColumns(tx); err != nil {
		return err
	}
	if err := ensureWebAuthnBackupColumns(tx); err != nil {
		return err
	}
	err = tx.Commit()
	return err
}

func ensureAuthStateColumns(tx *sql.Tx) error {
	rows, err := tx.Query(`PRAGMA table_info(auth_state);`)
	if err != nil {
		return err
	}
	defer rows.Close()

	cols := make(map[string]bool)
	for rows.Next() {
		var (
			cid        int
			name       string
			colType    string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultVal, &pk); err != nil {
			return err
		}
		cols[strings.ToLower(name)] = true
	}
	addColumn := func(name, definition string) error {
		if cols[strings.ToLower(name)] {
			return nil
		}
		_, err := tx.Exec(fmt.Sprintf("ALTER TABLE auth_state ADD COLUMN %s %s", name, definition))
		return err
	}
	if err := addColumn("password_stale", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumn("password_stale_at", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumn("password_ack_at", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumn("recovery_stale", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumn("recovery_stale_at", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumn("recovery_ack_at", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return nil
}

// ensureOIDCSchemaColumns adds columns introduced by RFC 20260122 to OIDC tables.
func ensureOIDCSchemaColumns(tx *sql.Tx) error {
	// Add portal_session_id to oidc_auth_codes for logout propagation (RFC 20260122 §6.3)
	if err := ensureColumn(tx, "oidc_auth_codes", "portal_session_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// Add type to oidc_clients for proxy client detection (RFC 20260122 §5.3)
	if err := ensureColumn(tx, "oidc_clients", "type", "TEXT NOT NULL DEFAULT 'app'"); err != nil {
		return err
	}
	return nil
}

// ensureWebAuthnBackupColumns adds backup flags to webauthn_credentials for existing installs.
func ensureWebAuthnBackupColumns(tx *sql.Tx) error {
	if err := ensureColumn(tx, "webauthn_credentials", "backup_eligible", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(tx, "webauthn_credentials", "backup_state", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	return nil
}

// ensureColumn adds a column to a table if it doesn't already exist.
func ensureColumn(tx *sql.Tx, table, column, definition string) error {
	rows, err := tx.Query(fmt.Sprintf(`PRAGMA table_info(%s);`, table))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var defaultVal sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultVal, &pk); err != nil {
			return err
		}
		if strings.EqualFold(name, column) {
			return nil // Column already exists
		}
	}
	_, err = tx.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err
}

func (s *sqliteControlStore) Close(ctx context.Context) error {
	_ = ctx
	s.Lock()
	if s.db != nil {
		err := s.db.Close()
		s.db = nil
		return err
	}
	return nil
}

func (s *sqliteControlStore) Lock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = false
	s.state = controlState{apps: make(map[string]AppRecord)}
	s.readOnly = false
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
}

func (s *sqliteControlStore) Unlock(ctx context.Context) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return nil
	}
	if s.keySource == nil {
		return ErrCryptoUnavailable
	}
	if err := s.keySource.WithSDEK(func([]byte) error { return nil }); err != nil {
		return err
	}
	if err := s.volumeReady(); err != nil {
		return err
	}
	if ro, err := detectReadOnlyMount(s.mountDir); err == nil {
		s.readOnly = ro
	} else {
		s.readOnly = false
	}
	if err := s.openDB(); err != nil {
		return err
	}
	// Migrate admin user from auth_state to users table (v1 -> v2 schema migration)
	if !s.readOnly {
		if err := s.migrateAdminUser(ctx); err != nil {
			log.Printf("WARN: admin user migration failed: %v", err)
		}
	}
	state, err := s.loadState()
	if err != nil {
		return err
	}
	s.state = state
	s.loaded = true
	return nil
}

// migrateAdminUser migrates the existing admin password from auth_state to the users table.
// This is a one-time migration for v1 -> v2 schema upgrade.
func (s *sqliteControlStore) migrateAdminUser(ctx context.Context) error {
	// Check if migration is needed: auth_state has password_hash but users table is empty
	var usersCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&usersCount); err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if usersCount > 0 {
		return nil // Already have users, no migration needed
	}

	// Check if auth_state has a password hash
	var initialized int
	var passwordHash sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT initialized, password_hash FROM auth_state WHERE id=1`).Scan(&initialized, &passwordHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // No auth state yet
		}
		return fmt.Errorf("read auth_state: %w", err)
	}
	if initialized != 1 || !passwordHash.Valid || passwordHash.String == "" {
		return nil // Not initialized or no password set
	}

	// Generate a unique admin user ID
	adminID, err := generateUUID()
	if err != nil {
		return fmt.Errorf("generate admin ID: %w", err)
	}
	now := formatTimestamp(time.Now().UTC())

	// Create the admin user with the existing password hash
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO users (id, username, email, password_hash, role, allowed_apps, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		adminID, "admin", "admin@piccolo.local", passwordHash.String, string(UserRoleAdmin), "null", now, now)
	if err != nil {
		return fmt.Errorf("create admin user: %w", err)
	}

	log.Printf("INFO: migrated admin user from auth_state to users table (id=%s)", adminID)
	return nil
}

// generateUUID generates a random UUID v4 string.
func generateUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // Variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func (s *sqliteControlStore) loadState() (controlState, error) {
	state := controlState{
		apps: make(map[string]AppRecord),
	}
	if s.db == nil {
		if err := s.openDB(); err != nil {
			return state, err
		}
	}
	var (
		revision int64
		checksum string
		updated  string
	)
	err := s.db.QueryRow(`SELECT revision, checksum, updated_at FROM meta WHERE id=1`).Scan(&revision, &checksum, &updated)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state, err
	}
	state.revision = uint64(revision)
	state.checksum = checksum

	var (
		initInt          int
		passwordHash     sql.NullString
		passwordStaleInt sql.NullInt64
		passwordStaleAt  sql.NullString
		passwordAckAt    sql.NullString
		recoveryStaleInt sql.NullInt64
		recoveryStaleAt  sql.NullString
		recoveryAckAt    sql.NullString
		authUpdated      string
	)
	err = s.db.QueryRow(`SELECT initialized, password_hash, password_stale, password_stale_at, password_ack_at, recovery_stale, recovery_stale_at, recovery_ack_at, updated_at FROM auth_state WHERE id=1`).Scan(
		&initInt,
		&passwordHash,
		&passwordStaleInt,
		&passwordStaleAt,
		&passwordAckAt,
		&recoveryStaleInt,
		&recoveryStaleAt,
		&recoveryAckAt,
		&authUpdated,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state, err
	}
	state.authInitialized = initInt == 1
	if passwordHash.Valid {
		state.passwordHash = passwordHash.String
	}
	if passwordStaleInt.Valid {
		state.passwordStale = passwordStaleInt.Int64 == 1
	}
	if v := strings.TrimSpace(passwordStaleAt.String); v != "" {
		state.passwordStaleAt = parseTimestamp(v)
	}
	if v := strings.TrimSpace(passwordAckAt.String); v != "" {
		state.passwordAckAt = parseTimestamp(v)
	}
	if recoveryStaleInt.Valid {
		state.recoveryStale = recoveryStaleInt.Int64 == 1
	}
	if v := strings.TrimSpace(recoveryStaleAt.String); v != "" {
		state.recoveryStaleAt = parseTimestamp(v)
	}
	if v := strings.TrimSpace(recoveryAckAt.String); v != "" {
		state.recoveryAckAt = parseTimestamp(v)
	}

	var payload []byte
	var remoteUpdated string
	err = s.db.QueryRow(`SELECT payload, updated_at FROM remote_config WHERE id=1`).Scan(&payload, &remoteUpdated)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return state, err
		}
	} else {
		cfg := RemoteConfig{Payload: append([]byte{}, payload...)}
		state.remoteConfig = &cfg
	}

	rows, err := s.db.Query(`SELECT name, payload FROM apps`)
	if err != nil {
		return state, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			name string
			data []byte
		)
		if err := rows.Scan(&name, &data); err != nil {
			return state, err
		}
		var record AppRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return state, err
		}
		state.apps[name] = record
	}
	if err := rows.Err(); err != nil {
		return state, err
	}
	return state, nil
}

func (s *sqliteControlStore) Revision(ctx context.Context) (uint64, string, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.loaded {
		return 0, "", ErrLocked
	}
	return s.state.revision, s.state.checksum, nil
}

func (s *sqliteControlStore) QuickCheck(ctx context.Context) (ControlHealthReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if !s.loaded {
		return ControlHealthReport{Status: ControlHealthStatusUnknown, Message: "control store locked", CheckedAt: now}, nil
	}
	if err := s.openDB(); err != nil {
		return ControlHealthReport{Status: ControlHealthStatusError, Message: err.Error(), CheckedAt: now}, err
	}
	rows, err := s.db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return ControlHealthReport{Status: ControlHealthStatusError, Message: err.Error(), CheckedAt: now}, err
	}
	defer rows.Close()
	var issues []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return ControlHealthReport{Status: ControlHealthStatusError, Message: err.Error(), CheckedAt: now}, err
		}
		if strings.EqualFold(strings.TrimSpace(line), "ok") {
			continue
		}
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			issues = append(issues, trimmed)
		}
	}
	if err := rows.Err(); err != nil {
		return ControlHealthReport{Status: ControlHealthStatusError, Message: err.Error(), CheckedAt: now}, err
	}
	if len(issues) == 0 {
		return ControlHealthReport{Status: ControlHealthStatusOK, Message: "ok", CheckedAt: now}, nil
	}
	return ControlHealthReport{Status: ControlHealthStatusDegraded, Message: strings.Join(issues, "; "), CheckedAt: now}, nil
}

func (s *sqliteControlStore) Auth() AuthRepo         { return &sqliteAuthRepo{store: s} }
func (s *sqliteControlStore) Remote() RemoteRepo     { return &sqliteRemoteRepo{store: s} }
func (s *sqliteControlStore) AppState() AppStateRepo { return &sqliteAppStateRepo{store: s} }
func (s *sqliteControlStore) Users() UserRepo        { return &sqliteUserRepo{store: s} }
func (s *sqliteControlStore) OIDCClients() OIDCClientRepo {
	return &sqliteOIDCClientRepo{store: s}
}
func (s *sqliteControlStore) OIDCKeys() OIDCKeyRepo { return &sqliteOIDCKeyRepo{store: s} }
func (s *sqliteControlStore) OIDCAuthCodes() OIDCAuthCodeRepo {
	return &sqliteOIDCAuthCodeRepo{store: s}
}
func (s *sqliteControlStore) OIDCRefreshTokens() OIDCRefreshTokenRepo {
	return &sqliteOIDCRefreshTokenRepo{store: s}
}
func (s *sqliteControlStore) OIDCConfig() OIDCConfigRepo {
	return &sqliteOIDCConfigRepo{store: s}
}
func (s *sqliteControlStore) WebAuthnCredentials() WebAuthnCredentialRepo {
	return &sqliteWebAuthnCredRepo{store: s}
}
func (s *sqliteControlStore) InviteTokens() InviteTokenRepo {
	return &sqliteInviteTokenRepo{store: s}
}

func (s *sqliteControlStore) ensureWritableLocked() error {
	if err := s.volumeReady(); err != nil {
		return err
	}
	if !s.loaded {
		return ErrLocked
	}
	if s.readOnly {
		return ErrLocked
	}
	return s.openDB()
}

func (s *sqliteControlStore) volumeReady() error {
	if os.Getenv("PICCOLO_ALLOW_UNMOUNTED_TESTS") != "1" {
		mounted, err := isMountPoint(s.mountDir)
		if err != nil {
			return err
		}
		if mounted {
			return nil
		}
	}
	return ensureControlVolumeMetadata(s.metaDir)
}

func (s *sqliteControlStore) withWrite(mutator func(tx *sql.Tx) error) error {
	if !s.loaded {
		return ErrLocked
	}
	if err := s.ensureWritableLocked(); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := mutator(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	payload := controlPayload{
		Version:         controlPayloadVersion,
		AuthInitialized: s.state.authInitialized,
		PasswordHash:    s.state.passwordHash,
		Revision:        s.state.revision + 1,
	}
	payload.PasswordStale = s.state.passwordStale
	if !s.state.passwordStaleAt.IsZero() {
		payload.PasswordStaleAt = formatTimestamp(s.state.passwordStaleAt)
	}
	if !s.state.passwordAckAt.IsZero() {
		payload.PasswordAckAt = formatTimestamp(s.state.passwordAckAt)
	}
	payload.RecoveryStale = s.state.recoveryStale
	if !s.state.recoveryStaleAt.IsZero() {
		payload.RecoveryStaleAt = formatTimestamp(s.state.recoveryStaleAt)
	}
	if !s.state.recoveryAckAt.IsZero() {
		payload.RecoveryAckAt = formatTimestamp(s.state.recoveryAckAt)
	}
	if s.state.remoteConfig != nil {
		rc := cloneRemoteConfig(*s.state.remoteConfig)
		payload.Remote = &rc
	}
	if len(s.state.apps) > 0 {
		payload.Apps = make([]AppRecord, 0, len(s.state.apps))
		for _, app := range s.state.apps {
			payload.Apps = append(payload.Apps, app)
		}
		sort.Slice(payload.Apps, func(i, j int) bool { return payload.Apps[i].Name < payload.Apps[j].Name })
	}
	plain, err := json.Marshal(payload)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	checksum := fmt.Sprintf("%x", sha256.Sum256(plain))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE meta SET revision=?, checksum=?, updated_at=? WHERE id=1`,
		payload.Revision, checksum, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.state.revision = payload.Revision
	s.state.checksum = checksum
	s.maybeCheckpointLocked()
	return nil
}

func (s *sqliteControlStore) updateAuthState(initialized bool, passwordHash *string) error {
	return s.withWrite(func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		hashValue := s.state.passwordHash
		if passwordHash != nil {
			hashValue = *passwordHash
		}
		if _, err := tx.Exec(`UPDATE auth_state SET initialized=?, password_hash=?, updated_at=? WHERE id=1`,
			boolToInt(initialized), hashValue, now); err != nil {
			return err
		}
		s.state.authInitialized = initialized
		if passwordHash != nil {
			s.state.passwordHash = *passwordHash
		}
		return nil
	})
}

func (s *sqliteControlStore) updateAuthStaleness(update AuthStalenessUpdate) error {
	if update.PasswordStale == nil &&
		update.PasswordStaleAt == nil &&
		update.PasswordAckAt == nil &&
		update.RecoveryStale == nil &&
		update.RecoveryStaleAt == nil &&
		update.RecoveryAckAt == nil {
		return nil
	}
	return s.withWrite(func(tx *sql.Tx) error {
		sets := make([]string, 0, 8)
		args := make([]any, 0, 8)

		if update.PasswordStale != nil {
			sets = append(sets, "password_stale=?")
			args = append(args, boolToInt(*update.PasswordStale))
			s.state.passwordStale = *update.PasswordStale
		}
		if update.PasswordStaleAt != nil {
			canon := canonicalTime(*update.PasswordStaleAt)
			sets = append(sets, "password_stale_at=?")
			args = append(args, formatTimestamp(canon))
			s.state.passwordStaleAt = canon
		}
		if update.PasswordAckAt != nil {
			canon := canonicalTime(*update.PasswordAckAt)
			sets = append(sets, "password_ack_at=?")
			args = append(args, formatTimestamp(canon))
			s.state.passwordAckAt = canon
		}
		if update.RecoveryStale != nil {
			sets = append(sets, "recovery_stale=?")
			args = append(args, boolToInt(*update.RecoveryStale))
			s.state.recoveryStale = *update.RecoveryStale
		}
		if update.RecoveryStaleAt != nil {
			canon := canonicalTime(*update.RecoveryStaleAt)
			sets = append(sets, "recovery_stale_at=?")
			args = append(args, formatTimestamp(canon))
			s.state.recoveryStaleAt = canon
		}
		if update.RecoveryAckAt != nil {
			canon := canonicalTime(*update.RecoveryAckAt)
			sets = append(sets, "recovery_ack_at=?")
			args = append(args, formatTimestamp(canon))
			s.state.recoveryAckAt = canon
		}
		if len(sets) == 0 {
			return nil
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		sets = append(sets, "updated_at=?")
		args = append(args, now, 1)
		stmt := fmt.Sprintf("UPDATE auth_state SET %s WHERE id=?", strings.Join(sets, ", "))
		if _, err := tx.Exec(stmt, args...); err != nil {
			return err
		}
		return nil
	})
}

func (s *sqliteControlStore) upsertRemoteConfig(payload []byte) error {
	return s.withWrite(func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.Exec(`INSERT INTO remote_config (id, payload, updated_at) VALUES (1, ?, ?)
			ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
			payload, now); err != nil {
			return err
		}
		cfg := RemoteConfig{Payload: append([]byte{}, payload...)}
		s.state.remoteConfig = &cfg
		return nil
	})
}

func (s *sqliteControlStore) upsertApp(record AppRecord) error {
	return s.withWrite(func(tx *sql.Tx) error {
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.Exec(`INSERT INTO apps (name, payload, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
			record.Name, data, now); err != nil {
			return err
		}
		s.state.apps[record.Name] = record
		return nil
	})
}

func (s *sqliteControlStore) maybeCheckpointLocked() {
	if s.db == nil || s.readOnly || s.checkpointFn == nil {
		return
	}
	if s.checkpointInterval > 0 && !s.lastCheckpoint.IsZero() {
		if time.Since(s.lastCheckpoint) < s.checkpointInterval {
			return
		}
	}
	if err := s.checkpointFn(s.db); err != nil {
		log.Printf("WARN: control-store checkpoint failed: %v", err)
		return
	}
	s.lastCheckpoint = time.Now().UTC()
}

func (s *sqliteControlStore) openDB() error {
	if s.db != nil {
		return nil
	}
	if s.readOnly {
		if _, err := os.Stat(s.path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return ErrLocked
			}
			return err
		}
	}
	dsn := s.path
	if s.readOnly {
		dsn = buildSQLiteDSN(s.path, true)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	if err := configureSQLite(db, s.readOnly); err != nil {
		db.Close()
		return err
	}
	// EXCLUSIVE locking in WAL mode restricts access to the single connection
	// that set the PRAGMA. Prevent the pool from opening unusable connections.
	db.SetMaxOpenConns(1)
	if !s.readOnly {
		if err := applyMigrations(db); err != nil {
			db.Close()
			return err
		}
	}
	s.db = db
	return nil
}

func cloneRemoteConfig(cfg RemoteConfig) RemoteConfig {
	dup := make([]byte, len(cfg.Payload))
	copy(dup, cfg.Payload)
	return RemoteConfig{Payload: dup}
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func canonicalTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}

func formatTimestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return canonicalTime(t).Format(time.RFC3339Nano)
}

func parseTimestamp(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return ts.UTC()
}

func buildSQLiteDSN(path string, readOnly bool) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	abs = filepath.ToSlash(abs)
	if !strings.HasPrefix(abs, "/") {
		abs = "/" + abs
	}
	u := &url.URL{Scheme: "file", Path: abs}
	if readOnly {
		u.RawQuery = "mode=ro"
	}
	return u.String()
}

var (
	_ ControlStore         = (*sqliteControlStore)(nil)
	_ lockableControlStore = (*sqliteControlStore)(nil)
)

type sqliteAuthRepo struct{ store *sqliteControlStore }

func (r *sqliteAuthRepo) IsInitialized(ctx context.Context) (bool, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return false, ErrLocked
	}
	return r.store.state.authInitialized, nil
}

func (r *sqliteAuthRepo) SetInitialized(ctx context.Context) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	if r.store.state.authInitialized {
		return nil
	}
	return r.store.updateAuthState(true, nil)
}

func (r *sqliteAuthRepo) PasswordHash(ctx context.Context) (string, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return "", ErrLocked
	}
	return r.store.state.passwordHash, nil
}

func (r *sqliteAuthRepo) SavePasswordHash(ctx context.Context, hash string) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	return r.store.updateAuthState(r.store.state.authInitialized, &hash)
}

func (r *sqliteAuthRepo) Staleness(ctx context.Context) (AuthStaleness, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return AuthStaleness{}, ErrLocked
	}
	return AuthStaleness{
		PasswordStale:   r.store.state.passwordStale,
		PasswordStaleAt: r.store.state.passwordStaleAt,
		PasswordAckAt:   r.store.state.passwordAckAt,
		RecoveryStale:   r.store.state.recoveryStale,
		RecoveryStaleAt: r.store.state.recoveryStaleAt,
		RecoveryAckAt:   r.store.state.recoveryAckAt,
	}, nil
}

func (r *sqliteAuthRepo) UpdateStaleness(ctx context.Context, update AuthStalenessUpdate) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	return r.store.updateAuthStaleness(update)
}

type sqliteRemoteRepo struct{ store *sqliteControlStore }

func (r *sqliteRemoteRepo) CurrentConfig(ctx context.Context) (RemoteConfig, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return RemoteConfig{}, ErrLocked
	}
	if r.store.state.remoteConfig == nil {
		return RemoteConfig{}, ErrNotFound
	}
	return cloneRemoteConfig(*r.store.state.remoteConfig), nil
}

func (r *sqliteRemoteRepo) SaveConfig(ctx context.Context, cfg RemoteConfig) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	copyCfg := cloneRemoteConfig(cfg)
	return r.store.upsertRemoteConfig(copyCfg.Payload)
}

type sqliteAppStateRepo struct{ store *sqliteControlStore }

func (r *sqliteAppStateRepo) ListApps(ctx context.Context) ([]AppRecord, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return nil, ErrLocked
	}
	result := make([]AppRecord, 0, len(r.store.state.apps))
	for _, app := range r.store.state.apps {
		result = append(result, app)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (r *sqliteAppStateRepo) UpsertApp(ctx context.Context, record AppRecord) error {
	if record.Name == "" {
		return errors.New("app name required")
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	return r.store.upsertApp(record)
}

// -----------------------------------------------------------------------------
// User Repository
// -----------------------------------------------------------------------------

type sqliteUserRepo struct{ store *sqliteControlStore }

func (r *sqliteUserRepo) Create(ctx context.Context, user User) error {
	if user.ID == "" || user.Username == "" || user.Email == "" {
		return errors.New("user id, username, and email required")
	}
	if user.Role != UserRoleAdmin && user.Role != UserRoleStandard {
		return errors.New("invalid role")
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	now := formatTimestamp(time.Now().UTC())
	allowedAppsJSON := "null"
	if user.AllowedApps != nil {
		data, err := json.Marshal(user.AllowedApps)
		if err != nil {
			return err
		}
		allowedAppsJSON = string(data)
	}
	_, err := r.store.db.ExecContext(ctx,
		`INSERT INTO users (id, username, email, password_hash, role, allowed_apps, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		user.ID, user.Username, user.Email, user.PasswordHash, string(user.Role), allowedAppsJSON, now, now)
	return err
}

func (r *sqliteUserRepo) Get(ctx context.Context, id string) (User, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return User{}, ErrLocked
	}
	return r.scanUser(ctx, `SELECT id, username, email, password_hash, role, allowed_apps, created_at, updated_at FROM users WHERE id=?`, id)
}

func (r *sqliteUserRepo) GetByUsername(ctx context.Context, username string) (User, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return User{}, ErrLocked
	}
	return r.scanUser(ctx, `SELECT id, username, email, password_hash, role, allowed_apps, created_at, updated_at FROM users WHERE username=?`, username)
}

func (r *sqliteUserRepo) GetByEmail(ctx context.Context, email string) (User, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return User{}, ErrLocked
	}
	return r.scanUser(ctx, `SELECT id, username, email, password_hash, role, allowed_apps, created_at, updated_at FROM users WHERE email=?`, email)
}

func (r *sqliteUserRepo) scanUser(ctx context.Context, query string, args ...any) (User, error) {
	var (
		user            User
		role            string
		allowedAppsJSON sql.NullString
		createdAt       string
		updatedAt       string
	)
	err := r.store.db.QueryRowContext(ctx, query, args...).Scan(
		&user.ID, &user.Username, &user.Email, &user.PasswordHash,
		&role, &allowedAppsJSON, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	user.Role = UserRole(role)
	if allowedAppsJSON.Valid && allowedAppsJSON.String != "null" {
		if err := json.Unmarshal([]byte(allowedAppsJSON.String), &user.AllowedApps); err != nil {
			return User{}, err
		}
	}
	user.CreatedAt = parseTimestamp(createdAt)
	user.UpdatedAt = parseTimestamp(updatedAt)
	return user, nil
}

func (r *sqliteUserRepo) List(ctx context.Context) ([]User, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return nil, ErrLocked
	}
	rows, err := r.store.db.QueryContext(ctx,
		`SELECT id, username, email, password_hash, role, allowed_apps, created_at, updated_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var (
			user            User
			role            string
			allowedAppsJSON sql.NullString
			createdAt       string
			updatedAt       string
		)
		if err := rows.Scan(&user.ID, &user.Username, &user.Email, &user.PasswordHash,
			&role, &allowedAppsJSON, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		user.Role = UserRole(role)
		if allowedAppsJSON.Valid && allowedAppsJSON.String != "null" {
			if err := json.Unmarshal([]byte(allowedAppsJSON.String), &user.AllowedApps); err != nil {
				return nil, err
			}
		}
		user.CreatedAt = parseTimestamp(createdAt)
		user.UpdatedAt = parseTimestamp(updatedAt)
		users = append(users, user)
	}
	return users, rows.Err()
}

func (r *sqliteUserRepo) Update(ctx context.Context, user User) error {
	if user.ID == "" {
		return errors.New("user id required")
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	now := formatTimestamp(time.Now().UTC())
	allowedAppsJSON := "null"
	if user.AllowedApps != nil {
		data, err := json.Marshal(user.AllowedApps)
		if err != nil {
			return err
		}
		allowedAppsJSON = string(data)
	}
	result, err := r.store.db.ExecContext(ctx,
		`UPDATE users SET username=?, email=?, password_hash=?, role=?, allowed_apps=?, updated_at=? WHERE id=?`,
		user.Username, user.Email, user.PasswordHash, string(user.Role), allowedAppsJSON, now, user.ID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *sqliteUserRepo) Delete(ctx context.Context, id string) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}

	// Delete refresh tokens first (no foreign key, so manual cascade)
	// This revokes all OIDC sessions for the deleted user
	_, err := r.store.db.ExecContext(ctx, `DELETE FROM oidc_refresh_tokens WHERE user_id=?`, id)
	if err != nil {
		return fmt.Errorf("delete user refresh tokens: %w", err)
	}
	// Delete webauthn credentials (manual cascade)
	_, err = r.store.db.ExecContext(ctx, `DELETE FROM webauthn_credentials WHERE user_id=?`, id)
	if err != nil {
		return fmt.Errorf("delete user webauthn credentials: %w", err)
	}
	// Delete invite tokens (manual cascade)
	_, err = r.store.db.ExecContext(ctx, `DELETE FROM invite_tokens WHERE user_id=?`, id)
	if err != nil {
		return fmt.Errorf("delete user invite tokens: %w", err)
	}

	result, err := r.store.db.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *sqliteUserRepo) Count(ctx context.Context) (int, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return 0, ErrLocked
	}
	var count int
	err := r.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count)
	return count, err
}

// -----------------------------------------------------------------------------
// OIDC Client Repository
// -----------------------------------------------------------------------------

type sqliteOIDCClientRepo struct{ store *sqliteControlStore }

func (r *sqliteOIDCClientRepo) Create(ctx context.Context, client OIDCClient) error {
	if client.ID == "" || client.Secret == "" || client.AppID == "" {
		return errors.New("client id, secret, and app_id required")
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	now := formatTimestamp(time.Now().UTC())
	clientType := string(client.Type)
	if clientType == "" {
		clientType = string(OIDCClientTypeApp)
	}
	_, err := r.store.db.ExecContext(ctx,
		`INSERT INTO oidc_clients (id, secret, app_id, type, created_at) VALUES (?, ?, ?, ?, ?)`,
		client.ID, client.Secret, client.AppID, clientType, now)
	return err
}

func (r *sqliteOIDCClientRepo) Get(ctx context.Context, clientID string) (OIDCClient, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return OIDCClient{}, ErrLocked
	}
	var client OIDCClient
	var createdAt string
	var clientType string
	err := r.store.db.QueryRowContext(ctx,
		`SELECT id, secret, app_id, type, created_at FROM oidc_clients WHERE id=?`, clientID).
		Scan(&client.ID, &client.Secret, &client.AppID, &clientType, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OIDCClient{}, ErrNotFound
		}
		return OIDCClient{}, err
	}
	client.Type = OIDCClientType(clientType)
	client.CreatedAt = parseTimestamp(createdAt)
	return client, nil
}

func (r *sqliteOIDCClientRepo) GetByAppID(ctx context.Context, appID string) (OIDCClient, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return OIDCClient{}, ErrLocked
	}
	var client OIDCClient
	var createdAt string
	var clientType string
	err := r.store.db.QueryRowContext(ctx,
		`SELECT id, secret, app_id, type, created_at FROM oidc_clients WHERE app_id=?`, appID).
		Scan(&client.ID, &client.Secret, &client.AppID, &clientType, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OIDCClient{}, ErrNotFound
		}
		return OIDCClient{}, err
	}
	client.Type = OIDCClientType(clientType)
	client.CreatedAt = parseTimestamp(createdAt)
	return client, nil
}

func (r *sqliteOIDCClientRepo) Delete(ctx context.Context, clientID string) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	result, err := r.store.db.ExecContext(ctx, `DELETE FROM oidc_clients WHERE id=?`, clientID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *sqliteOIDCClientRepo) DeleteByAppID(ctx context.Context, appID string) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	_, err := r.store.db.ExecContext(ctx, `DELETE FROM oidc_clients WHERE app_id=?`, appID)
	return err
}

func (r *sqliteOIDCClientRepo) List(ctx context.Context) ([]OIDCClient, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return nil, ErrLocked
	}
	rows, err := r.store.db.QueryContext(ctx,
		`SELECT id, secret, app_id, type, created_at FROM oidc_clients ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var clients []OIDCClient
	for rows.Next() {
		var client OIDCClient
		var createdAt string
		var clientType string
		if err := rows.Scan(&client.ID, &client.Secret, &client.AppID, &clientType, &createdAt); err != nil {
			return nil, err
		}
		client.Type = OIDCClientType(clientType)
		client.CreatedAt = parseTimestamp(createdAt)
		clients = append(clients, client)
	}
	return clients, rows.Err()
}

// -----------------------------------------------------------------------------
// OIDC Key Repository
// -----------------------------------------------------------------------------

type sqliteOIDCKeyRepo struct{ store *sqliteControlStore }

func (r *sqliteOIDCKeyRepo) Create(ctx context.Context, key OIDCKey) error {
	if key.KID == "" || key.Alg == "" || len(key.PrivateKey) == 0 {
		return errors.New("kid, alg, and private_key required")
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	now := formatTimestamp(time.Now().UTC())
	_, err := r.store.db.ExecContext(ctx,
		`INSERT INTO oidc_keys (kid, alg, private_key, created_at, retired_at) VALUES (?, ?, ?, ?, NULL)`,
		key.KID, key.Alg, key.PrivateKey, now)
	return err
}

func (r *sqliteOIDCKeyRepo) GetActive(ctx context.Context) ([]OIDCKey, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return nil, ErrLocked
	}
	rows, err := r.store.db.QueryContext(ctx,
		`SELECT kid, alg, private_key, created_at, retired_at FROM oidc_keys WHERE retired_at IS NULL ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []OIDCKey
	for rows.Next() {
		var key OIDCKey
		var createdAt string
		var retiredAt sql.NullString
		if err := rows.Scan(&key.KID, &key.Alg, &key.PrivateKey, &createdAt, &retiredAt); err != nil {
			return nil, err
		}
		key.CreatedAt = parseTimestamp(createdAt)
		if retiredAt.Valid {
			t := parseTimestamp(retiredAt.String)
			key.RetiredAt = &t
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (r *sqliteOIDCKeyRepo) Get(ctx context.Context, kid string) (OIDCKey, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return OIDCKey{}, ErrLocked
	}
	var key OIDCKey
	var createdAt string
	var retiredAt sql.NullString
	err := r.store.db.QueryRowContext(ctx,
		`SELECT kid, alg, private_key, created_at, retired_at FROM oidc_keys WHERE kid=?`, kid).
		Scan(&key.KID, &key.Alg, &key.PrivateKey, &createdAt, &retiredAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OIDCKey{}, ErrNotFound
		}
		return OIDCKey{}, err
	}
	key.CreatedAt = parseTimestamp(createdAt)
	if retiredAt.Valid {
		t := parseTimestamp(retiredAt.String)
		key.RetiredAt = &t
	}
	return key, nil
}

func (r *sqliteOIDCKeyRepo) Retire(ctx context.Context, kid string) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	now := formatTimestamp(time.Now().UTC())
	result, err := r.store.db.ExecContext(ctx,
		`UPDATE oidc_keys SET retired_at=? WHERE kid=? AND retired_at IS NULL`, now, kid)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// -----------------------------------------------------------------------------
// OIDC Auth Code Repository
// -----------------------------------------------------------------------------

type sqliteOIDCAuthCodeRepo struct{ store *sqliteControlStore }

func (r *sqliteOIDCAuthCodeRepo) Store(ctx context.Context, code OIDCAuthCode) error {
	if code.Code == "" || code.ClientID == "" || code.UserID == "" {
		return errors.New("code, client_id, and user_id required")
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	now := formatTimestamp(time.Now().UTC())
	expiresAt := formatTimestamp(code.ExpiresAt)
	_, err := r.store.db.ExecContext(ctx,
		`INSERT INTO oidc_auth_codes (code, client_id, user_id, redirect_uri, scope, nonce, code_challenge, code_challenge_method, portal_session_id, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		code.Code, code.ClientID, code.UserID, code.RedirectURI, code.Scope,
		code.Nonce, code.CodeChallenge, code.CodeChallengeMethod, code.PortalSessionID, expiresAt, now)
	return err
}

func (r *sqliteOIDCAuthCodeRepo) Consume(ctx context.Context, code string) (OIDCAuthCode, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return OIDCAuthCode{}, err
	}

	// Use transaction for atomic SELECT + DELETE
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return OIDCAuthCode{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var authCode OIDCAuthCode
	var expiresAt, createdAt string
	var nonce, codeChallenge, codeChallengeMethod, portalSessionID sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT code, client_id, user_id, redirect_uri, scope, nonce, code_challenge, code_challenge_method, portal_session_id, expires_at, created_at
		 FROM oidc_auth_codes WHERE code=?`, code).
		Scan(&authCode.Code, &authCode.ClientID, &authCode.UserID, &authCode.RedirectURI,
			&authCode.Scope, &nonce, &codeChallenge, &codeChallengeMethod, &portalSessionID, &expiresAt, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OIDCAuthCode{}, ErrNotFound
		}
		return OIDCAuthCode{}, err
	}

	authCode.Nonce = nonce.String
	authCode.CodeChallenge = codeChallenge.String
	authCode.CodeChallengeMethod = codeChallengeMethod.String
	authCode.PortalSessionID = portalSessionID.String
	authCode.ExpiresAt = parseTimestamp(expiresAt)
	authCode.CreatedAt = parseTimestamp(createdAt)

	// Validate expiration BEFORE deletion
	if time.Now().UTC().After(authCode.ExpiresAt) {
		// Still delete the expired code to clean up
		_, _ = tx.ExecContext(ctx, `DELETE FROM oidc_auth_codes WHERE code=?`, code)
		_ = tx.Commit()
		return OIDCAuthCode{}, ErrNotFound
	}

	// Delete the code (consume it)
	result, err := tx.ExecContext(ctx, `DELETE FROM oidc_auth_codes WHERE code=?`, code)
	if err != nil {
		return OIDCAuthCode{}, fmt.Errorf("delete auth code: %w", err)
	}

	// Verify deletion succeeded (guards against race conditions)
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return OIDCAuthCode{}, ErrNotFound
	}

	if err = tx.Commit(); err != nil {
		return OIDCAuthCode{}, fmt.Errorf("commit transaction: %w", err)
	}

	return authCode, nil
}

func (r *sqliteOIDCAuthCodeRepo) Delete(ctx context.Context, code string) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	_, err := r.store.db.ExecContext(ctx, `DELETE FROM oidc_auth_codes WHERE code=?`, code)
	return err
}

func (r *sqliteOIDCAuthCodeRepo) Cleanup(ctx context.Context) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	now := formatTimestamp(time.Now().UTC())
	_, err := r.store.db.ExecContext(ctx, `DELETE FROM oidc_auth_codes WHERE expires_at < ?`, now)
	return err
}

// -----------------------------------------------------------------------------
// OIDC Refresh Token Repository
// -----------------------------------------------------------------------------

type sqliteOIDCRefreshTokenRepo struct{ store *sqliteControlStore }

func (r *sqliteOIDCRefreshTokenRepo) Store(ctx context.Context, token OIDCRefreshToken) error {
	if token.Token == "" || token.ClientID == "" || token.UserID == "" {
		return errors.New("token, client_id, and user_id required")
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	now := formatTimestamp(time.Now().UTC())
	expiresAt := formatTimestamp(token.ExpiresAt)
	_, err := r.store.db.ExecContext(ctx,
		`INSERT INTO oidc_refresh_tokens (token, client_id, user_id, scope, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		token.Token, token.ClientID, token.UserID, token.Scope, expiresAt, now)
	return err
}

func (r *sqliteOIDCRefreshTokenRepo) Get(ctx context.Context, token string) (OIDCRefreshToken, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return OIDCRefreshToken{}, ErrLocked
	}
	var refreshToken OIDCRefreshToken
	var expiresAt, createdAt string
	err := r.store.db.QueryRowContext(ctx,
		`SELECT token, client_id, user_id, scope, expires_at, created_at FROM oidc_refresh_tokens WHERE token=?`, token).
		Scan(&refreshToken.Token, &refreshToken.ClientID, &refreshToken.UserID, &refreshToken.Scope, &expiresAt, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OIDCRefreshToken{}, ErrNotFound
		}
		return OIDCRefreshToken{}, err
	}
	refreshToken.ExpiresAt = parseTimestamp(expiresAt)
	refreshToken.CreatedAt = parseTimestamp(createdAt)
	return refreshToken, nil
}

func (r *sqliteOIDCRefreshTokenRepo) Revoke(ctx context.Context, token string) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	_, err := r.store.db.ExecContext(ctx, `DELETE FROM oidc_refresh_tokens WHERE token=?`, token)
	return err
}

func (r *sqliteOIDCRefreshTokenRepo) RevokeByUserAndClient(ctx context.Context, userID, clientID string) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	_, err := r.store.db.ExecContext(ctx,
		`DELETE FROM oidc_refresh_tokens WHERE user_id=? AND client_id=?`, userID, clientID)
	return err
}

func (r *sqliteOIDCRefreshTokenRepo) Cleanup(ctx context.Context) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	now := formatTimestamp(time.Now().UTC())
	_, err := r.store.db.ExecContext(ctx, `DELETE FROM oidc_refresh_tokens WHERE expires_at < ?`, now)
	return err
}

// -----------------------------------------------------------------------------
// OIDC Config Repository
// -----------------------------------------------------------------------------

type sqliteOIDCConfigRepo struct{ store *sqliteControlStore }

func (r *sqliteOIDCConfigRepo) GetEncryptionKey(ctx context.Context) ([]byte, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return nil, ErrLocked
	}
	var key []byte
	err := r.store.db.QueryRowContext(ctx,
		`SELECT encryption_key FROM oidc_config WHERE id=1`).Scan(&key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // Not set yet
		}
		return nil, err
	}
	return key, nil
}

func (r *sqliteOIDCConfigRepo) SetEncryptionKey(ctx context.Context, key []byte) error {
	if len(key) == 0 {
		return errors.New("encryption key required")
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	now := formatTimestamp(time.Now().UTC())
	_, err := r.store.db.ExecContext(ctx,
		`INSERT INTO oidc_config (id, encryption_key, created_at, updated_at) VALUES (1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET encryption_key=excluded.encryption_key, updated_at=excluded.updated_at`,
		key, now, now)
	return err
}

// -----------------------------------------------------------------------------
// WebAuthn Credential Repository
// -----------------------------------------------------------------------------

type sqliteWebAuthnCredRepo struct{ store *sqliteControlStore }

func (r *sqliteWebAuthnCredRepo) Create(ctx context.Context, cred WebAuthnCredential) error {
	if cred.ID == "" || cred.UserID == "" || len(cred.PublicKey) == 0 || cred.RPID == "" {
		return errors.New("credential id, user_id, public_key, and rp_id required")
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	var transportsJSON *string
	if cred.Transports != nil {
		data, err := json.Marshal(cred.Transports)
		if err != nil {
			return err
		}
		s := string(data)
		transportsJSON = &s
	}
	_, err := r.store.db.ExecContext(ctx,
		`INSERT INTO webauthn_credentials (id, user_id, public_key, attestation_type, transports, sign_count, rp_id, aaguid, friendly_name, backup_eligible, backup_state, created_at, last_used_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cred.ID, cred.UserID, cred.PublicKey, cred.AttestationType, transportsJSON,
		cred.SignCount, cred.RPID, cred.AAGUID, cred.FriendlyName,
		boolToInt(cred.BackupEligible), boolToInt(cred.BackupState),
		formatTimestamp(cred.CreatedAt), formatTimestamp(cred.LastUsedAt))
	return err
}

func (r *sqliteWebAuthnCredRepo) Get(ctx context.Context, credID string) (WebAuthnCredential, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return WebAuthnCredential{}, ErrLocked
	}
	return r.scanCred(ctx,
		`SELECT id, user_id, public_key, attestation_type, transports, sign_count, rp_id, aaguid, friendly_name, backup_eligible, backup_state, created_at, last_used_at
		 FROM webauthn_credentials WHERE id=?`, credID)
}

func (r *sqliteWebAuthnCredRepo) ListByUser(ctx context.Context, userID string) ([]WebAuthnCredential, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return nil, ErrLocked
	}
	return r.scanCreds(ctx,
		`SELECT id, user_id, public_key, attestation_type, transports, sign_count, rp_id, aaguid, friendly_name, backup_eligible, backup_state, created_at, last_used_at
		 FROM webauthn_credentials WHERE user_id=? ORDER BY created_at`, userID)
}

func (r *sqliteWebAuthnCredRepo) ListByUserAndRP(ctx context.Context, userID, rpID string) ([]WebAuthnCredential, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return nil, ErrLocked
	}
	return r.scanCreds(ctx,
		`SELECT id, user_id, public_key, attestation_type, transports, sign_count, rp_id, aaguid, friendly_name, backup_eligible, backup_state, created_at, last_used_at
		 FROM webauthn_credentials WHERE user_id=? AND rp_id=? ORDER BY created_at`, userID, rpID)
}

func (r *sqliteWebAuthnCredRepo) ListByRP(ctx context.Context, rpID string) ([]WebAuthnCredential, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return nil, ErrLocked
	}
	return r.scanCreds(ctx,
		`SELECT id, user_id, public_key, attestation_type, transports, sign_count, rp_id, aaguid, friendly_name, backup_eligible, backup_state, created_at, last_used_at
		 FROM webauthn_credentials WHERE rp_id=? ORDER BY created_at`, rpID)
}

func (r *sqliteWebAuthnCredRepo) scanCred(ctx context.Context, query string, args ...any) (WebAuthnCredential, error) {
	var (
		cred           WebAuthnCredential
		transportsJSON sql.NullString
		aaguid         []byte
		createdAt      string
		lastUsedAt     string
	)
	var backupEligible, backupState int
	err := r.store.db.QueryRowContext(ctx, query, args...).Scan(
		&cred.ID, &cred.UserID, &cred.PublicKey, &cred.AttestationType,
		&transportsJSON, &cred.SignCount, &cred.RPID, &aaguid,
		&cred.FriendlyName, &backupEligible, &backupState, &createdAt, &lastUsedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WebAuthnCredential{}, ErrNotFound
		}
		return WebAuthnCredential{}, err
	}
	if transportsJSON.Valid && transportsJSON.String != "" {
		if err := json.Unmarshal([]byte(transportsJSON.String), &cred.Transports); err != nil {
			return WebAuthnCredential{}, err
		}
	}
	cred.AAGUID = aaguid
	cred.BackupEligible = backupEligible != 0
	cred.BackupState = backupState != 0
	cred.CreatedAt = parseTimestamp(createdAt)
	cred.LastUsedAt = parseTimestamp(lastUsedAt)
	return cred, nil
}

func (r *sqliteWebAuthnCredRepo) scanCreds(ctx context.Context, query string, args ...any) ([]WebAuthnCredential, error) {
	rows, err := r.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var creds []WebAuthnCredential
	for rows.Next() {
		var (
			cred           WebAuthnCredential
			transportsJSON sql.NullString
			aaguid         []byte
			createdAt      string
			lastUsedAt     string
		)
		var backupEligible, backupState int
		if err := rows.Scan(
			&cred.ID, &cred.UserID, &cred.PublicKey, &cred.AttestationType,
			&transportsJSON, &cred.SignCount, &cred.RPID, &aaguid,
			&cred.FriendlyName, &backupEligible, &backupState, &createdAt, &lastUsedAt); err != nil {
			return nil, err
		}
		if transportsJSON.Valid && transportsJSON.String != "" {
			if err := json.Unmarshal([]byte(transportsJSON.String), &cred.Transports); err != nil {
				return nil, err
			}
		}
		cred.AAGUID = aaguid
		cred.BackupEligible = backupEligible != 0
		cred.BackupState = backupState != 0
		cred.CreatedAt = parseTimestamp(createdAt)
		cred.LastUsedAt = parseTimestamp(lastUsedAt)
		creds = append(creds, cred)
	}
	return creds, rows.Err()
}

func (r *sqliteWebAuthnCredRepo) UpdateAfterAuth(ctx context.Context, credID string, signCount uint32, lastUsed time.Time) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	result, err := r.store.db.ExecContext(ctx,
		`UPDATE webauthn_credentials SET sign_count=?, last_used_at=? WHERE id=?`,
		signCount, formatTimestamp(lastUsed), credID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *sqliteWebAuthnCredRepo) UpdateFriendlyName(ctx context.Context, credID, name string) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	result, err := r.store.db.ExecContext(ctx,
		`UPDATE webauthn_credentials SET friendly_name=? WHERE id=?`, name, credID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *sqliteWebAuthnCredRepo) Delete(ctx context.Context, credID string) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	result, err := r.store.db.ExecContext(ctx,
		`DELETE FROM webauthn_credentials WHERE id=?`, credID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *sqliteWebAuthnCredRepo) DeleteByUser(ctx context.Context, userID string) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	_, err := r.store.db.ExecContext(ctx,
		`DELETE FROM webauthn_credentials WHERE user_id=?`, userID)
	return err
}

func (r *sqliteWebAuthnCredRepo) CountByUser(ctx context.Context, userID string) (int, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return 0, ErrLocked
	}
	var count int
	err := r.store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM webauthn_credentials WHERE user_id=?`, userID).Scan(&count)
	return count, err
}

func (r *sqliteWebAuthnCredRepo) CountByUserAndRP(ctx context.Context, userID, rpID string) (int, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return 0, ErrLocked
	}
	var count int
	err := r.store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM webauthn_credentials WHERE user_id=? AND rp_id=?`, userID, rpID).Scan(&count)
	return count, err
}

func (r *sqliteWebAuthnCredRepo) CountByUserSplitRP(ctx context.Context, userID, rpID string) (int, int, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return 0, 0, ErrLocked
	}
	var total, rpCount int
	err := r.store.db.QueryRowContext(ctx,
		`SELECT COUNT(*), SUM(CASE WHEN rp_id=? THEN 1 ELSE 0 END) FROM webauthn_credentials WHERE user_id=?`,
		rpID, userID).Scan(&total, &rpCount)
	return total, rpCount, err
}

// -----------------------------------------------------------------------------
// Invite Token Repository
// -----------------------------------------------------------------------------

type sqliteInviteTokenRepo struct{ store *sqliteControlStore }

func (r *sqliteInviteTokenRepo) Create(ctx context.Context, token InviteToken) error {
	if token.Token == "" || token.UserID == "" || token.CreatedBy == "" {
		return errors.New("token, user_id, and created_by required")
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	_, err := r.store.db.ExecContext(ctx,
		`INSERT INTO invite_tokens (token, user_id, created_by, expires_at, consumed_at, created_at)
		 VALUES (?, ?, ?, ?, NULL, ?)`,
		token.Token, token.UserID, token.CreatedBy,
		formatTimestamp(token.ExpiresAt), formatTimestamp(token.CreatedAt))
	return err
}

func (r *sqliteInviteTokenRepo) Get(ctx context.Context, token string) (InviteToken, error) {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	if !r.store.loaded {
		return InviteToken{}, ErrLocked
	}
	var (
		it         InviteToken
		expiresAt  string
		consumedAt sql.NullString
		createdAt  string
	)
	err := r.store.db.QueryRowContext(ctx,
		`SELECT token, user_id, created_by, expires_at, consumed_at, created_at
		 FROM invite_tokens WHERE token=?`, token).Scan(
		&it.Token, &it.UserID, &it.CreatedBy, &expiresAt, &consumedAt, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return InviteToken{}, ErrNotFound
		}
		return InviteToken{}, err
	}
	it.ExpiresAt = parseTimestamp(expiresAt)
	if consumedAt.Valid {
		t := parseTimestamp(consumedAt.String)
		it.ConsumedAt = &t
	}
	it.CreatedAt = parseTimestamp(createdAt)
	return it, nil
}

func (r *sqliteInviteTokenRepo) Consume(ctx context.Context, token string) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	now := formatTimestamp(time.Now().UTC())
	result, err := r.store.db.ExecContext(ctx,
		`UPDATE invite_tokens SET consumed_at=? WHERE token=? AND consumed_at IS NULL`, now, token)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return errors.New("invite token not found or already consumed")
	}
	return nil
}

func (r *sqliteInviteTokenRepo) DeleteByUser(ctx context.Context, userID string) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	_, err := r.store.db.ExecContext(ctx,
		`DELETE FROM invite_tokens WHERE user_id=?`, userID)
	return err
}

func (r *sqliteInviteTokenRepo) DeleteExpired(ctx context.Context) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.store.ensureWritableLocked(); err != nil {
		return err
	}
	now := formatTimestamp(time.Now().UTC())
	_, err := r.store.db.ExecContext(ctx,
		`DELETE FROM invite_tokens WHERE expires_at < ? AND consumed_at IS NULL`, now)
	return err
}
