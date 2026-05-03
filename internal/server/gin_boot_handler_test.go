package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/lifecycle"
	"piccolod/internal/persistence"
)

func TestBoot_FreshServer(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	srv.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["screen"] != "setup" {
		t.Fatalf("expected screen=setup, got %v", resp["screen"])
	}
}

func TestBoot_AuthenticatedDesktop(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, _ := setupTestAdminSession(t, srv)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	req.AddCookie(sessionCookie)
	srv.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["screen"] != "desktop" {
		t.Fatalf("expected screen=desktop, got %v", resp["screen"])
	}
	if resp["user"] != "admin" {
		t.Fatalf("expected user=admin, got %v", resp["user"])
	}
	if _, ok := resp["has_passkey"]; !ok {
		t.Fatalf("expected has_passkey field in desktop response")
	}
	if _, ok := resp["must_register_passkey"]; !ok {
		t.Fatalf("expected must_register_passkey field in desktop response")
	}
}

func TestBoot_NoSession(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	setupTestAdminSession(t, srv) // initialize crypto+auth but don't use the session

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	// No session cookie
	srv.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["screen"] != "login" {
		t.Fatalf("expected screen=login, got %v", resp["screen"])
	}
}

func TestBoot_MustRegisterPasskey(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, _ := setupTestAdminSession(t, srv)

	// Set MustRegisterPasskey on the session
	sess, ok := srv.sessions.Get(sessionCookie.Value)
	if !ok {
		t.Fatal("session not found")
	}
	sess.MustRegisterPasskey.Store(true)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	req.AddCookie(sessionCookie)
	srv.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["screen"] != "passkey_required" {
		t.Fatalf("expected screen=passkey_required, got %v", resp["screen"])
	}
}

func TestBoot_Locked(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	setupTestAdminSession(t, srv) // initializes crypto

	// Lock crypto + lifecycle. Production code (handleCryptoLock) keeps
	// these aligned; tests must do the same to mirror the post-lock state
	// the boot handler is meant to route on.
	srv.cryptoManager.Lock()
	if err := srv.lifecycle.Lock(); err != nil {
		t.Fatalf("lifecycle.Lock: %v", err)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	srv.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["screen"] != "unlock" {
		t.Fatalf("expected screen=unlock, got %v", resp["screen"])
	}
}

// TestBoot_Locked_DoesNotClaimRecoveryPending guards against a regression
// where the staleness gate runs on locked devices, where Staleness() always
// returns ErrLocked, fail-closing recovery_key_pending=true. The auth
// controller would then call /recovery-key/generate on unlock, which rotates
// the existing recovery key and silently invalidates the operator's saved
// words on every routine reboot. Locked boots must fall back to file-presence
// only.
func TestBoot_Locked_DoesNotClaimRecoveryPending(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	setupTestAdminSession(t, srv) // initializes crypto

	// Generate a recovery key, then lock crypto and inject the locked-state
	// read failure (the real repo returns ErrLocked when locked).
	if _, _, err := srv.cryptoManager.GenerateRecoveryKey(false); err != nil {
		t.Fatalf("GenerateRecoveryKey: %v", err)
	}
	srv.cryptoManager.Lock()
	if err := srv.lifecycle.Lock(); err != nil {
		t.Fatalf("lifecycle.Lock: %v", err)
	}
	srv.authRepo = &fakeAuthRepo{loadErr: persistence.ErrLocked}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	srv.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["screen"] != "unlock" {
		t.Fatalf("expected screen=unlock, got %v", resp["screen"])
	}
	if v, present := resp["recovery_key_pending"]; present && v == true {
		t.Fatalf("locked boot must not claim recovery_key_pending — would trigger rotation on unlock and destroy saved key (got %v)", v)
	}
}

// TestBoot_RecoveryPendingClearedAfterAck verifies the ack→gate contract:
// post-setup with a recovery key generated but unacknowledged, boot reports
// pending; once the operator acks via /crypto/recovery-key/ack with the
// matching key_id, boot stops reporting pending. This is the central
// guarantee REC-1 provides; REC-4 adds the key_id binding.
func TestBoot_RecoveryPendingClearedAfterAck(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)
	if _, _, err := srv.cryptoManager.GenerateRecoveryKey(false); err != nil {
		t.Fatalf("GenerateRecoveryKey: %v", err)
	}
	repo := &fakeAuthRepo{} // RecoveryAckAt zero
	srv.authRepo = repo
	keyID := srv.cryptoManager.RecoveryKeyID()
	if keyID == "" {
		t.Fatalf("RecoveryKeyID() should be non-empty after GenerateRecoveryKey")
	}

	// Pre-ack: boot must report pending.
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	req.AddCookie(sessionCookie)
	srv.router.ServeHTTP(w, req)
	var pre map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &pre)
	if pre["recovery_key_pending"] != true {
		t.Fatalf("pre-ack: expected recovery_key_pending=true, got %v body=%s", pre["recovery_key_pending"], w.Body.String())
	}

	// Ack via the new endpoint, supplying the key_id the operator was shown.
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/crypto/recovery-key/ack",
		strings.NewReader(fmt.Sprintf(`{"key_id":%q}`, keyID)))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ack failed: status=%d body=%s", w.Code, w.Body.String())
	}
	if repo.staleness.RecoveryAckAt.IsZero() {
		t.Fatalf("ack endpoint did not persist RecoveryAckAt")
	}

	// Post-ack: boot must NOT report pending.
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	req.AddCookie(sessionCookie)
	srv.router.ServeHTTP(w, req)
	var post map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &post)
	if v, present := post["recovery_key_pending"]; present && v == true {
		t.Fatalf("post-ack: recovery_key_pending must be cleared (got %v)", v)
	}
}

// TestBoot_RecoveryPendingFailClosedOnUnlockedReadError verifies the gate's
// fail-closed posture: on an unlocked boot, if the staleness read errors
// for any reason other than ErrLocked, recovery_key_pending must be true so
// the user is re-prompted rather than silently treated as ack'd.
func TestBoot_RecoveryPendingFailClosedOnUnlockedReadError(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, _ := setupTestAdminSession(t, srv)
	if _, _, err := srv.cryptoManager.GenerateRecoveryKey(false); err != nil {
		t.Fatalf("GenerateRecoveryKey: %v", err)
	}
	// Inject a fake auth repo that errors with a non-ErrLocked failure.
	srv.authRepo = &fakeAuthRepo{loadErr: errors.New("transient db failure")}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	req.AddCookie(sessionCookie)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["recovery_key_pending"] != true {
		t.Fatalf("expected fail-closed recovery_key_pending=true on staleness read error, got %v", resp["recovery_key_pending"])
	}
}

// TestBoot_RecoveryNotPendingWhenAcked is the happy-path direct read: a
// fully populated repo (RecoveryAckAt non-zero) on an unlocked, initialized
// device with HasRecoveryKey()=true must return recovery_key_pending absent.
// Complements the ack-via-endpoint test by asserting the gate's read of an
// already-persisted ack works independently of the endpoint write path.
func TestBoot_RecoveryNotPendingWhenAcked(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, _ := setupTestAdminSession(t, srv)
	if _, _, err := srv.cryptoManager.GenerateRecoveryKey(false); err != nil {
		t.Fatalf("GenerateRecoveryKey: %v", err)
	}
	srv.authRepo = &fakeAuthRepo{
		staleness: persistence.AuthStaleness{RecoveryAckAt: time.Now().UTC()},
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	req.AddCookie(sessionCookie)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if v, present := resp["recovery_key_pending"]; present && v == true {
		t.Fatalf("acked-state direct read must not flip pending=true; got %v", v)
	}
}

// TestLogin_SurfacesRecoveryKeyPendingForUnlockChain guards the post-unlock
// re-evaluation of the recovery-key gate. Locked boot suppresses pending=true
// (file-presence-only fallback to avoid the rotation hazard), and the
// unlock-then-login chain doesn't re-call /system/boot. The login response
// must therefore re-evaluate and surface the gate so the auth controller can
// route the operator to the recovery-key screen instead of straight to
// dashboard with an unack'd key.
func TestLogin_SurfacesRecoveryKeyPendingForUnlockChain(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	setupTestAdminSession(t, srv) // initializes crypto + admin user
	if _, _, err := srv.cryptoManager.GenerateRecoveryKey(false); err != nil {
		t.Fatalf("GenerateRecoveryKey: %v", err)
	}
	srv.authRepo = &fakeAuthRepo{} // RecoveryAckAt zero

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"username":"admin","password":"TestPass123!"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["recovery_key_pending"] != true {
		t.Fatalf("login response must surface recovery_key_pending=true on unack'd device; got %v", resp["recovery_key_pending"])
	}
}

// TestLogin_RecoveryKeyPendingFalseWhenAcked locks in the always-emit
// contract: the login response must include `recovery_key_pending` (true OR
// false) so the client can treat it as authoritative rather than OR-ing in
// the stale boot snapshot. Asserts false-case is explicit, not absent.
func TestLogin_RecoveryKeyPendingFalseWhenAcked(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	setupTestAdminSession(t, srv)
	if _, _, err := srv.cryptoManager.GenerateRecoveryKey(false); err != nil {
		t.Fatalf("GenerateRecoveryKey: %v", err)
	}
	srv.authRepo = &fakeAuthRepo{
		staleness: persistence.AuthStaleness{RecoveryAckAt: time.Now().UTC()},
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"username":"admin","password":"TestPass123!"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	v, present := resp["recovery_key_pending"]
	if !present {
		t.Fatalf("login response must always include recovery_key_pending field; got body=%s", w.Body.String())
	}
	if v != false {
		t.Fatalf("expected recovery_key_pending=false on acked device; got %v", v)
	}
}

// TestRecoveryKeyGenerate_RefusesRotationOnAckedKey guards REC-FOLLOWUP:
// /generate must refuse to rotate an already-acknowledged recovery key
// without explicit force_rotate. Without this, any false-positive on the
// recovery_key_pending gate (transient DB error, race, locked-state misclass)
// triggers the UI to call /generate, which would silently invalidate the
// operator's saved paper words.
func TestRecoveryKeyGenerate_RefusesRotationOnAckedKey(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)
	if _, _, err := srv.cryptoManager.GenerateRecoveryKey(false); err != nil {
		t.Fatalf("GenerateRecoveryKey: %v", err)
	}
	srv.authRepo = &fakeAuthRepo{
		staleness: persistence.AuthStaleness{RecoveryAckAt: time.Now().UTC()},
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/crypto/recovery-key/generate",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 conflict on acked-key rotation; got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != ErrRecoveryKeyAlreadyAcked {
		t.Fatalf("expected error=%s; got %v", ErrRecoveryKeyAlreadyAcked, resp["error"])
	}
}

// TestRecoveryKeyGenerate_AllowsRotationWithForceFlag verifies that explicit
// rotations (e.g., a future Settings → Rotate Recovery Key flow) bypass the
// REC-FOLLOWUP guard with force_rotate=true.
//
// Also pins REC-3: rotated=false when force_rotate=true. The "these words are
// new" banner is for *silent* rotation (the operator was prompted unexpectedly);
// an explicit Settings rotation has its own UX and would only confuse if it
// also raised the banner.
func TestRecoveryKeyGenerate_AllowsRotationWithForceFlag(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)
	if _, _, err := srv.cryptoManager.GenerateRecoveryKey(false); err != nil {
		t.Fatalf("GenerateRecoveryKey: %v", err)
	}
	srv.authRepo = &fakeAuthRepo{
		staleness: persistence.AuthStaleness{RecoveryAckAt: time.Now().UTC()},
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/crypto/recovery-key/generate",
		strings.NewReader(`{"force_rotate":true}`))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with force_rotate=true; got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if rotated, _ := resp["rotated"].(bool); rotated {
		t.Fatalf("expected rotated=false on force_rotate=true (explicit rotation, no banner); got rotated=true")
	}
}

// TestRecoveryKeyGenerate_AllowsRotationOnUnackedKey verifies that the
// timeout-recovery scenario still works: server has a key written but never
// acked → /generate rotates and returns fresh words (operator never had
// paper from the old key).
//
// Also pins REC-3: rotated=true when /generate replaces an unack'd previous
// key. The UI uses this signal to render the "these words are new" banner so
// an operator who saved stale paper notices the words changed.
func TestRecoveryKeyGenerate_AllowsRotationOnUnackedKey(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)
	if _, _, err := srv.cryptoManager.GenerateRecoveryKey(false); err != nil {
		t.Fatalf("GenerateRecoveryKey: %v", err)
	}
	srv.authRepo = &fakeAuthRepo{} // RecoveryAckAt zero — unack'd

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/crypto/recovery-key/generate",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on unack'd-key rotation; got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if _, ok := resp["words"]; !ok {
		t.Fatalf("expected words in response; got %v", resp)
	}
	if rotated, _ := resp["rotated"].(bool); !rotated {
		t.Fatalf("expected rotated=true on silent rotation of unack'd key (REC-3 banner trigger); got %v", resp["rotated"])
	}
}

// TestRecoveryKeyGenerate_AuditVsResponse_DivergeOnForceRotate pins the
// intentional semantic split: the audit log records `rotated: rotating`
// (mechanical fact — was a prior key replaced), while the JSON response
// carries `rotated: silentRotation` (UX signal — should the UI banner show).
// On force_rotate=true these diverge: audit=true, response=false. Without
// this test, a future "consolidate to one rotated value" refactor could
// silently break either the audit trail or the silent-rotation banner.
func TestRecoveryKeyGenerate_AuditVsResponse_DivergeOnForceRotate(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)
	if _, _, err := srv.cryptoManager.GenerateRecoveryKey(false); err != nil {
		t.Fatalf("GenerateRecoveryKey: %v", err)
	}
	srv.authRepo = &fakeAuthRepo{
		staleness: persistence.AuthStaleness{RecoveryAckAt: time.Now().UTC()},
	}

	auditCh := srv.events.Subscribe(events.TopicAudit, 8)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/crypto/recovery-key/generate",
		strings.NewReader(`{"force_rotate":true}`))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200; got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if rotated, _ := resp["rotated"].(bool); rotated {
		t.Fatalf("response: expected rotated=false on force_rotate (silent-rotation signal); got true")
	}

	select {
	case ev := <-auditCh:
		audit, ok := ev.Payload.(events.AuditEvent)
		if !ok {
			t.Fatalf("audit payload type: got %T", ev.Payload)
		}
		if audit.Kind != "auth.recovery_key_generate" {
			t.Fatalf("audit kind: got %q want auth.recovery_key_generate", audit.Kind)
		}
		if rotated, _ := audit.Metadata["rotated"].(bool); !rotated {
			t.Fatalf("audit metadata: expected rotated=true on force_rotate (mechanical fact); got %v", audit.Metadata["rotated"])
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for audit event")
	}
}

// TestRecoveryKeyGenerate_FirstTimeGen_RotatedFalse pins the first-time
// generation case for REC-3: when no recovery key existed before this call,
// rotated=false. There's no prior paper for the operator to be confused
// about, so the banner would be misleading.
func TestRecoveryKeyGenerate_FirstTimeGen_RotatedFalse(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)
	srv.authRepo = &fakeAuthRepo{} // RecoveryAckAt zero

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/crypto/recovery-key/generate",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on first-time generation; got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if rotated, _ := resp["rotated"].(bool); rotated {
		t.Fatalf("expected rotated=false on first-time generation (no prior paper to invalidate); got rotated=true")
	}
}

// TestRecoveryKeyGenerate_RefusesOnAckStateUnknown guards the fail-closed
// posture of the REC-FOLLOWUP guard: if we cannot read the ack state
// (locked, transient DB error, repo nil), rotation is refused rather than
// silently destroying the operator's saved words. Mirrors the conservative
// posture computeRecoveryKeyPending takes for the gate-decision side.
func TestRecoveryKeyGenerate_RefusesOnAckStateUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"ErrLocked", persistence.ErrLocked},
		{"transient", errors.New("transient db failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := createGinTestServer(t, t.TempDir())
			sessionCookie, csrfToken := setupTestAdminSession(t, srv)
			if _, _, err := srv.cryptoManager.GenerateRecoveryKey(false); err != nil {
				t.Fatalf("GenerateRecoveryKey: %v", err)
			}
			srv.authRepo = &fakeAuthRepo{loadErr: tc.err}

			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPost, "/api/v1/crypto/recovery-key/generate",
				strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			attachAuth(req, sessionCookie, csrfToken)
			srv.router.ServeHTTP(w, req)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503 fail-closed on unknown ack state; got %d body=%s", w.Code, w.Body.String())
			}
			var resp map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if resp["error"] != ErrAckStateUnknown {
				t.Fatalf("expected error=%s; got %v", ErrAckStateUnknown, resp["error"])
			}
		})
	}
}

// TestRecoveryKeyAck_REC4_KeyIDBinding pins the REC-4 contract: /ack must
// supply a key_id that matches the current SDEKRK fingerprint, otherwise
// the server refuses (400 if missing, 409 stale_recovery_key_id if mismatch).
// Closes the multi-tab rotation race + the silent-ack-failure resurrection
// class structurally — a stale tab acking with the OLD key_id can no longer
// suppress the boot prompt the operator needs to see for the NEW key.
func TestRecoveryKeyAck_REC4_KeyIDBinding(t *testing.T) {
	post := func(t *testing.T, srv *GinServer, sessionCookie *http.Cookie, csrfToken, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		var req *http.Request
		if body == "" {
			req, _ = http.NewRequest(http.MethodPost, "/api/v1/crypto/recovery-key/ack", nil)
		} else {
			req, _ = http.NewRequest(http.MethodPost, "/api/v1/crypto/recovery-key/ack", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		}
		attachAuth(req, sessionCookie, csrfToken)
		srv.router.ServeHTTP(w, req)
		return w
	}

	t.Run("missing_key_id_returns_400", func(t *testing.T) {
		srv := createGinTestServer(t, t.TempDir())
		sessionCookie, csrfToken := setupTestAdminSession(t, srv)
		if _, _, err := srv.cryptoManager.GenerateRecoveryKey(false); err != nil {
			t.Fatalf("GenerateRecoveryKey: %v", err)
		}
		srv.authRepo = &fakeAuthRepo{}

		w := post(t, srv, sessionCookie, csrfToken, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("missing key_id: want 400, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("stale_key_id_returns_409", func(t *testing.T) {
		srv := createGinTestServer(t, t.TempDir())
		sessionCookie, csrfToken := setupTestAdminSession(t, srv)
		if _, _, err := srv.cryptoManager.GenerateRecoveryKey(false); err != nil {
			t.Fatalf("GenerateRecoveryKey: %v", err)
		}
		srv.authRepo = &fakeAuthRepo{}
		// Capture the version Tab A would have received.
		staleID := srv.cryptoManager.RecoveryKeyID()
		// Tab B rotates; current SDEKRK changes, so RecoveryKeyID() does too.
		if _, _, err := srv.cryptoManager.GenerateRecoveryKey(true); err != nil {
			t.Fatalf("rotation: %v", err)
		}
		if srv.cryptoManager.RecoveryKeyID() == staleID {
			t.Fatalf("rotation did not change RecoveryKeyID — REC-4 fingerprint not capturing rotation")
		}
		// Tab A acks with the old id — must be rejected with 409, NOT silently
		// stamp RecoveryAckAt over Tab B's words.
		w := post(t, srv, sessionCookie, csrfToken, fmt.Sprintf(`{"key_id":%q}`, staleID))
		if w.Code != http.StatusConflict {
			t.Fatalf("stale key_id: want 409, got %d body=%s", w.Code, w.Body.String())
		}
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if body["error"] != "stale_recovery_key_id" {
			t.Fatalf("stale key_id: expected error=stale_recovery_key_id, got %v", body)
		}
	})

	t.Run("matching_key_id_succeeds", func(t *testing.T) {
		srv := createGinTestServer(t, t.TempDir())
		sessionCookie, csrfToken := setupTestAdminSession(t, srv)
		if _, _, err := srv.cryptoManager.GenerateRecoveryKey(false); err != nil {
			t.Fatalf("GenerateRecoveryKey: %v", err)
		}
		repo := &fakeAuthRepo{}
		srv.authRepo = repo
		keyID := srv.cryptoManager.RecoveryKeyID()
		w := post(t, srv, sessionCookie, csrfToken, fmt.Sprintf(`{"key_id":%q}`, keyID))
		if w.Code != http.StatusOK {
			t.Fatalf("matching key_id: want 200, got %d body=%s", w.Code, w.Body.String())
		}
		if repo.staleness.RecoveryAckAt.IsZero() {
			t.Fatalf("matching key_id: ack did not persist RecoveryAckAt")
		}
	})

	t.Run("no_key_on_disk_returns_409", func(t *testing.T) {
		srv := createGinTestServer(t, t.TempDir())
		sessionCookie, csrfToken := setupTestAdminSession(t, srv)
		// No GenerateRecoveryKey call — RecoveryKeyID() is empty.
		srv.authRepo = &fakeAuthRepo{}
		w := post(t, srv, sessionCookie, csrfToken, `{"key_id":"deadbeef00000000"}`)
		if w.Code != http.StatusConflict {
			t.Fatalf("no key on disk: want 409 stale_recovery_key_id, got %d body=%s", w.Code, w.Body.String())
		}
	})
}

// TestRecoveryKeyAck_DeniedDuringForcedPasskeyRegistration guards the C7
// belt-and-suspenders contract: state-mutating handlers under the
// /crypto/recovery-key/ prefix must reject sessions with MustRegisterPasskey
// set. Without this, a stolen-admin-password attacker could ack the recovery
// key during the bootstrap window, suppressing the boot prompt that the
// legitimate operator needs to actually see/save their recovery words.
func TestRecoveryKeyAck_DeniedDuringForcedPasskeyRegistration(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)
	sess, ok := srv.sessions.Get(sessionCookie.Value)
	if !ok {
		t.Fatalf("session not found")
	}
	sess.MustRegisterPasskey.Store(true)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/crypto/recovery-key/ack", nil)
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 during MustRegisterPasskey, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestBoot_DuringUnlockChain_DoesNotRouteToSetup is the regression test for
// the bug that motivated the lifecycle package. Pre-fix: a /system/boot poll
// arriving in the multi-second window between cryptoManager.Unlock returning
// (SDEK loaded → IsLocked() == false) and notifyPersistenceLockState
// completing (persistence still locked → ErrLocked from any user query)
// would see step 7b's isSetupComplete return (false, nil) and route an
// already-provisioned device to the first-run setup wizard. Reproduced
// in the field on a Raspberry Pi after a remote unlock.
//
// Post-fix: handleBoot routes on lifecycle.State() (StateUnlocking) instead
// of cryptoManager.IsLocked() + isSetupComplete derivation. screen must be
// "unlock" with lifecycle="unlocking", never "setup".
func TestBoot_DuringUnlockChain_DoesNotRouteToSetup(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	setupTestAdminSession(t, srv) // initializes crypto + admin, lifecycle = Ready

	// Simulate the race window: SDEK is loaded (cryptoManager not locked)
	// but the unlock chain is still running (lifecycle = Unlocking). In
	// production this state is held for the duration of completeUnlockChain;
	// here we drive it directly to keep the test deterministic.
	if err := srv.lifecycle.Lock(); err != nil {
		t.Fatalf("lifecycle.Lock: %v", err)
	}
	if err := srv.lifecycle.BeginUnlock(); err != nil {
		t.Fatalf("lifecycle.BeginUnlock: %v", err)
	}
	if got := srv.lifecycle.State(); got != lifecycle.StateUnlocking {
		t.Fatalf("setup precondition: state = %s, want %s", got, lifecycle.StateUnlocking)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["screen"] == "setup" {
		t.Fatalf("REGRESSION: boot routed to setup during unlock chain — the precise bug the lifecycle package is built to prevent. body=%s", w.Body.String())
	}
	if resp["screen"] != "unlock" {
		t.Fatalf("expected screen=unlock during Unlocking, got %v", resp["screen"])
	}
	if resp["lifecycle"] != "unlocking" {
		t.Fatalf("expected lifecycle=unlocking, got %v", resp["lifecycle"])
	}
}

// TestBoot_AutoUnlockInFlight_SurfacesUnlockingViaLifecycle covers the
// wireToken bridge: when state is still Locked (BeginUnlock fires inside
// completeUnlockChain, AFTER UnwrapSDEKWithEscrow) but autounlockOrch.InFlight()
// is true (pickup mid-namek), the wire format must report lifecycle=unlocking
// so the UI shows the spinner. Without this bridge the UI would show the
// password prompt during the early-pickup window.
func TestBoot_AutoUnlockInFlight_SurfacesUnlockingViaLifecycle(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	setupTestAdminSession(t, srv)
	if err := srv.lifecycle.Lock(); err != nil {
		t.Fatalf("lifecycle.Lock: %v", err)
	}

	// No autounlockOrch is wired into the test fixture, so we can't directly
	// test the InFlight branch end-to-end here without injecting a stub.
	// Instead exercise the wireLifecycleToken normalizer directly — its
	// behavior is the integration-relevant property.
	if got := wireLifecycleToken(lifecycle.StateLocked, true); got != "unlocking" {
		t.Errorf("wireLifecycleToken(Locked, autoInFlight=true) = %q, want %q", got, "unlocking")
	}
	if got := wireLifecycleToken(lifecycle.StateLocked, false); got != "locked" {
		t.Errorf("wireLifecycleToken(Locked, autoInFlight=false) = %q, want %q", got, "locked")
	}
	if got := wireLifecycleToken(lifecycle.StateUnlocking, false); got != "unlocking" {
		t.Errorf("wireLifecycleToken(Unlocking, autoInFlight=false) = %q, want %q", got, "unlocking")
	}
	// Failed normalizes to "locked" per the no-failure-callout UX feedback.
	if got := wireLifecycleToken(lifecycle.StateFailed, false); got != "locked" {
		t.Errorf("wireLifecycleToken(Failed, autoInFlight=false) = %q, want %q", got, "locked")
	}
}

// TestBoot_FailedAndUnprovisioned_RoutesToSetup covers a partial-setup
// failure scenario: setup ran step 4 (BeginUnlock) but failed before
// MarkProvisioned. lifecycle=Failed; provisioning marker absent. The boot
// handler must route to the setup wizard, not the unlock prompt — the user
// has no unlock password to enter.
func TestBoot_FailedAndUnprovisioned_RoutesToSetup(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	// Explicitly DO NOT call setupTestAdminSession. Drive lifecycle into
	// Failed via the legal path (Locked → Unlocking → Failed) without ever
	// touching the provisioning marker.
	if err := srv.lifecycle.BeginUnlock(); err != nil {
		t.Fatalf("BeginUnlock: %v", err)
	}
	if err := srv.lifecycle.MarkFailed(errors.New("simulated mid-setup failure")); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if got := srv.lifecycle.State(); got != lifecycle.StateFailed {
		t.Fatalf("precondition: state = %s, want %s", got, lifecycle.StateFailed)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["screen"] != "setup" {
		t.Fatalf("Failed + !provisioned must route to setup (user has no unlock password); got screen=%v", resp["screen"])
	}
}

// TestBoot_RecoveryPendingNotPendingOnErrLockedRace guards against a TOCTOU
// where /crypto/lock fires between the IsLocked() check and Staleness() call.
// The staleness read returns ErrLocked; the gate must treat that identically
// to IsLocked()=true (file-presence only) rather than fail-closing to
// pending=true, which would re-introduce the B1 rotation hazard via the race.
func TestBoot_RecoveryPendingNotPendingOnErrLockedRace(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, _ := setupTestAdminSession(t, srv)
	if _, _, err := srv.cryptoManager.GenerateRecoveryKey(false); err != nil {
		t.Fatalf("GenerateRecoveryKey: %v", err)
	}
	// Crypto stays unlocked at the IsLocked() check, but the staleness read
	// errors with ErrLocked — simulating a concurrent /crypto/lock that
	// landed between the two probes.
	srv.authRepo = &fakeAuthRepo{loadErr: persistence.ErrLocked}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	req.AddCookie(sessionCookie)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if v, present := resp["recovery_key_pending"]; present && v == true {
		t.Fatalf("ErrLocked race must not flip recovery_key_pending=true (would invalidate saved key on unlock); got %v", v)
	}
}
