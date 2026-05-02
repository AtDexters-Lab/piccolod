package server

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

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

// handleBoot: GET /api/v1/system/boot
//
// Returns a single authoritative routing directive for the UI. Replaces the
// frontend's previous pattern of calling 4-6 separate status endpoints and
// independently interpreting the results. The priority cascade mirrors the
// system's boot sequence: emergency → onboarding → crypto → auth → desktop.
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

	// 6. Crypto not initialized — first-time setup.
	//    (Soft emergency falls through here — user can still unlock/login.)
	cryptoInit := s.cryptoManager != nil && s.cryptoManager.IsInitialized()

	recoveryKeyPending := s.computeRecoveryKeyPending(c.Request.Context())

	if !cryptoInit {
		c.JSON(http.StatusOK, s.bootSetupResponse())
		return
	}

	// 7. Crypto locked — need password to unlock.
	if s.cryptoManager.IsLocked() {
		resp := gin.H{"screen": bootScreenUnlock}
		if sip := s.isSetupInProgress(); sip {
			resp["setup_in_progress"] = sip
		}
		if recoveryKeyPending {
			resp["recovery_key_pending"] = true
		}
		// auto_unlock_in_flight drives the UI's transient
		// "Auto-unlocking…" state on the unlock screen. Pre-auth (no
		// session needed) — the orchestrator only flips this to true
		// when state.Enabled is already on, so opting out leaves the
		// field absent / false.
		if s.autounlockOrch != nil && s.autounlockOrch.InFlight() {
			resp["auto_unlock_in_flight"] = true
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	// 7b. Crypto unlocked but setup incomplete — route back to setup screen.
	// Catches daemon restart mid-setup (crypto stays unlocked, no admin user).
	// Fail toward setup when completeness is indeterminate (SQLite error) —
	// an already-complete device just triggers a benign 409 setup_complete
	// from POST /crypto/setup, which the UI handles gracefully. Falling
	// through to login on a device with no users would leave the user stuck.
	setupComplete, setupErr := s.isSetupComplete(c.Request.Context())
	if setupErr != nil {
		log.Printf("WARN: boot setup-complete check failed, routing to setup: %v", setupErr)
	}
	if setupErr != nil || !setupComplete {
		c.JSON(http.StatusOK, s.bootSetupResponse())
		return
	}

	// 8. No valid session — need login.
	sess := s.getSessionFromContext(c)
	if sess == nil {
		resp := gin.H{"screen": bootScreenLogin}
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
		resp := gin.H{"screen": bootScreenPasskeyRequired}
		if recoveryKeyPending {
			resp["recovery_key_pending"] = true
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	presence := s.computePasskeyPresence(c, sess.UserID, s.getRPID(c))

	resp := gin.H{
		"screen":                bootScreenDesktop,
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
// setup-in-progress state and namek enrollment info. Used by both step 6
// (crypto not initialized) and step 7b (crypto unlocked, setup incomplete).
//
// Derives namek_enrolled from SetupStatus() so both signals stay coherent:
// a device flipped from Active→Suspended at runtime correctly reports
// namek_enrolled: false even if the internal enrolled flag is still true,
// eliminating contract drift between the two fields.
func (s *GinServer) bootSetupResponse() gin.H {
	resp := gin.H{"screen": bootScreenSetup, "setup_in_progress": s.isSetupInProgress()}
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
