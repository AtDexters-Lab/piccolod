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
	"piccolod/internal/events"
	"piccolod/internal/health"
	"piccolod/internal/persistence"
)

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
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	// 1. Validate password policy FIRST (before any state changes)
	if err := auth.ValidatePasswordStrength(body.Password); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 2. Reject if already initialized and locked — this is a reboot, not first-time
	// setup. The caller should use /crypto/unlock instead, which handles recovery
	// for incomplete LUKS initialization (Posture RFC §5.3).
	if s.cryptoManager.IsInitialized() && s.cryptoManager.IsLocked() {
		c.JSON(http.StatusConflict, gin.H{"error": "already initialized, use /crypto/unlock"})
		return
	}

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
	if s.cryptoManager.IsLocked() {
		if err := s.cryptoManager.Unlock(body.Password); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "wrong password"})
			return
		}
	} else if s.cryptoManager.IsInitialized() {
		// Already initialized and unlocked. If setup is fully complete,
		// reject to prevent unauthenticated session creation (auth bypass).
		// Fail closed: transient lookup errors return 500 rather than
		// falling through to the destructive InitializeDataVolume path.
		setupComplete := false
		if s.userManager != nil {
			// Check if any users exist (not just "admin") so that
			// renamed/deleted admin accounts don't bypass the guard.
			count, err := s.userManager.Count(c.Request.Context())
			if err == nil && count > 0 {
				setupComplete = true
			} else if err != nil && !errors.Is(err, persistence.ErrLocked) {
				log.Printf("ERROR: user count failed during setup guard: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "setup state check failed"})
				return
			}
		} else if s.authManager != nil {
			initialized, err := s.authManager.IsInitialized(c.Request.Context())
			if err != nil && !errors.Is(err, persistence.ErrLocked) {
				log.Printf("ERROR: auth init check failed during setup guard: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "setup state check failed"})
				return
			}
			if err == nil && initialized {
				setupComplete = true
			}
		}
		if setupComplete {
			c.JSON(http.StatusConflict, gin.H{"error": "setup already complete"})
			return
		}
		// Partial retry: verify password matches the existing crypto key.
		// Unlock is idempotent when already unlocked (re-derives SDEK),
		// and leaves state unchanged on failure — no Lock() side effect.
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

	// 5. Initialize LUKS data volume BEFORE notifying persistence, so that
	// /piccolo-data is mounted before the app-manager reconcile loop
	// starts (RCA: docs/rca/20260212-gocryptfs-password-mismatch-on-reboot.md).
	// Capture error but defer failure until after session creation,
	// so the user gets a portal session for recovery on LUKS failure.
	var luksErr error
	if s.storageMgr != nil {
		if err := s.storageMgr.InitializeDataVolume(setupCtx, body.Password, nil); err != nil {
			log.Printf("ERROR: data volume initialization failed: %v", err)
			luksErr = err
			if s.healthTracker != nil {
				s.healthTracker.Setf("storage", health.LevelError, "data volume initialization failed")
			}
		}
	}

	// 6. Notify persistence (mounts control-plane gocryptfs, enables SQLite)
	if err := s.notifyPersistenceLockState(setupCtx, false); err != nil {
		log.Printf("WARN: failed to propagate unlock state: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update persistence state"})
		return
	}

	// 7. Setup auth manager (mandatory — needs SQLite from step 6) — skip if already initialized.
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

	// 9. Activate PCV publisher (depends on gocryptfs, not LUKS — safe even on LUKS failure).
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
	s.setSessionCookie(c, sess.ID, portalSessionCookieTTL)

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
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

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

	// Unlock LUKS data volume BEFORE notifying persistence, so that
	// /piccolo-data is mounted before the app-manager reconcile loop
	// starts (RCA: docs/rca/20260212-gocryptfs-password-mismatch-on-reboot.md).
	var luksErr error
	if s.storageMgr != nil {
		if err := s.storageMgr.UnlockDataVolume(unlockCtx, password); err != nil {
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
	// Activate PCV publisher (always — depends on gocryptfs, not LUKS).
	if s.pcvPublisher != nil {
		s.pcvPublisher.Activate()
	}
	// Best-effort: verify admin credentials and create a session automatically.
	ctx := unlockCtx
	init := false
	if s.authManager != nil {
		if initialized, err := s.authManager.IsInitialized(ctx); err == nil {
			init = initialized
			if !initialized {
				if err := s.authManager.Setup(ctx, password); err != nil {
					log.Printf("WARN: auth setup during unlock failed: %v", err)
				} else {
					init = true
				}
			}
		}
		if init {
			if ok, err := s.authManager.Verify(ctx, "admin", password); err == nil && ok {
				userID := ""
				if s.userManager != nil {
					if u, err := s.userManager.GetByUsername(ctx, "admin"); err == nil {
						userID = u.ID
					}
				}
				// RFC 20260122 §6.2: Create portal session with origin binding
				boundOrigin := s.computeCanonicalOrigin(c)
				sess := s.sessions.CreatePortalSession(userID, "admin", "admin", boundOrigin, portalSessionTTL)
				s.setSessionCookie(c, sess.ID, portalSessionCookieTTL)
			}
		}
	}
	// Fail if LUKS unlock failed (after session is created for portal recovery access).
	if luksErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "data volume unlock failed: " + luksErr.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "ok"})
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

	if s.events != nil {
		s.events.Publish(events.Event{
			Topic: events.TopicAudit,
			Payload: events.AuditEvent{
				Kind:   "auth.reset_with_recovery",
				Time:   now,
				Source: c.ClientIP(),
				Metadata: map[string]any{
					"was_locked": wasLocked,
				},
			},
		})
	}

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
	if err := s.applyStalenessUpdate(c.Request.Context(), persistence.AuthStalenessUpdate{
		RecoveryStale:   boolPtr(false),
		RecoveryStaleAt: timePtr(time.Time{}),
		RecoveryAckAt:   timePtr(time.Time{}),
	}); err != nil {
		log.Printf("WARN: failed to clear recovery staleness: %v", err)
	}
	if s.events != nil {
		s.events.Publish(events.Event{
			Topic: events.TopicAudit,
			Payload: events.AuditEvent{
				Kind:   "auth.recovery_key_generate",
				Time:   time.Now().UTC(),
				Source: c.ClientIP(),
				Metadata: map[string]any{
					"rotated": rotating,
				},
			},
		})
	}
	c.JSON(http.StatusOK, gin.H{"words": words})
}
