package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"piccolod/internal/auth"
	"piccolod/internal/cryptoutil"
	"piccolod/internal/health"
	"piccolod/internal/persistence"
)

// Structured error codes returned in JSON responses. Matched by frontend.
const (
	errorCodeSetupInProgress     = "setup_in_progress"
	errorCodeSetupComplete       = "setup_complete"
	errorCodeStorageInitFailed   = "storage_init_failed"
	errorCodeStorageUnlockFailed = "storage_unlock_failed"
	errorCodeStorageEmergency    = "storage_emergency"

	// ErrRecoveryKeyAlreadyAcked: /recovery-key/generate refused to rotate
	// because the existing key is acknowledged. Caller must pass
	// force_rotate=true if rotation is genuinely intended.
	ErrRecoveryKeyAlreadyAcked = "recovery_key_already_acked"
	// ErrAckStateUnknown: /recovery-key/generate could not read the ack
	// state to verify rotation safety. Treated as transient by callers.
	ErrAckStateUnknown = "ack_state_unknown"
)

// stripNumberedPrefixes removes tokens like "1.", "2", "24." that appear
// when a user copies the recovery key from the numbered on-screen grid.
// BIP-39 words are always alphabetic, so any token that is purely digits
// (with an optional trailing dot) is safe to discard.
func stripNumberedPrefixes(tokens []string) []string {
	filtered := make([]string, 0, len(tokens))
	for _, t := range tokens {
		s := strings.TrimRight(t, ".")
		if s == "" {
			continue
		}
		isNumeric := true
		for _, c := range s {
			if c < '0' || c > '9' {
				isNumeric = false
				break
			}
		}
		if isNumeric {
			continue
		}
		filtered = append(filtered, t)
	}
	return filtered
}

// isSetupComplete checks whether first-run provisioning has finished by
// counting users (primary) or checking auth initialization (fallback).
// Returns (false, nil) when the store is locked (ErrLocked).
// Returns (false, err) on transient errors — callers should fail closed.
func (s *GinServer) isSetupComplete(ctx context.Context) (bool, error) {
	if s.userManager != nil {
		count, err := s.userManager.Count(ctx)
		if err == nil && count > 0 {
			return true, nil
		}
		if err != nil && !errors.Is(err, persistence.ErrLocked) {
			return false, err
		}
		return false, nil
	}
	if s.authManager != nil {
		initialized, err := s.authManager.IsInitialized(ctx)
		if err == nil && initialized {
			return true, nil
		}
		if err != nil && !errors.Is(err, persistence.ErrLocked) {
			return false, err
		}
	}
	return false, nil
}

func (s *GinServer) notifyPersistenceLockState(ctx context.Context, locked bool) error {
	if s == nil || s.dispatcher == nil {
		return errors.New("persistence dispatcher unavailable")
	}
	_, err := s.dispatcher.Dispatch(ctx, persistence.RecordLockStateCommand{Locked: locked})
	return err
}

// isSetupInProgress returns true if the setup mutex is currently held.
// Point-in-time probe: inherently racy (TOCTOU) but acceptable for
// polling-based UX — a stale value self-corrects on the next 3s poll.
func (s *GinServer) isSetupInProgress() bool {
	if !s.setupMu.TryLock() {
		return true
	}
	s.setupMu.Unlock()
	return false
}

// handleCryptoStatus: GET /api/v1/crypto/status
func (s *GinServer) handleCryptoStatus(c *gin.Context) {
	init := s.cryptoManager != nil && s.cryptoManager.IsInitialized()
	locked := false
	if init {
		locked = s.cryptoManager.IsLocked()
	}
	c.JSON(http.StatusOK, gin.H{"initialized": init, "locked": locked, "setup_in_progress": s.isSetupInProgress()})
}

// handleCryptoSetup: POST /api/v1/crypto/setup { password }
// This is the single atomic initialization point for Piccolo.
// It sets up: crypto (disk encryption), auth manager, and admin user.
// Idempotent: each step checks whether it has already been completed, so a
// retry after a partial failure (e.g. client disconnect) picks up where it
// left off instead of failing on already-done steps.
func (s *GinServer) handleCryptoSetup(c *gin.Context) {
	// Serialize setup requests to prevent concurrent LUKS initialization.
	if !s.setupMu.TryLock() {
		c.JSON(http.StatusConflict, gin.H{"error": "setup already in progress", "code": errorCodeSetupInProgress})
		return
	}
	defer s.setupMu.Unlock()

	var body struct {
		Password   string `json:"password"`
		SetupNonce string `json:"setup_nonce"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	// 0. For remote requests, verify the setup nonce (CGNAT defense).
	// The nonce was generated during hostname claim on the LAN and proves the
	// remote user initiated setup from the LAN (physical access trust zone).
	if !isLANRequest(c) {
		if !s.sessions.ValidateSetupNonce(body.SetupNonce) {
			c.JSON(http.StatusForbidden, gin.H{"error": "invalid setup nonce"})
			return
		}
	}

	// 1. Validate password policy FIRST (before any state changes)
	if err := auth.ValidatePasswordStrength(body.Password); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 2. [Gate removed] Setup is fully idempotent — every step has a "skip if
	// already done" guard. When crypto is initialized+locked (reboot after
	// partial setup failure), step 4 unlocks with the provided password and
	// the remaining steps complete provisioning.

	// 3. Setup crypto manager (disk encryption key) — skip if already initialized.
	if !s.cryptoManager.IsInitialized() {
		if err := s.cryptoManager.Setup(body.Password); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	} else {
		log.Printf("INFO: crypto already initialized, skipping Setup")
	}

	// 4. Unlock crypto — skip if already unlocked.
	// Unlock is idempotent when already unlocked (re-derives SDEK),
	// and leaves state unchanged on failure — no Lock() side effect.
	if s.cryptoManager.IsLocked() {
		if err := s.cryptoManager.Unlock(body.Password); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "wrong password"})
			return
		}
	} else if s.cryptoManager.IsInitialized() {
		// Partial retry on already-unlocked device: verify password matches.
		if err := s.cryptoManager.Unlock(body.Password); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "wrong password"})
			return
		}
		log.Printf("INFO: crypto re-verified, continuing partial setup")
	}

	// Decouple from HTTP request context — setup must survive connection drops.
	setupCtx, setupCancel := context.WithTimeout(s.serverContext(), 10*time.Minute)
	defer setupCancel()

	// Log if the client disconnects while setup is still running.
	reqCtx := c.Request.Context()
	handlerDone := make(chan struct{})
	defer close(handlerDone)
	go func() {
		select {
		case <-reqCtx.Done():
			log.Printf("INFO: crypto/setup client disconnected; setup continuing in background")
		case <-handlerDone:
		}
	}()

	// Release KDF memory before LUKS initialization — the Argon2id key
	// derivation above may have allocated hundreds of MiBs that Go's GC
	// hasn't returned to the OS yet. cryptsetup spawns its own Argon2id,
	// and the combined footprint can OOM a 2 GB device.
	log.Printf("INFO: releasing KDF memory before storage initialization")
	runtime.GC()
	debug.FreeOSMemory()

	// 5. Notify persistence (mounts control-plane LUKS volume, enables SQLite).
	// Runs BEFORE data volume so the setupComplete guard (step 5b) can query
	// SQLite and reject fully provisioned devices before any destructive ops.
	if err := s.notifyPersistenceLockState(setupCtx, false); err != nil {
		log.Printf("WARN: failed to propagate unlock state: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update persistence state"})
		return
	}

	// 5b. Guard: reject if setup is already complete to prevent /setup from
	// bypassing the two-door login model on a fully configured device.
	//
	// On error: re-lock persistence (no session exists, safe to undo step 5).
	// On complete (409): do NOT re-lock — a concurrent remote setup may have an
	// active session that depends on persistence staying unlocked (RFC 20260328).
	if complete, err := s.isSetupComplete(setupCtx); err != nil {
		log.Printf("ERROR: setup-complete guard failed: %v", err)
		_ = s.notifyPersistenceLockState(setupCtx, true)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "setup state check failed"})
		return
	} else if complete {
		c.JSON(http.StatusConflict, gin.H{"error": "setup already complete", "code": errorCodeSetupComplete})
		return
	}

	// 5c. Mark provisioning BEFORE any irreversible state writes (data volume,
	// admin user, session). Strict here: if the marker write fails, fail the
	// whole request so the client can retry — at this point no irreversible
	// state has been committed and step 5b will let a retry through. After
	// admin-user creation in step 8, the same step 5b guard would reject
	// retries (isSetupComplete returns true), so a write failure later in
	// the flow would have no in-flow recovery path. Doing the write here
	// closes that gap.
	if err := s.provisioningState.MarkProvisioned(); err != nil {
		log.Printf("ERROR: mark provisioned: %v", err)
		_ = s.notifyPersistenceLockState(setupCtx, true)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record setup completion: " + err.Error()})
		return
	}

	// 6. Activate/initialize data volume. Uses UnlockDataVolume (idempotent):
	// checks VGExists first, falls back to InitializeDataVolume only if no VG.
	// Safe for both first-run and retry.
	var luksErr error
	if s.storageMgr != nil {
		if err := s.storageMgr.UnlockDataVolume(setupCtx); err != nil {
			log.Printf("ERROR: data volume setup failed: %v", err)
			luksErr = err
			if s.healthTracker != nil {
				s.healthTracker.Setf("storage", health.LevelError, "data volume setup failed")
			}
		}
	}

	// 6b. Provision LUKS keyslot 1 (admin password) on all v3 volumes.
	if luksErr == nil {
		if kp, ok := s.persistence.(persistence.KeyslotProvisioner); ok {
			passBytes := []byte(body.Password)
			if err := kp.ProvisionLUKSKeyslot(setupCtx, 1, passBytes); err != nil {
				log.Printf("WARN: LUKS keyslot 1 provisioning during setup: %v", err)
			}
			cryptoutil.SecureZero(passBytes)
		}
	}

	// 7. Setup auth manager (mandatory — needs SQLite from step 5) — skip if already initialized.
	if s.authManager != nil {
		initialized, err := s.authManager.IsInitialized(setupCtx)
		if err != nil {
			log.Printf("ERROR: auth init check failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "auth initialization check failed"})
			return
		}
		if !initialized {
			if err := s.authManager.Setup(setupCtx, body.Password); err != nil {
				log.Printf("ERROR: auth manager setup failed: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to setup auth: " + err.Error()})
				return
			}
		} else {
			log.Printf("INFO: auth manager already initialized, skipping Setup")
		}
	}

	// 8. Create admin user in SQLite — skip if admin already exists.
	if s.userManager != nil {
		_, err := s.userManager.GetByUsername(setupCtx, "admin")
		if err != nil && !errors.Is(err, auth.ErrUserNotFound) {
			log.Printf("ERROR: admin lookup failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "admin user check failed"})
			return
		}
		if errors.Is(err, auth.ErrUserNotFound) {
			adminInput := auth.CreateUserInput{
				Username: "admin",
				Email:    "admin@piccolo.local",
				Password: body.Password,
				Role:     persistence.UserRoleAdmin,
			}
			if _, err := s.userManager.Create(setupCtx, adminInput); err != nil {
				log.Printf("ERROR: admin user creation failed: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create admin user: " + err.Error()})
				return
			}
		} else {
			log.Printf("INFO: admin user already exists, skipping creation")
		}
	}

	// 9. Activate PCV publisher (depends on control-plane mount, safe even on data LUKS failure).
	if s.pcvPublisher != nil {
		s.pcvPublisher.Activate()
	}

	// 10. Create session for the admin user
	userID := ""
	if s.userManager != nil {
		if u, err := s.userManager.GetByUsername(setupCtx, "admin"); err == nil {
			userID = u.ID
		}
	}
	// RFC 20260122 §6.2: Create portal session with origin binding
	boundOrigin := s.computeCanonicalOrigin(c)
	sess := s.sessions.CreatePortalSession(userID, "admin", "admin", boundOrigin, portalSessionTTL)

	// RFC 20260328: When setup happens on the remote domain (via IP match + nonce),
	// require passkey registration before accessing the dashboard. This ensures
	// the boot handler enforces the passkey step even after page refresh.
	if s.isRemoteSecureRequest(c.Request) && s.webauthnMgr != nil {
		s.sessions.SetMustRegisterPasskey(sess.ID)
	}

	s.setSessionCookie(c, sess.ID, portalSessionCookieTTL)

	// Consume the setup nonce now that setup has succeeded.
	s.sessions.ConsumeSetupNonce()

	// Wake the setup-heartbeat loop so it sends its terminal heartbeat
	// immediately rather than waiting up to one 30s tick — the device
	// should disappear from picolospace.com/setup as soon as the user
	// finishes the wizard. (MarkProvisioned ran earlier, in step 5c.)
	if s.identityService != nil {
		s.identityService.NotifySetupComplete()
	}

	// Fail after session creation so the user has portal access for recovery.
	if luksErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "data volume initialization failed: " + luksErr.Error(),
			"code":  errorCodeStorageInitFailed,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "ok"})
}

// handleCryptoUnlock: POST /api/v1/crypto/unlock { password }
func (s *GinServer) handleCryptoUnlock(c *gin.Context) {
	var body struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if !s.cryptoManager.IsInitialized() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not initialized"})
		return
	}
	password := strings.TrimSpace(body.Password)
	if password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password required"})
		return
	}
	if err := s.cryptoManager.Unlock(password); err != nil {
		if s.recordLoginFailure() {
			c.Header("Retry-After", "5")
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too Many Requests"})
		} else {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		}
		return
	}
	s.resetLoginFailures()

	// Decouple from HTTP request context — unlock must survive connection drops.
	unlockCtx, unlockCancel := context.WithTimeout(s.serverContext(), 10*time.Minute)
	defer unlockCancel()

	reqCtx := c.Request.Context()
	handlerDone := make(chan struct{})
	defer close(handlerDone)
	go func() {
		select {
		case <-reqCtx.Done():
			log.Printf("INFO: crypto/unlock client disconnected; unlock continuing in background")
		case <-handlerDone:
		}
	}()

	// Release KDF memory before LUKS unlock — same rationale as setup handler.
	log.Printf("INFO: releasing KDF memory before storage unlock")
	runtime.GC()
	debug.FreeOSMemory()

	// Unlock data volume BEFORE notifying persistence, so that
	// storage volumes are available before the app-manager reconcile loop starts.
	var luksErr error
	if s.storageMgr != nil {
		if err := s.storageMgr.UnlockDataVolume(unlockCtx); err != nil {
			log.Printf("ERROR: data volume unlock failed: %v", err)
			luksErr = err
			if s.healthTracker != nil {
				s.healthTracker.Setf("storage", health.LevelError, "data volume unlock failed")
			}
		}
	}
	if err := s.notifyPersistenceLockState(unlockCtx, false); err != nil {
		log.Printf("WARN: failed to propagate unlock state: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update persistence state"})
		return
	}
	// Activate PCV publisher (depends on control-plane mount, safe even on data LUKS failure).
	if s.pcvPublisher != nil {
		s.pcvPublisher.Activate()
	}
	// Two-door model: unlock is a disk operation only — no session creation.
	// Report setup_complete so the UI can route to /crypto/setup (partial
	// recovery) or chain /auth/login (normal reboot).
	setupComplete, err := s.isSetupComplete(unlockCtx)
	if err != nil {
		log.Printf("WARN: setup-complete check failed, assuming complete: %v", err)
		setupComplete = true // Fail closed: assume provisioned, route to login
	}
	// Reconcile the durable provisioning marker from the authoritative
	// post-unlock answer. Closes the gap left by handleCryptoSetup's
	// best-effort MarkProvisioned write.
	if err == nil {
		if rerr := s.provisioningState.ReconcileFromPersistence(setupComplete); rerr != nil {
			log.Printf("WARN: reconcile provisioning: %v", rerr)
		}
	}
	resp := gin.H{"message": "ok", "setup_complete": setupComplete}
	if luksErr != nil {
		resp["warning"] = "data volume unlock failed: " + luksErr.Error()
		resp["warning_code"] = errorCodeStorageUnlockFailed
	}
	c.JSON(http.StatusOK, resp)
}

// handleCryptoResetPassword: POST /api/v1/crypto/reset-password
func (s *GinServer) handleCryptoResetPassword(c *gin.Context) {
	var body struct {
		RecoveryKey string `json:"recovery_key"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	recoveryKey := strings.TrimSpace(body.RecoveryKey)
	newPassword := body.NewPassword
	if recoveryKey == "" || newPassword == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "recovery_key and new_password required"})
		return
	}
	if s.cryptoManager == nil || !s.cryptoManager.IsInitialized() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not initialized"})
		return
	}
	if s.authManager == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "auth unavailable"})
		return
	}

	ctx, cancel := s.opContext(c, 2*time.Minute)
	defer cancel()
	words := stripNumberedPrefixes(strings.Fields(recoveryKey))
	wasLocked := s.cryptoManager.IsLocked()
	needRelock := wasLocked

	if err := s.cryptoManager.UnlockWithRecoveryKey(words); err != nil {
		if s.recordResetFailure() {
			c.Header("Retry-After", "5")
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too Many Requests"})
		} else {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		}
		return
	}
	s.resetResetFailures()

	if wasLocked {
		if err := s.notifyPersistenceLockState(ctx, false); err != nil {
			log.Printf("WARN: reset-password unlock notify failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to unlock persistence"})
			return
		}
		defer func() {
			if needRelock {
				s.cryptoManager.Lock()
				if err := s.notifyPersistenceLockState(ctx, true); err != nil {
					log.Printf("WARN: reset-password relock notify failed: %v", err)
				}
			}
		}()
	}

	if err := s.authManager.ChangePasswordWithRecovery(ctx, newPassword); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Also update admin user password in SQLite (userManager)
	if s.userManager != nil {
		if admin, err := s.userManager.GetByUsername(ctx, "admin"); err == nil {
			if err := s.userManager.SetPassword(ctx, admin.ID, newPassword); err != nil {
				log.Printf("ERROR: reset-password userManager sync failed: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to sync admin password"})
				return
			}
		} else {
			log.Printf("WARN: admin user not found in userManager during reset: %v", err)
		}
	}

	if err := s.cryptoManager.RewrapUnlocked(newPassword); err != nil {
		log.Printf("ERROR: reset-password rewrap failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to rewrap keys"})
		return
	}

	// Rotate LUKS keyslot 1 (admin-password passphrase) on all volumes.
	if kp, ok := s.persistence.(persistence.KeyslotProvisioner); ok {
		passBytes := []byte(newPassword)
		if err := kp.ProvisionLUKSKeyslot(ctx, 1, passBytes); err != nil {
			log.Printf("WARN: LUKS keyslot 1 rotation during reset: %v", err)
		}
		cryptoutil.SecureZero(passBytes)
	}

	now := time.Now().UTC()
	update := persistence.AuthStalenessUpdate{
		PasswordStale:   boolPtr(true),
		PasswordStaleAt: timePtr(now),
		PasswordAckAt:   timePtr(time.Time{}),
		RecoveryStale:   boolPtr(true),
		RecoveryStaleAt: timePtr(now),
		RecoveryAckAt:   timePtr(time.Time{}),
	}
	if err := s.applyStalenessUpdate(ctx, update); err != nil {
		log.Printf("WARN: failed to mark staleness: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to mark staleness"})
		return
	}

	if wasLocked {
		s.cryptoManager.Lock()
		if err := s.notifyPersistenceLockState(ctx, true); err != nil {
			log.Printf("WARN: reset-password relock notify failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to relock persistence"})
			return
		}
		needRelock = false
	}

	s.publishAuditEvent(c, "auth.reset_with_recovery", map[string]any{"was_locked": wasLocked})

	c.JSON(http.StatusOK, gin.H{"message": "ok"})
}

// handleCryptoLock: POST /api/v1/crypto/lock
func (s *GinServer) handleCryptoLock(c *gin.Context) {
	if !s.cryptoManager.IsInitialized() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not initialized"})
		return
	}
	ctx, cancel := s.opContext(c, 2*time.Minute)
	defer cancel()
	// Lock LUKS data volume before crypto lock (best-effort).
	if s.storageMgr != nil {
		if err := s.storageMgr.LockDataVolume(ctx); err != nil {
			log.Printf("WARN: lock data volume: %v", err)
		}
	}
	s.cryptoManager.Lock()
	if err := s.notifyPersistenceLockState(ctx, true); err != nil {
		log.Printf("WARN: failed to propagate lock state: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update persistence state"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "ok"})
}

// handleCryptoRecoveryStatus: GET /api/v1/crypto/recovery-key
func (s *GinServer) handleCryptoRecoveryStatus(c *gin.Context) {
	present := false
	if s.cryptoManager != nil && s.cryptoManager.IsInitialized() {
		present = s.cryptoManager.HasRecoveryKey()
	}
	stale := false
	if st, err := s.readAuthStaleness(c.Request.Context()); err == nil {
		stale = st.RecoveryStale
	}
	c.JSON(http.StatusOK, gin.H{"present": present, "stale": stale})
}

// handleCryptoRecoveryGenerate: POST /api/v1/crypto/recovery-key/generate
func (s *GinServer) handleCryptoRecoveryGenerate(c *gin.Context) {
	// C7 / RF-4: belt-and-suspenders denial for the bootstrap window. Runs
	// before the initialization check to avoid leaking crypto-state via the
	// 400-vs-403 response distinction. Even if the middleware allowlist
	// regresses to admit /crypto/recovery-key/generate under
	// MustRegisterPasskey=true, this handler refuses. Closes both the D-1
	// extension and the pre-existing variant where an attacker with the
	// password could rotate the recovery key during normal bootstrap.
	if _, gated := s.sessionGate(c); gated {
		c.JSON(http.StatusForbidden, gin.H{
			"error": "passkey_registration_required",
		})
		return
	}
	if s.cryptoManager == nil || !s.cryptoManager.IsInitialized() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not initialized"})
		return
	}
	// Optional body: { password, force_rotate }
	var body struct {
		Password    string `json:"password"`
		ForceRotate bool   `json:"force_rotate"`
	}
	_ = c.ShouldBindJSON(&body)
	// Refuse to rotate an already-acknowledged recovery key without explicit
	// force_rotate. Closes the REC-FOLLOWUP root cause: `recovery_key_pending`
	// can spuriously flip true (transient DB error, locked-state misclass,
	// etc.) — without this guard, the UI's response is to call /generate,
	// which would rotate and silently invalidate the operator's saved paper
	// words. The unack'd-key path stays rotation-friendly because the
	// operator never had paper words from that key (UI didn't display them
	// or display failed), so generating fresh is harmless. Fail-closed on
	// read errors mirrors computeRecoveryKeyPending's conservative posture.
	keyExists := s.cryptoManager.HasRecoveryKey()
	if keyExists && !body.ForceRotate {
		st, err := s.readAuthStaleness(c.Request.Context())
		switch {
		case errors.Is(err, persistence.ErrLocked):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": ErrAckStateUnknown})
			return
		case err != nil:
			log.Printf("WARN: recovery-key generate ack-state read: %v", err)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": ErrAckStateUnknown})
			return
		case !st.RecoveryAckAt.IsZero():
			c.JSON(http.StatusConflict, gin.H{"error": ErrRecoveryKeyAlreadyAcked})
			return
		}
	}
	var words []string
	var keyID string
	var err error
	rotating := keyExists
	// Prefer unlocked path; else use direct with password.
	// REC-4: keyID is returned atomically by GenerateRecoveryKey under its
	// write lock — pairing with words from the same call. Computing the id
	// here via a separate RecoveryKeyID() read would reintroduce the
	// concurrent-rotation race the binding is designed to close.
	if !s.cryptoManager.IsLocked() {
		words, keyID, err = s.cryptoManager.GenerateRecoveryKey(rotating)
	} else if strings.TrimSpace(body.Password) != "" {
		words, keyID, err = s.cryptoManager.GenerateRecoveryKeyWithPassword(body.Password, rotating)
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unlock required"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Provision LUKS keyslot 2 (recovery-mnemonic passphrase) on all volumes.
	// Only possible when unlocked — SDEK must be cached to unwrap master key.
	// When locked, the mnemonic still works for app-level recovery (wrapped in
	// state file) but won't be usable for direct disk-level cryptsetup open
	// until the next unlocked recovery-key rotation.
	ctx, cancel := s.opContext(c, 2*time.Minute)
	defer cancel()
	if !s.cryptoManager.IsLocked() {
		if kp, ok := s.persistence.(persistence.KeyslotProvisioner); ok {
			mnemonic := strings.Join(words, " ")
			mnemonicBytes := []byte(mnemonic)
			if err := kp.ProvisionLUKSKeyslot(ctx, 2, mnemonicBytes); err != nil {
				log.Printf("WARN: LUKS keyslot 2 provisioning: %v", err)
			}
			cryptoutil.SecureZero(mnemonicBytes)
		}
	} else {
		log.Printf("INFO: LUKS keyslot 2 provisioning deferred (system locked)")
	}
	if err := s.applyStalenessUpdate(ctx, persistence.AuthStalenessUpdate{
		RecoveryStale:   boolPtr(false),
		RecoveryStaleAt: timePtr(time.Time{}),
		RecoveryAckAt:   timePtr(time.Time{}),
	}); err != nil {
		log.Printf("WARN: failed to clear recovery staleness: %v", err)
	}
	s.publishAuditEvent(c, "auth.recovery_key_generate", map[string]any{"rotated": rotating})
	// REC-4: keyID is paired with words by GenerateRecoveryKey under its
	// write lock. Do not re-derive from RecoveryKeyID() here — a concurrent
	// /generate from another tab can rotate past us between lock release
	// and the read, returning words from generation A but key_id of
	// generation B. The /ack would then accept the wrong-words tab's id.
	c.JSON(http.StatusOK, gin.H{"words": words, "key_id": keyID})
}

// handleCryptoRecoveryKeyAck: POST /api/v1/crypto/recovery-key/ack
// Records that the operator has seen and saved the current recovery-key words.
// Without this, recovery_key_pending is presence-based — a UI timeout between
// /crypto/setup writing keyset.json and the words reaching the browser leaves
// the user permanently locked out of password recovery despite the file
// existing on disk. The boot handler gates recovery_key_pending on
// RecoveryAckAt being non-zero so a reload after timeout re-prompts.
func (s *GinServer) handleCryptoRecoveryKeyAck(c *gin.Context) {
	// C7 belt-and-suspenders: the requireSession middleware admits the entire
	// /api/v1/crypto/recovery-key/ prefix during the MustRegisterPasskey
	// bootstrap window so the read-only status endpoint stays reachable. State-
	// mutating handlers under that prefix must individually deny — sibling
	// handleCryptoRecoveryGenerate enforces the same gate. Without this guard,
	// a stolen-admin-password attacker in the bootstrap window could ack the
	// recovery key, suppressing the boot-time prompt that REC-1 exists to
	// surface to the legitimate operator.
	if _, gated := s.sessionGate(c); gated {
		c.JSON(http.StatusForbidden, gin.H{"error": "passkey_registration_required"})
		return
	}
	// REC-4: bind ack to the specific recovery-key version the operator was
	// shown. Without this binding, a multi-tab rotation race or a delayed
	// fire-and-forget ack from a stale tab can mark a *rotated* key as acked,
	// suppressing the boot-time prompt for the operator who never saw the
	// new words. The server rejects with 409 stale_recovery_key_id when the
	// supplied key_id doesn't match the current SDEKRK fingerprint; the UI
	// surfaces this as a "your saved words are out of date — view current"
	// flow rather than silently advancing.
	var ackBody struct {
		KeyID string `json:"key_id"`
	}
	_ = c.ShouldBindJSON(&ackBody)
	suppliedID := strings.TrimSpace(ackBody.KeyID)
	if suppliedID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key_id required"})
		return
	}
	currentID := s.cryptoManager.RecoveryKeyID()
	if currentID == "" {
		// No recovery key on disk — nothing to ack against. Either an
		// out-of-band reset cleared it or /generate has never run; either way
		// /ack is meaningless. 409 over 400 because the request shape is
		// valid; only the server state is wrong.
		c.JSON(http.StatusConflict, gin.H{"error": "stale_recovery_key_id"})
		return
	}
	if suppliedID != currentID {
		c.JSON(http.StatusConflict, gin.H{"error": "stale_recovery_key_id"})
		return
	}
	ctx, cancel := s.opContext(c, 30*time.Second)
	defer cancel()
	now := time.Now().UTC()
	if err := s.applyStalenessUpdate(ctx, persistence.AuthStalenessUpdate{
		RecoveryAckAt: timePtr(now),
	}); err != nil {
		log.Printf("WARN: recovery-key ack failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record acknowledgement"})
		return
	}
	s.publishAuditEvent(c, "auth.recovery_key_ack", nil)
	c.JSON(http.StatusOK, gin.H{"message": "ok"})
}
