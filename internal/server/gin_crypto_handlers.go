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

// handleCryptoStatus: GET /api/v1/crypto/status
func (s *GinServer) handleCryptoStatus(c *gin.Context) {
	init := s.cryptoManager != nil && s.cryptoManager.IsInitialized()
	locked := false
	if init {
		locked = s.cryptoManager.IsLocked()
	}
	c.JSON(http.StatusOK, gin.H{"initialized": init, "locked": locked})
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
		c.JSON(http.StatusConflict, gin.H{"error": "setup already in progress"})
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

	// Use a background context so long-running ops survive client disconnect.
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 10*time.Minute)
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
		c.JSON(http.StatusConflict, gin.H{"error": "setup already complete"})
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
		sess.MustRegisterPasskey = true
	}

	s.setSessionCookie(c, sess.ID, portalSessionCookieTTL)

	// Consume the setup nonce now that setup has succeeded.
	s.sessions.ConsumeSetupNonce()

	// Mark onboarding as complete (accepts try_piccolo, pending, or already-complete).
	// Best-effort: failure here doesn't block setup since LUKS header serves as fallback signal.
	if s.onboardingMgr != nil {
		if err := s.onboardingMgr.Complete(); err != nil {
			log.Printf("WARN: onboarding complete: %v", err)
		}
	}

	// Fail after session creation so the user has portal access for recovery.
	if luksErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "data volume initialization failed: " + luksErr.Error(),
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

	// Use a background context so long-running ops survive client disconnect.
	unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 10*time.Minute)
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
	resp := gin.H{"message": "ok", "setup_complete": setupComplete}
	if luksErr != nil {
		resp["warning"] = "data volume unlock failed: " + luksErr.Error()
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

	ctx := c.Request.Context()
	words := strings.Fields(recoveryKey)
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
	// Lock LUKS data volume before crypto lock (best-effort).
	if s.storageMgr != nil {
		if err := s.storageMgr.LockDataVolume(c.Request.Context()); err != nil {
			log.Printf("WARN: lock data volume: %v", err)
		}
	}
	s.cryptoManager.Lock()
	if err := s.notifyPersistenceLockState(c.Request.Context(), true); err != nil {
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
	if s.cryptoManager == nil || !s.cryptoManager.IsInitialized() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not initialized"})
		return
	}
	// Optional body: { password }
	var body struct {
		Password string `json:"password"`
	}
	_ = c.ShouldBindJSON(&body)
	var words []string
	var err error
	rotating := s.cryptoManager.HasRecoveryKey()
	// Prefer unlocked path; else use direct with password
	if !s.cryptoManager.IsLocked() {
		words, err = s.cryptoManager.GenerateRecoveryKey(rotating)
	} else if strings.TrimSpace(body.Password) != "" {
		words, err = s.cryptoManager.GenerateRecoveryKeyWithPassword(body.Password, rotating)
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
	if !s.cryptoManager.IsLocked() {
		if kp, ok := s.persistence.(persistence.KeyslotProvisioner); ok {
			mnemonic := strings.Join(words, " ")
			mnemonicBytes := []byte(mnemonic)
			if err := kp.ProvisionLUKSKeyslot(c.Request.Context(), 2, mnemonicBytes); err != nil {
				log.Printf("WARN: LUKS keyslot 2 provisioning: %v", err)
			}
			cryptoutil.SecureZero(mnemonicBytes)
		}
	} else {
		log.Printf("INFO: LUKS keyslot 2 provisioning deferred (system locked)")
	}
	if err := s.applyStalenessUpdate(c.Request.Context(), persistence.AuthStalenessUpdate{
		RecoveryStale:   boolPtr(false),
		RecoveryStaleAt: timePtr(time.Time{}),
		RecoveryAckAt:   timePtr(time.Time{}),
	}); err != nil {
		log.Printf("WARN: failed to clear recovery staleness: %v", err)
	}
	s.publishAuditEvent(c, "auth.recovery_key_generate", map[string]any{"rotated": rotating})
	c.JSON(http.StatusOK, gin.H{"words": words})
}
