package server

import (
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
	if !cryptoInit {
		resp := gin.H{"screen": bootScreenSetup}
		if s.identityService != nil {
			enrolled := s.identityService.IsEnrolled()
			resp["namek_enrolled"] = enrolled
			if enrolled {
				cfg := s.identityService.DeviceConfig()
				resp["namek_base_domain"] = cfg.BaseDomain
			}
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	// 7. Crypto locked — need password to unlock.
	if s.cryptoManager.IsLocked() {
		c.JSON(http.StatusOK, gin.H{"screen": bootScreenUnlock})
		return
	}

	// 8. No valid session — need login.
	sess := s.getSessionFromContext(c)
	if sess == nil {
		c.JSON(http.StatusOK, gin.H{"screen": bootScreenLogin})
		return
	}

	// 9. Passkey registration required — block dashboard until registered.
	if sess.MustRegisterPasskey {
		c.JSON(http.StatusOK, gin.H{"screen": bootScreenPasskeyRequired})
		return
	}

	// 10. All clear — desktop.
	hasPasskey := false
	if s.webauthnMgr != nil && sess.UserID != "" {
		rpID := s.getRPID(c)
		if _, rpCount, err := s.webauthnMgr.CountByUserSplitRP(c.Request.Context(), sess.UserID, rpID); err == nil {
			hasPasskey = rpCount > 0
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"screen":                bootScreenDesktop,
		"user":                  sess.User,
		"has_passkey":           hasPasskey,
		"must_register_passkey": sess.MustRegisterPasskey,
	})
}
