package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestOIDCAuthorize_BlockedUnderMustRegisterPasskey mirrors plan test #22 for
// the OIDC authorize endpoint. D-10's forcing-gate must prevent a
// MustRegisterPasskey=true session from minting OIDC tokens via the authorize
// fast-path — otherwise an attacker with the victim's remote password can
// bypass the gate in two requests to any oidc_passthrough/headers app.
//
// Differential: with the gate ON we expect an explicit 302 to "/"
// (bootscreen); with the gate OFF we expect any other outcome (the fast-
// path won't succeed in this harness due to test-only OIDC-infra gaps, but
// the refusal redirect specifically must not fire).
func TestOIDCAuthorize_BlockedUnderMustRegisterPasskey(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, _ := setupTestAdminSession(t, srv)
	sess, ok := srv.sessions.Get(sessionCookie.Value)
	if !ok {
		t.Fatalf("session not found")
	}

	do := func() (status int, location string) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet,
			"/oauth/authorize?client_id=piccolo-anyapp-proxy&response_type=code&scope=openid&redirect_uri=https://example.invalid/cb",
			nil)
		req.AddCookie(sessionCookie)
		srv.router.ServeHTTP(w, req)
		return w.Code, w.Header().Get("Location")
	}

	// Gate ON: explicit 302 to the portal root (resolved via
	// portalOriginForRequest; may be absolute or "/" when portal origin is
	// unresolvable). The critical assertion is 302 + path ending in "/"
	// (portal root), never a URL carrying an OIDC code=.
	sess.MustRegisterPasskey.Store(true)
	status, loc := do()
	if status != http.StatusFound {
		t.Fatalf("gate=true: want 302, got status=%d loc=%q", status, loc)
	}
	if !strings.HasSuffix(loc, "/") || strings.Contains(loc, "code=") {
		t.Fatalf("gate=true: expected portal-root redirect, got loc=%q", loc)
	}

	// Gate OFF: the handler must not produce the same portal-root refusal,
	// and must never mint an OIDC code (belt-and-suspenders — symmetric
	// with the gate-ON safety assertion above).
	sess.MustRegisterPasskey.Store(false)
	gatedLoc := loc
	status, loc = do()
	if status == http.StatusFound && loc == gatedLoc {
		t.Fatalf("gate=false: refusal redirect fired incorrectly (status=%d loc=%q)", status, loc)
	}
	if strings.Contains(loc, "code=") {
		t.Fatalf("gate=false: unexpected OIDC code in Location (test harness should not reach full-authorize): %q", loc)
	}
}

// Test #22 (plan): under MustRegisterPasskey=true, POST /crypto/recovery-key/generate
// must return 403 with passkey_registration_required (C7 / RF-4).
func TestRecoveryGenerate_BlockedUnderMustRegisterPasskey(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrf := setupTestAdminSession(t, srv)
	sess, ok := srv.sessions.Get(sessionCookie.Value)
	if !ok {
		t.Fatalf("session not found")
	}
	sess.MustRegisterPasskey.Store(true)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost,
		"/api/v1/crypto/recovery-key/generate", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(sessionCookie)
	srv.router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "passkey_registration_required" {
		t.Fatalf("expected error=passkey_registration_required, got %v", body)
	}
}

// TestLoginOptions_NoPasskeyAnywhere_WhenEmpty asserts the fresh-server shape:
// no_passkey_anywhere=true and passkey is suppressed from the methods list.
func TestLoginOptions_NoPasskeyAnywhere_WhenEmpty(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	setupTestAdminSession(t, srv) // initialize crypto+admin, no creds

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet,
		"/api/v1/auth/login-options", nil)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, ok := body["no_passkey_anywhere"].(bool); !ok || !v {
		t.Fatalf("no_passkey_anywhere: want true, got %v (ok=%v)", body["no_passkey_anywhere"], ok)
	}
	methods, _ := body["methods"].([]any)
	for _, m := range methods {
		if m == "passkey" {
			t.Fatalf("expected no passkey in methods when no_passkey_anywhere=true, got %v", methods)
		}
	}
}

// TestLoginRateLimit_PerUsername_LockoutAfter10Failures is plan test #9.
func TestLoginRateLimit_PerUsername_LockoutAfter10Failures(t *testing.T) {
	rl := newLoginRateLimiter(10, 15*time.Minute, 100)
	const u = "alice"
	for i := 0; i < 9; i++ {
		allowed, release := rl.Allow(u)
		if !allowed {
			t.Fatalf("expected Allow=true at i=%d", i)
		}
		rl.RecordFailure(u)
		// Release the inflight reservation per the Allow→release
		// contract; RecordFailure owns failures only, not inflight.
		release()
	}
	// 10th failure crosses threshold.
	rl.RecordFailure(u)
	if allowed, _ := rl.Allow(u); allowed {
		t.Fatalf("expected lockout after 10 failures")
	}
}

// TestLoginRateLimit_LockedBucket_SurvivesDistinctUsernameChurn guards the P1
// fix in evictLocked: a locked victim bucket must not be evicted by a flood
// of distinct attacker-chosen usernames that would otherwise push it off the
// LRU tail.
func TestLoginRateLimit_LockedBucket_SurvivesDistinctUsernameChurn(t *testing.T) {
	const cap = 50
	rl := newLoginRateLimiter(3, 15*time.Minute, cap)
	// Lock the victim.
	for i := 0; i < 3; i++ {
		rl.RecordFailure("victim")
	}
	if allowed, _ := rl.Allow("victim"); allowed {
		t.Fatalf("precondition: victim should be locked")
	}
	// Churn cap*3 distinct attacker-chosen usernames. Each is a fresh bucket
	// pushed to the front of the LRU; cumulatively they would evict the tail
	// many times over.
	for i := 0; i < cap*3; i++ {
		rl.RecordFailure("churn_" + strings.Repeat("x", i%8) + "_" + time.Now().Format("150405.000000"))
	}
	if allowed, _ := rl.Allow("victim"); allowed {
		t.Fatalf("victim lockout was reset by distinct-username churn — LRU eviction bypassed the pin")
	}
}

// TestLoginRateLimit_PartialBucket_SurvivesDistinctUsernameChurn guards the
// Codex P2 fix (2026-04-20): an attacker with maxFails-1 guesses in flight
// for a victim must not be able to flood distinct usernames to evict the
// partial bucket and restart the failure counter fresh.
func TestLoginRateLimit_PartialBucket_SurvivesDistinctUsernameChurn(t *testing.T) {
	const cap = 50
	const maxFails = 10
	rl := newLoginRateLimiter(maxFails, 15*time.Minute, cap)
	// Victim bucket at maxFails-1 — one more failure would trigger lockout.
	for i := 0; i < maxFails-1; i++ {
		rl.RecordFailure("victim")
	}
	if allowed, _ := rl.Allow("victim"); !allowed {
		t.Fatalf("precondition: victim should still be allowed at %d failures", maxFails-1)
	}
	// Churn distinct attacker-chosen usernames to try to evict the victim.
	for i := 0; i < cap*3; i++ {
		rl.RecordFailure(fmt.Sprintf("churn_%d", i))
	}
	// One more victim failure should trip the lockout — if the churn evicted
	// the partial bucket, the counter would be fresh and this single failure
	// would NOT lock (10 more would be needed).
	rl.RecordFailure("victim")
	if allowed, _ := rl.Allow("victim"); allowed {
		t.Fatalf("partial bucket was evicted by distinct-username churn — failure counter reset")
	}
}

// TestLoginRateLimit_SuccessOnDifferentUser_DoesNotResetLockout is plan #10.
func TestLoginRateLimit_SuccessOnDifferentUser_DoesNotResetLockout(t *testing.T) {
	rl := newLoginRateLimiter(3, 15*time.Minute, 100)
	for i := 0; i < 3; i++ {
		rl.RecordFailure("victim")
	}
	if allowed, _ := rl.Allow("victim"); allowed {
		t.Fatalf("victim should be locked")
	}
	rl.RecordSuccess("attacker")
	if allowed, _ := rl.Allow("victim"); allowed {
		t.Fatalf("victim should remain locked after attacker success")
	}
	// A success for victim itself should clear the lock.
	rl.RecordSuccess("victim")
	if allowed, _ := rl.Allow("victim"); !allowed {
		t.Fatalf("victim should be unlocked after own success")
	}
}

// TestLoginRateLimit_PartialBucket_AgesOutAfterLockoutWindow guards the
// Codex P2 fix (2026-04-20 round 2): 9 stale failures followed by one fresh
// failure beyond the lockout window must NOT immediately lock the account.
// Legitimate users who mistype twice over months should not accumulate.
func TestLoginRateLimit_PartialBucket_AgesOutAfterLockoutWindow(t *testing.T) {
	rl := newLoginRateLimiter(10, 50*time.Millisecond, 100)
	for i := 0; i < 9; i++ {
		rl.RecordFailure("alice")
	}
	// Wait for the lockout window to elapse.
	time.Sleep(60 * time.Millisecond)
	// A single fresh failure should restart the counter at 1, not jump to 10.
	rl.RecordFailure("alice")
	if allowed, _ := rl.Allow("alice"); !allowed {
		t.Fatalf("stale partial bucket was not aged out: single fresh failure after window locked the account")
	}
}

// TestLoginRateLimit_HardCap_EvictsExpiredBeforeDropping guards the Codex
// P2 fix (2026-04-20 round 2): when the map is at 2×cap with expired pinned
// buckets, RecordFailure for a new username must reclaim the expired slot
// before falling back to the hard-cap drop.
func TestLoginRateLimit_HardCap_EvictsExpiredBeforeDropping(t *testing.T) {
	const cap = 10
	rl := newLoginRateLimiter(3, 40*time.Millisecond, cap)
	// Fill to 2×cap with partial-failure buckets (all pinned).
	for i := 0; i < 2*cap; i++ {
		rl.RecordFailure(fmt.Sprintf("churn_%d", i))
	}
	// Wait past the pin window so all partials are now expired.
	time.Sleep(50 * time.Millisecond)
	// New user's failure must be recorded, not dropped.
	rl.RecordFailure("new_user")
	for i := 0; i < 2; i++ {
		rl.RecordFailure("new_user")
	}
	if allowed, _ := rl.Allow("new_user"); allowed {
		t.Fatalf("new_user lockout did not engage — failures were silently dropped")
	}
}
