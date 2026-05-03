package server

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"piccolod/internal/lifecycle"
	"piccolod/internal/storage"
)

// Boot screen directives returned by handleBoot.
const (
	bootScreenEmergency       = "emergency"
	bootScreenOnboarding      = "onboarding"
	bootScreenInstallProgress = "install_progress"
	bootScreenInstallComplete = "install_complete"
	bootScreenSetup           = "setup"
	bootScreenUnlock          = "unlock"
	bootScreenLogin           = "login"
	bootScreenPasskeyRequired = "passkey_required"
	bootScreenDesktop         = "desktop"
)

// wireLifecycleToken normalizes the internal lifecycle.State for the
// /system/boot wire. The token strings come from lifecycle.State.String()
// (which owns the wire contract); this helper applies two view-side
// normalizations:
//
//   - StateFailed → "locked": per the no-failure-callout UX feedback,
//     pre-auth screens never narrate that the system tried and failed.
//     Failed retains the failure reason for forensic logging, but UI sees
//     the same "locked" prompt as a fresh Locked state.
//
//   - autoUnlockInFlight bridge: while autounlock pickup is fetching the
//     escrow blob from namek and unwrapping the SDEK, the lifecycle is
//     still Locked (BeginUnlock fires inside completeUnlockChain, which
//     runs after the unwrap). During this window the UI must show the
//     spinner, not the password prompt — override to "unlocking" when
//     pickup is in flight.
func wireLifecycleToken(state lifecycle.State, autoUnlockInFlight bool) string {
	if state == lifecycle.StateLocked && autoUnlockInFlight {
		return lifecycle.StateUnlocking.String()
	}
	if state == lifecycle.StateFailed {
		return lifecycle.StateLocked.String()
	}
	return state.String()
}

// handleBoot: GET /api/v1/system/boot
//
// Returns a single authoritative routing directive for the UI. Replaces the
// frontend's previous pattern of calling 4-6 separate status endpoints and
// independently interpreting the results. The priority cascade mirrors the
// system's boot sequence: emergency → onboarding → lifecycle → auth → desktop.
//
// Routing on lifecycle state. The crypto/persistence/data-volume readiness
// signals are consolidated by the lifecycle.Coordinator into a single
// composite state. handleBoot routes on that composite state instead of
// re-deriving readiness from cryptoManager.IsLocked() + a persistence query
// — that derivation exposed a multi-second window during /crypto/unlock's
// post-decrypt chain where the layers disagreed (SDEK loaded but persistence
// still loading), causing isSetupComplete to return ErrLocked → (false, nil)
// and routing an already-provisioned device to the first-run setup wizard.
//
// Setup-vs-login decision (state==Ready). Uses provisioningState as the
// primary durable signal, with isSetupComplete as a defense-in-depth check
// to catch the narrow "MarkProvisioned wrote but admin user creation
// failed" partial-setup state. State==Ready guarantees persistence is
// loaded, so isSetupComplete is reliable here (no ErrLocked).
func (s *GinServer) handleBoot(c *gin.Context) {
	// 1. Hard emergency — device is in an irrecoverable storage state.
	//    Soft emergency falls through to crypto/auth checks so the user can
	//    still unlock and access the desktop (with degraded functionality).
	if s.storageMgr != nil && s.storageMgr.IsEmergencyMode() {
		level := s.storageMgr.GetEmergencyLevel()
		if level != storage.EmergencySoft {
			errMsg := ""
			if err := s.storageMgr.EmergencyError(); err != nil {
				errMsg = err.Error()
			}
			c.JSON(http.StatusOK, gin.H{
				"screen": bootScreenEmergency,
				"level":  string(level),
				"error":  errMsg,
			})
			return
		}
	}

	// 2. Onboarding — USB boot may require an initial choice before setup.
	if s.onboardingMgr != nil {
		ob := s.onboardingMgr.StatusResponse()

		if ob["required"] == true {
			c.JSON(http.StatusOK, gin.H{
				"screen":                bootScreenOnboarding,
				"boot_mode":             ob["boot_mode"],
				"boot_order_configured": ob["boot_order_configured"],
			})
			return
		}

		state, _ := ob["state"].(string)
		if state == "install_disk" {
			installDone, _ := ob["install_done"].(bool)

			// 3. Install complete — prompt for reboot.
			if installDone {
				c.JSON(http.StatusOK, gin.H{
					"screen":                bootScreenInstallComplete,
					"boot_order_configured": ob["boot_order_configured"],
				})
				return
			}

			// 4. Install in progress — reconnect to the progress stream.
			taskID := ""
			if s.installer != nil {
				taskID = s.installer.ActiveTaskID()
			}
			if taskID != "" {
				c.JSON(http.StatusOK, gin.H{
					"screen":          bootScreenInstallProgress,
					"install_task_id": taskID,
				})
				return
			}

			// 5. Install abandoned — no active task, not done.
			bootMode, _ := ob["boot_mode"].(string)
			if bootMode != "internal" {
				c.JSON(http.StatusOK, gin.H{
					"screen":                bootScreenOnboarding,
					"boot_mode":             bootMode,
					"boot_order_configured": ob["boot_order_configured"],
					"install_abandoned":     true,
				})
				return
			}
			// Internal boot: fall through to crypto/auth flow.
		}
	}

	recoveryKeyPending := s.computeRecoveryKeyPending(c.Request.Context())

	state := s.lifecycle.State()
	autoInFlight := s.autounlockOrch != nil && s.autounlockOrch.InFlight()
	wireToken := wireLifecycleToken(state, autoInFlight)

	// 6. Lifecycle-driven routing.
	switch state {
	case lifecycle.StateNotInitialized:
		// Fresh device — first-run setup.
		c.JSON(http.StatusOK, s.bootSetupResponse(wireToken))
		return

	case lifecycle.StateFailed:
		// In-memory failure during the current daemon run. Three sub-cases:
		//   - mid-setup failure pre-marker (provisioning marker absent):
		//     no unlock password to enter — route to setup so the wizard
		//     can resume.
		//   - mid-setup failure post-marker, pre-admin-user (marker
		//     present but isSetupComplete()==false): /crypto/setup wrote
		//     the marker at step 5c then failed before admin-user
		//     creation. Routing to unlock would dead-end (no admin to
		//     log in as); route to setup so the wizard can complete the
		//     missing step.
		//   - post-unlock failure on a fully provisioned device (e.g.,
		//     LUKS data volume failed): user's unlock password is
		//     meaningful — route to unlock (per Failed→"locked" wire
		//     normalization).
		// Note: Failed only persists across the current daemon run; on
		// daemon restart, an in-flight Failed transitions to Locked via
		// the cryptoManager.IsInitialized() bootstrap derivation. State
		// is Failed → persistence may or may not be loaded depending on
		// where the failure landed; isSetupComplete returns ErrLocked →
		// (false, nil) when persistence is closed, which collapses
		// safely into the partial-setup branch (route to setup; the
		// wizard's idempotent guards handle the rest).
		if !s.provisioningState.IsProvisioned() {
			c.JSON(http.StatusOK, s.bootSetupResponse(wireToken))
			return
		}
		setupComplete, _ := s.isSetupComplete(c.Request.Context())
		if !setupComplete {
			c.JSON(http.StatusOK, s.bootSetupResponse(wireToken))
			return
		}
		// Provisioned device with a transient post-unlock failure:
		// route as Locked.
		fallthrough

	case lifecycle.StateLocked, lifecycle.StateUnlocking:
		// SDEK absent (Locked) or post-decrypt chain in flight
		// (Unlocking). UI shows password prompt or spinner — `wireToken`
		// disambiguates ("locked" vs "unlocking", with the autoInFlight
		// bridge for the early-pickup window).
		resp := gin.H{
			"screen":    bootScreenUnlock,
			"lifecycle": wireToken,
		}
		if sip := s.isSetupInProgress(); sip {
			resp["setup_in_progress"] = sip
		}
		if recoveryKeyPending {
			resp["recovery_key_pending"] = true
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	// 7. State == Ready. The post-decrypt chain has completed; persistence
	//    is loaded; queries against unlock-dependent subsystems are safe.

	// 7a. Setup-vs-login decision. Primary signal is the durable
	// provisioning marker (pre-unlock-readable, sticky-once-written).
	// Defense-in-depth secondary check via isSetupComplete catches the
	// narrow "MarkProvisioned ran but admin user creation didn't" partial
	// state. State==Ready means persistence is loaded, so isSetupComplete
	// is reliable here (no ErrLocked).
	if !s.provisioningState.IsProvisioned() {
		c.JSON(http.StatusOK, s.bootSetupResponse(wireToken))
		return
	}
	setupComplete, setupErr := s.isSetupComplete(c.Request.Context())
	if setupErr != nil {
		log.Printf("WARN: boot setup-complete check failed (state=%s, marker=true): %v", state, setupErr)
	}
	if setupErr == nil && !setupComplete {
		// Marker present but no users — partial-setup state. Route to setup
		// so the wizard can complete the missing step. (When setupErr != nil
		// we trust the marker; a transient persistence error doesn't change
		// the answer for an already-provisioned device.)
		c.JSON(http.StatusOK, s.bootSetupResponse(wireToken))
		return
	}

	// 8. No valid session — need login.
	sess := s.getSessionFromContext(c)
	if sess == nil {
		resp := gin.H{
			"screen":    bootScreenLogin,
			"lifecycle": wireToken,
		}
		if sip := s.isSetupInProgress(); sip {
			resp["setup_in_progress"] = sip
		}
		if recoveryKeyPending {
			resp["recovery_key_pending"] = true
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	// 9. Passkey registration required — block dashboard until registered.
	if sess.MustRegisterPasskey.Load() {
		resp := gin.H{
			"screen":    bootScreenPasskeyRequired,
			"lifecycle": wireToken,
		}
		if recoveryKeyPending {
			resp["recovery_key_pending"] = true
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	presence := s.computePasskeyPresence(c, sess.UserID, s.getRPID(c))

	resp := gin.H{
		"screen":                bootScreenDesktop,
		"lifecycle":             wireToken,
		"user":                  sess.User,
		"has_passkey":           presence.HasPasskey,
		"must_register_passkey": sess.MustRegisterPasskey.Load(),
	}
	if recoveryKeyPending {
		resp["recovery_key_pending"] = true
	}
	c.JSON(http.StatusOK, resp)
}

// bootSetupResponse builds the JSON response for the setup screen, including
// setup-in-progress state, namek enrollment info, and the lifecycle wire
// token. Used by step 6 (NotInitialized) and step 7a (Ready but
// provisioning marker missing or admin user not created).
//
// Derives namek_enrolled from SetupStatus() so both signals stay coherent:
// a device flipped from Active→Suspended at runtime correctly reports
// namek_enrolled: false even if the internal enrolled flag is still true,
// eliminating contract drift between the two fields.
func (s *GinServer) bootSetupResponse(wireToken string) gin.H {
	resp := gin.H{
		"screen":            bootScreenSetup,
		"lifecycle":         wireToken,
		"setup_in_progress": s.isSetupInProgress(),
	}
	if s.identityService != nil {
		status := s.identityService.SetupStatus()
		enrolled := status == "enrolled"
		resp["namek_enrolled"] = enrolled
		resp["namek_status"] = status
		if enrolled {
			cfg := s.identityService.DeviceConfig()
			resp["namek_base_domain"] = cfg.BaseDomain
			if sh := s.identityService.SuggestedHostname(); sh != "" {
				resp["namek_suggested_hostname"] = sh
			}
		}
	}
	return resp
}
