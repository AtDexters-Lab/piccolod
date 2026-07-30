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
	"piccolod/internal/autounlock"
	"piccolod/internal/crypt"
	"piccolod/internal/cryptoutil"
	"piccolod/internal/health"
	"piccolod/internal/lifecycle"
	"piccolod/internal/persistence"
)

// Structured error codes returned in JSON responses. Matched by frontend.
const (
	errorCodeSetupInProgress     = "setup_in_progress"
	errorCodeSetupComplete       = "setup_complete"
	errorCodeStorageInitFailed   = "storage_init_failed"
	errorCodeStorageUnlockFailed = "storage_unlock_failed"
	errorCodeStorageEmergency    = "storage_emergency"
	errorCodeRecoveryInProgress  = "recovery_in_progress"
	// errorCodeAuthMigrationDegradedStorage: persistence.Unlock aborted
	// because the recovery_ack_at backfill could not evaluate keyset.json on
	// degraded storage (RFC 20260510 §Backfill migration). The control store
	// stays locked; the operator must attend storage health before retrying.
	errorCodeAuthMigrationDegradedStorage = "auth_migration_degraded_storage"

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

// handoffSlot1ToReconciler hands a newly-set admin password off to the
// keyslot reconciler (RFC 20260510 §Slot-1 expansion). The key_id is the
// fingerprint of the committed admin password_hash from the users repo —
// reading back here (rather than recomputing the hash) guarantees the
// fingerprint matches what the reconciler's livePasswordKeyID probe sees
// (which also reads the users repo). Otherwise the kskey_id stamping
// could permanently drift from the live id and prevent convergence.
//
// Common to handleAuthPassword, handleCryptoResetPassword, handleCryptoSetup.
//
// Slot-1 ordering deviation from RFC D11: the password commit
// (userManager.ChangePassword + Rewrap) has already happened by the time
// this runs. Full D11 would require pre-computing the hash, writing the
// blob with the candidate fingerprint, then calling a SetPasswordHash
// (pre-hashed) variant on userManager — a 3-call-site API change across
// userManager/authManager/CreateUserInput that exceeds this RFC's surface.
// Trade-off: on blob-write failure here, the operator's web/portal access
// works (password changed cleanly), but cryptsetup luksOpen on volumes
// stays at the prior password until the next rotation. Same recovery
// shape as RFC §Volume-creation sub-case-ii's rotate-to-recover policy.
//
// No sync fallback: the legacy ProvisionLUKSKeyslot path is what this RFC
// exists to escape (N×Argon2id under the request opContext). On blob-write
// failure we surface a typed audit event and stop — operator retries the
// password change after attending storage health.
func (s *GinServer) handoffSlot1ToReconciler(ctx context.Context, newPassword string) {
	kp, ok := s.persistence.(persistence.KeyslotProvisioner)
	if !ok {
		return
	}
	passBytes := []byte(newPassword)
	defer cryptoutil.SecureZero(passBytes)

	keyID := ""
	if s.userManager != nil {
		if u, err := s.persistence.Control().Users().GetByUsername(ctx, "admin"); err == nil && u.PasswordHash != "" {
			keyID = crypt.FingerprintPasswordHash(u.PasswordHash)
		}
	}
	if keyID == "" {
		log.Printf("WARN: keyslot 1 hand-off skipped: admin password_hash unavailable; next password change will retry")
		return
	}
	if err := kp.WriteKeyslotBlob(ctx, persistence.KeyslotPassword, keyID, passBytes); err != nil {
		log.Printf("ERROR: keyslot 1 blob write failed: %v; slot-1 LUKS provisioning deferred to next password change", err)
		return
	}
	if s.keyslotReconciler != nil {
		s.keyslotReconciler.Nudge()
	}
}

// unlockChainResult carries the data each caller needs to shape its response
// after the post-decrypt chain runs. luksErr is non-fatal — caller surfaces
// it as a warning. setupComplete drives the UI's post-unlock routing.
type unlockChainResult struct {
	setupComplete bool
	luksErr       error
}

// runUnlockChain is the lifecycle-unaware body of the post-decrypt chain.
// The joinable execution coordinator is the only owner allowed to publish a
// lifecycle terminal transition after this function returns.
func (s *GinServer) runUnlockChain(ctx context.Context) (unlockChainResult, error) {
	var luksErr error

	log.Printf("INFO: releasing KDF memory before storage unlock")
	runtime.GC()
	debug.FreeOSMemory()

	// Unlock data volume BEFORE notifying persistence, so storage volumes
	// are available before the app-manager reconcile loop starts.
	if s.storageMgr != nil {
		if err := s.storageMgr.UnlockDataVolume(ctx); err != nil {
			log.Printf("ERROR: data volume unlock failed: %v", err)
			luksErr = err
			if s.healthTracker != nil {
				s.healthTracker.Setf("storage", health.LevelError, "data volume unlock failed")
			}
		}
	}
	if err := s.notifyPersistenceLockState(ctx, false); err != nil {
		log.Printf("WARN: failed to propagate unlock state: %v", err)
		return unlockChainResult{}, err
	}
	// PCV publisher depends on the control-plane mount, safe even on data LUKS failure.
	if s.pcvPublisher != nil {
		s.pcvPublisher.Activate()
	}
	// Nudge the keyslot reconciler after the control plane is mounted —
	// any pending blob left by a prior process or reboot was un-drainable
	// during the reconciler's Start() initial pass (SDEK locked at boot)
	// and will not retry until an unrelated rotation otherwise. RFC
	// 20260510 §Reconciler restart-safety + codex P2 #2.
	if s.keyslotReconciler != nil {
		s.keyslotReconciler.Nudge()
	}

	setupComplete, err := s.isSetupComplete(ctx)
	if err != nil {
		// ErrLocked here means persistence claimed unlocked
		// (notifyPersistenceLockState returned nil above) but the user
		// repo immediately reports locked. The two layers are out of
		// sync — a real bug, not a transient. Escalate to the caller
		// (HTTP 500) so the user retries; treating it as "fail closed
		// to complete" would silently route to login on a system whose
		// data plane isn't actually queryable.
		if errors.Is(err, persistence.ErrLocked) {
			log.Printf("ERROR: persistence inconsistency — notify said unlocked but userManager reports ErrLocked: %v", err)
			return unlockChainResult{}, err
		}
		log.Printf("WARN: setup-complete check failed, assuming complete: %v", err)
		// Fail closed for genuine transient errors: assume provisioned,
		// route to login. The user can retry if needed.
		s.reloadComponentsAfterUnlock(ctx)
		return unlockChainResult{setupComplete: true, luksErr: luksErr}, nil
	}
	// Reconcile the durable provisioning marker from the authoritative
	// post-unlock answer. Closes the gap left by handleCryptoSetup's
	// best-effort MarkProvisioned write.
	if rerr := s.provisioningState.ReconcileFromPersistence(setupComplete); rerr != nil {
		log.Printf("WARN: reconcile provisioning: %v", rerr)
	}
	// Reloaders are part of the complete-unlock execution, not a detached
	// post-success callback. Keeping them inside this body means the
	// coordinator's independent liveness timer also covers a blocked reloader
	// and lifecycle Ready cannot be published prematurely.
	s.reloadComponentsAfterUnlock(ctx)
	return unlockChainResult{setupComplete: setupComplete, luksErr: luksErr}, nil
}

// initialLifecycleState derives the lifecycle bootstrap state from durable
// signals available at process start. cryptoManager.IsInitialized() is the
// only signal needed: a fresh device has no keyset (NotInitialized); an
// already-set-up device starts Locked and transitions via BeginUnlock when
// the unlock pathway (manual or auto-unlock pickup) begins.
func initialLifecycleState(cmgr *crypt.Manager) lifecycle.State {
	if cmgr == nil || !cmgr.IsInitialized() {
		return lifecycle.StateNotInitialized
	}
	return lifecycle.StateLocked
}

// isSetupComplete checks whether first-run provisioning has finished by
// counting users (primary) or checking auth initialization (fallback).
//
// Three-way result encoded in (bool, error):
// - (true, nil) — provisioning complete (≥1 user OR auth
// manager reports initialized)
// - (false, nil) — store readable AND no users / auth not
// initialized (genuinely incomplete)
// - (false, persistence.ErrLocked) — control store is locked; the
// question cannot be answered authoritatively
// right now. Callers MUST distinguish this
// from genuine incompleteness via
// errors.Is(err, persistence.ErrLocked).
// - (false, err) — transient error; callers should fail
// toward whichever direction is safer for
// their gate.
//
// The ErrLocked propagation is what closes the original race-window bug:
// previously this function swallowed ErrLocked into (false, nil), which
// any gate of shape "if !setupComplete → setup" would misclassify as
// "device not provisioned" during the multi-second window between SDEK
// load and persistence load. Callers that need a binary answer in the
// presence of ErrLocked should consult provisioningState.IsProvisioned()
// (durable, pre-unlock-readable) as the primary signal.
func (s *GinServer) isSetupComplete(ctx context.Context) (bool, error) {
	if s.userManager != nil {
		count, err := s.userManager.Count(ctx)
		if err == nil {
			return count > 0, nil
		}
		return false, err
	}
	if s.authManager != nil {
		initialized, err := s.authManager.IsInitialized(ctx)
		if err == nil {
			return initialized, nil
		}
		return false, err
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
	// `locked` is composite readiness when the device has a keyset —
	// sourced from lifecycle so callers don't see an inconsistent
	// "unlocked" answer mid-chain while persistence is still loading.
	// On a fresh (uninitialized) device, locked=false preserves the
	// pre-existing wire contract where pre-setup means "no keyset, no
	// lock to speak of" (the smoke test in scripts/production/dev-vm-test.sh
	// asserts this shape).
	locked := false
	if init {
		locked = s.lifecycle == nil || !s.lifecycle.IsReady()
	}
	resp := gin.H{
		"initialized":       init,
		"locked":            locked,
		"setup_in_progress": s.isSetupInProgress(),
	}
	// Lifecycle wire token — same view-side normalization as /system/boot
	// (Failed→"locked", autoInFlight bridge → "unlocking"). Without the
	// bridge, /crypto/status loses the in-flight progress signal that the
	// retired auto_unlock_in_flight field carried during the early-pickup
	// window where coordinator state is still Locked.
	autoInFlight := s.autounlockOrch != nil && s.autounlockOrch.InFlight()
	resp["lifecycle"] = wireLifecycleToken(s.lifecycle.State(), autoInFlight)

	// Auto-unlock surface — public, pre-auth. Reports enabled state so
	// pre-auth UI can decide whether the auto-unlock card should render
	// the toggle as on. The previous `auto_unlock_in_flight` field is
	// retired in favor of the canonical `lifecycle` token (==
	// "unlocking" while pickup is in flight). Failure detail (reason +
	// timestamp) lives behind the session gate; emitting it here would
	// leak operator-visible state.
	if state, err := autounlock.LoadState(); err == nil {
		resp["auto_unlock_enabled"] = state.Enabled
	}

	// Keyslot provisioning surface (RFC 20260510 §Status surface). Reports
	// per-slot reconciler progress so the operator can observe
	// offline-recovery-readiness for slot 1 (admin password) and slot 2
	// (recovery mnemonic) independently. captured_key_id == target_key_id
	// means "no rotation queued; reconciler is converging on the latest";
	// captured != target means "rotation storm — reconciler will pick up
	// the latest after the current pass completes" (S5 fix).
	if s.keyslotReconciler != nil {
		status := s.keyslotReconciler.Status()
		// unprovisioned_steady_state is counted from on-disk volume
		// metadata (single walk for both slots) so it reflects the
		// current truth — volumes created since the last reconciler
		// pass are stamped "unprovisioned" at creation and need to be
		// surfaced even before the next pass runs.
		s1Unprov, s2Unprov := 0, 0
		if kp, ok := s.persistence.(persistence.KeyslotProvisioner); ok {
			s1Unprov, s2Unprov, _ = kp.CountKeyslotUnprovisioned()
		}
		resp["keyslot_provisioning"] = gin.H{
			"slot1": keyslotStatusForWire(status[persistence.KeyslotPassword], s1Unprov),
			"slot2": keyslotStatusForWire(status[persistence.KeyslotRecovery], s2Unprov),
		}
	}
	c.JSON(http.StatusOK, resp)
}

// keyslotStatusForWire shapes the per-slot status struct into the JSON
// surface defined in RFC 20260510 §Status surface. Field names are wire-
// canonical; do not rename without updating frontend consumers. The
// unprovisioned_steady_state override is supplied by the caller (counted
// from on-disk metadata, not from reconciler state) per the S7 fix.
func keyslotStatusForWire(st persistence.KeyslotReconcilerStatus, unprovisionedCount int) gin.H {
	return gin.H{
		"captured_key_id":            st.CapturedKeyID,
		"target_key_id":              st.TargetKeyID,
		"total_volumes":              st.TotalVolumes,
		"done":                       st.Done,
		"pending":                    st.Pending,
		"unprovisioned_steady_state": unprovisionedCount,
		"rotation_storm_pending":     st.RotationStormPending,
		"in_progress":                st.InProgress,
		"last_error":                 st.LastError,
	}
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
	// Narrow SDEK-presence check here (not lifecycle.IsReady) — we want
	// "is the SDEK currently in memory?", which is exactly what the
	// crypto layer answers; lifecycle composite readiness would also
	// trigger Unlock during the post-decrypt chain, which is wrong.
	if !s.cryptoManager.SDEKLoaded() {
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

	// Lifecycle: claim the unlock-chain phase now that the SDEK is loaded.
	// Resolved by the deferred clause based on `ready` — the 409
	// already-complete short-circuit also counts as Ready (the system is in
	// fact up; we just rejected a duplicate setup attempt). All other early
	// returns leave ready=false and resolve as Failed.
	lifecycleOwned := false
	ready := false
	if s.lifecycle != nil {
		if err := s.lifecycle.BeginUnlock(); err == nil {
			lifecycleOwned = true
		} else {
			log.Printf("INFO: lifecycle: BeginUnlock skipped during setup (%v)", err)
		}
	}
	defer func() {
		if !lifecycleOwned || s.lifecycle == nil {
			return
		}
		if ready {
			// Fresh setup does not run through the joinable unlock execution
			// owner. Complete the same post-decrypt reload work before publishing
			// lifecycle Ready so decrypted observers cannot run ahead of it.
			s.reloadComponentsAfterUnlock(setupCtx)
			if err := s.lifecycle.MarkReady(); err != nil {
				log.Printf("WARN: lifecycle: setup Ready commit failed: %v", err)
				return
			}
			s.onUnlockChainReady()
		} else {
			_ = s.lifecycle.MarkFailed(errors.New("setup did not complete"))
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
		// 409: the device is fully provisioned. The post-decrypt chain ran
		// successfully above (steps 4 + 5), so lifecycle should resolve to
		// Ready — nothing failed; we just rejected a duplicate setup.
		ready = true
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

	// The data pool now exists. At first setup the persistence unlock ran
	// before this step, so the app-logs store could not provision then —
	// attach it now. Idempotent on retries and on
	// normal boots (where the pool is activated before persistence unlock).
	if luksErr == nil && s.persistence != nil {
		s.persistence.AttachAppLogs(setupCtx)
	}

	// 6b. Provision LUKS keyslot 1 (admin password) via the async
	// reconciler (RFC 20260510 §Slot-1 expansion). At first setup there's
	// typically only the control-plane volume, so the latency benefit is
	// small, but uniformity with the post-setup rotation path keeps the
	// volume metadata stamping consistent (otherwise sub-case-i hooks
	// during early app installs would observe an inconsistent slot-1
	// fingerprint state).
	//
	// Wait for the admin user creation below to commit the password_hash
	// before handing off — the fingerprint is derived from the persisted
	// hash. We defer the handoff via a slot1 closure and call it in
	// step 8 after admin user creation.
	slot1Handoff := func(handoffCtx context.Context) {
		if luksErr != nil {
			return
		}
		s.handoffSlot1ToReconciler(handoffCtx, body.Password)
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

	// 8b. Hand off slot-1 (admin password) to the keyslot reconciler now
	// that the admin user's password_hash is committed to the users repo.
	// The handoff fingerprints the persisted hash so the reconciler's
	// livePasswordKeyID probe sees the same id (RFC 20260510 §Slot-1
	// expansion).
	slot1Handoff(setupCtx)

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

	// Lifecycle Ready: admin user, persistence, and control plane are up.
	// Set BEFORE the luksErr 500 fork so a degraded data plane still resolves
	// to Ready — subsequent boot polls correctly route to desktop, and the
	// LUKS error surfaces via health tracker rather than as a persistent
	// "system is failed" lifecycle state.
	ready = true

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

	// Decouple from HTTP request context — unlock chain must survive connection drops.
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

	result, err := s.completeUnlockChain(unlockCtx)
	if err == nil && s.healthTracker != nil {
		// Successful unlock — clear any prior auth-migration degraded
		// flag so the locked-screen banner doesn't persist across
		// recovery (operator fixed storage and the migration succeeded
		// this time). Idempotent on absent keys.
		s.healthTracker.Clear("auth-migration")
	}
	if err != nil {
		var inProgress *recoveryInProgressError
		if errors.As(err, &inProgress) {
			c.JSON(http.StatusConflict, gin.H{
				"error": inProgress.Error(),
				"code":  inProgress.Code(),
			})
			return
		}
		if errors.Is(err, persistence.ErrAuthMigrationDegradedStorage) {
			// RFC 20260510 §Backfill migration: keep the typed signal alive
			// for /system/boot to surface a degraded-startup banner so
			// operators triaging by web UI or serial console see the
			// underlying storage health, not just a generic 5xx. Health
			// tracker is the durable surface across polls; the JSON code
			// carries the immediate signal.
			if s.healthTracker != nil {
				s.healthTracker.Setf("auth-migration", health.LevelError, "auth migration aborted (degraded storage): %v", err)
			}
			s.publishAuditEvent(c, "auth.migration_keyset_unreadable", map[string]any{"reason": err.Error()})
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "Storage attention required. Saved recovery words remain valid. Use the device console or contact support.",
				"code":  errorCodeAuthMigrationDegradedStorage,
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update persistence state"})
		return
	}
	resp := gin.H{"message": "ok", "setup_complete": result.setupComplete}
	if result.luksErr != nil {
		resp["warning"] = "data volume unlock failed: " + result.luksErr.Error()
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
	// Narrow SDEK-presence check: the rewrap flow needs to know whether
	// the SDEK was loaded at entry so it can re-lock at the end if it
	// wasn't. Composite-readiness (lifecycle) would conflate the two
	// directions of the recovery flow.
	wasLocked := !s.cryptoManager.SDEKLoaded()
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
		// Reset-password operates BELOW the lifecycle layer. The transient
		// unlock-rewrap-relock dance is an internal key rotation, not a
		// user-facing unlock attempt — externally, the device must remain
		// "locked" from start to end (the user is on the recovery-key
		// modal, not transitioning to the desktop). The lifecycle stays
		// in StateLocked throughout; composite-readiness consumers
		// (requireUnlocked middleware, etc.) correctly continue to block
		// state-changing operations because IsReady() stays false.
		//
		// Narrow-semantics consumers (cryptoManager.SDEKLoaded) DO observe
		// the SDEK toggle — that's the truth they're asking for.
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
	// Two paths (codex P2 #2):
	// - wasLocked=false: async via the reconciler — handler returns
	// sub-second, reconciler drains in the background.
	// - wasLocked=true: SYNC via legacy ProvisionLUKSKeyslotSync — the
	// deferred relock below would otherwise race the reconciler
	// (SDEKLoaded flips false mid-pass, pass aborts, slot-1 LUKS
	// stays at OLD password until next unlock+nudge). For
	// locked-recovery the operator is already in "this will take a
	// minute" mode; pay the N×Argon2id cost to keep the rotation
	// atomic with the password change as the legacy code did.
	if wasLocked {
		if kp, ok := s.persistence.(persistence.KeyslotProvisioner); ok {
			passBytes := []byte(newPassword)
			// Read back the committed admin password_hash to fingerprint
			// (matches livePasswordKeyID's read-back pattern). The sync
			// provisioner then stamps each volume's PasswordKeyslotKeyID
			// = fingerprint so the async reconciler doesn't redundantly
			// re-provision later (codex iter-3 P2).
			stampID := ""
			if u, err := s.persistence.Control().Users().GetByUsername(ctx, "admin"); err == nil && u.PasswordHash != "" {
				stampID = crypt.FingerprintPasswordHash(u.PasswordHash)
			}
			if err := kp.ProvisionLUKSKeyslotSync(ctx, 1, passBytes, stampID); err != nil {
				log.Printf("WARN: locked-reset slot-1 sync provision: %v", err)
			}
			cryptoutil.SecureZero(passBytes)
		}
	} else {
		s.handoffSlot1ToReconciler(ctx, newPassword)
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

	// RFC 20260510 D11 blob-write-first ordering: the slot-2 pending-blob
	// is written under the crypt write mutex BEFORE keyset.json is
	// committed. If the blob write fails, keyset.json is NOT updated and
	// the operator's prior recovery key remains authoritative — they
	// retry, no silent rotation hostility. Reconciler nudge fires after
	// the commit so the slot-2 provisioning across volumes runs async.
	//
	// The hook MUST NOT call back into cryptoManager methods that take
	// the mutex — caller is inside its write lock; that would deadlock.
	// Hook receives the SDEK by parameter and uses WriteKeyslotBlobWithKey
	// to encrypt without re-locking.
	kp, _ := s.persistence.(persistence.KeyslotProvisioner)
	prepareHook := func(words []string, keyID string, sdek []byte) error {
		if kp == nil {
			return nil
		}
		blobCtx, blobCancel := s.opContext(c, 30*time.Second)
		defer blobCancel()
		mnemonic := strings.Join(words, " ")
		mnemonicBytes := []byte(mnemonic)
		defer cryptoutil.SecureZero(mnemonicBytes)
		err := kp.WriteKeyslotBlobWithKey(blobCtx, sdek, persistence.KeyslotRecovery, keyID, mnemonicBytes)
		// Non-LUKS configurations (test stubs, future non-LUKS volume
		// managers) surface ErrNotImplemented from the Module method —
		// not an error here, just "no async path needed." Without this
		// translation the hook would propagate the error and abort
		// keyset.json commit, breaking /generate on non-LUKS installs
		// (codex P2 #1).
		if errors.Is(err, persistence.ErrNotImplemented) {
			return nil
		}
		return err
	}

	// Narrow SDEK check: routes between the unlocked-path generator (uses
	// the in-memory SDEK to wrap the recovery key) and the password-path
	// generator. Composite readiness is the wrong question here — what
	// matters is whether the SDEK is currently available in this process.
	if s.cryptoManager.SDEKLoaded() {
		words, keyID, err = s.cryptoManager.GenerateRecoveryKeyWithHook(rotating, prepareHook)
	} else if strings.TrimSpace(body.Password) != "" {
		// Locked-rotation path: control-plane mount is not available,
		// so the blob would land on the un-mounted rootfs and be hidden
		// after unlock (codex P2 #3). Skip the async hand-off entirely
		// — keyset.json + paper words still rotate (online recovery
		// works), LUKS slot-2 stays at the prior generation until the
		// next unlocked rotation. Matches pre-RFC locked-path semantics
		// of "LUKS keyslot 2 provisioning deferred (system locked)".
		log.Printf("INFO: locked-path /generate: skipping async slot-2 blob (control plane unmounted)")
		words, keyID, err = s.cryptoManager.GenerateRecoveryKeyWithPasswordHook(body.Password, rotating, nil)
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unlock required"})
		return
	}
	if err != nil {
		// Blob-write failure surfaces here (wrapped by the hook). Operator
		// retries; their prior recovery state is unchanged.
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Nudge the async reconciler. Non-blocking. The reconciler walks all
	// v3 volumes and rotates slot 2 to the new mnemonic in the background;
	// the handler returns sub-second regardless of volume count, closing
	// the regenerate-loop's "120 s timeout before ack screen renders" bug
	// on the RPi 400.
	if s.keyslotReconciler != nil {
		s.keyslotReconciler.Nudge()
	}

	ctx, cancel := s.opContext(c, 30*time.Second)
	defer cancel()
	if err := s.applyStalenessUpdate(ctx, persistence.AuthStalenessUpdate{
		RecoveryStale:   boolPtr(false),
		RecoveryStaleAt: timePtr(time.Time{}),
		RecoveryAckAt:   timePtr(time.Time{}),
	}); err != nil {
		log.Printf("WARN: failed to clear recovery staleness: %v", err)
	}
	// silentRotation: true when this generation replaced an unack'd previous
	// key without the operator explicitly requesting rotation. Distinct from
	// the audit log's `rotated` (which records the mechanical fact that a
	// prior key existed): force_rotate=true is a rotation in the audit sense
	// but NOT silent — the operator triggered it, the Settings UX owns the
	// "you just rotated" affordance, and a second banner here would only
	// confuse. The wire field is named `rotated` for backwards-compatibility
	// with the older response shape; UI-side it's parsed as wasRotated.
	silentRotation := rotating && !body.ForceRotate
	s.publishAuditEvent(c, "auth.recovery_key_generate", map[string]any{"rotated": rotating})
	// REC-4: keyID is paired with words by GenerateRecoveryKey under its
	// write lock. Do not re-derive from RecoveryKeyID() here — a concurrent
	// /generate from another tab can rotate past us between lock release
	// and the read, returning words from generation A but key_id of
	// generation B. The /ack would then accept the wrong-words tab's id.
	c.JSON(http.StatusOK, gin.H{"words": words, "key_id": keyID, "rotated": silentRotation})
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
