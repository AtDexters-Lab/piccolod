package server

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"piccolod/internal/events"
	"piccolod/internal/onboarding"
	"piccolod/internal/storage"
)

// handleOnboardingStatus returns the current onboarding state.
// GET /api/v1/system/onboarding
func (s *GinServer) handleOnboardingStatus(c *gin.Context) {
	if s.onboardingMgr == nil {
		c.JSON(http.StatusOK, gin.H{
			"state":        "complete",
			"boot_mode":    "internal",
			"required":     false,
			"install_done": false,
		})
		return
	}
	resp := s.onboardingMgr.StatusResponse()
	if s.installer != nil {
		if taskID := s.installer.ActiveTaskID(); taskID != "" {
			resp["install_task_id"] = taskID
		}
	}
	c.JSON(http.StatusOK, resp)
}

// handleOnboardingChoice processes the user's onboarding choice.
// POST /api/v1/system/onboarding
func (s *GinServer) handleOnboardingChoice(c *gin.Context) {
	if s.onboardingMgr == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "onboarding not available"})
		return
	}

	var req struct {
		Choice string `json:"choice"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Internal boots never go through onboarding — reject to prevent double
	// partitioning and to keep the API surface small.
	if s.onboardingMgr.BootMode() == storage.BootModeInternal {
		c.JSON(http.StatusBadRequest, gin.H{"error": "onboarding not applicable for internal boot"})
		return
	}

	choice := onboarding.OnboardingState(req.Choice)
	switch choice {
	case onboarding.StateTryPiccolo:
		if err := s.onboardingMgr.Choose(choice); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		// Start disk preparation for USB "Try Piccolo".
		if s.storageMgr != nil {
			s.storageMgr.StartPartitioningAsync(context.Background())
		}
		s.publishOnboardingEvent(string(onboarding.StatePending), string(choice))
		c.JSON(http.StatusOK, gin.H{
			"state":   string(choice),
			"message": "Disk preparation started",
		})

	case onboarding.StateInstallDisk:
		if err := s.onboardingMgr.Choose(choice); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		s.publishOnboardingEvent(string(onboarding.StatePending), string(choice))
		c.JSON(http.StatusOK, gin.H{
			"state": string(choice),
		})

	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid choice: must be try_piccolo or install_disk"})
	}
}

// handleOnboardingDisks lists internal (non-boot, non-USB) disks.
// GET /api/v1/storage/disks
func (s *GinServer) handleOnboardingDisks(c *gin.Context) {
	disks, err := onboarding.DiscoverInternalDisks(c.Request.Context(), s.execRunner)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "disk discovery failed: " + err.Error()})
		return
	}
	if disks == nil {
		disks = []onboarding.DiskInfo{}
	}
	c.JSON(http.StatusOK, gin.H{"disks": disks})
}

// handleInstallToDisk starts the Install to Disk pipeline.
// POST /api/v1/system/install-to-disk
func (s *GinServer) handleInstallToDisk(c *gin.Context) {
	if s.onboardingMgr == nil || s.installer == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "install not available"})
		return
	}

	// Conditional auth: during USB onboarding (pending/install_disk), no auth required.
	// Internal boots and Settings paths (try_piccolo/complete) always require admin auth.
	// Note: BootModeUnknown is intentionally treated like USB — it only occurs on
	// genuinely unconfigured systems. A configured system that fails boot detection
	// would have state=try_piccolo/complete (persisted), which requires auth.
	state := s.onboardingMgr.State()
	if s.onboardingMgr.BootMode() == storage.BootModeInternal ||
		state == onboarding.StateTryPiccolo || state == onboarding.StateComplete {
		if !s.isSessionAdmin(c) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "admin authentication required"})
			return
		}
	}

	var req struct {
		TargetDisk      string `json:"target_disk"`
		ConfirmDataLoss bool   `json:"confirm_data_loss"`
		TaskID          string `json:"task_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if !req.ConfirmDataLoss {
		c.JSON(http.StatusBadRequest, gin.H{"error": "confirm_data_loss must be true"})
		return
	}
	if req.TargetDisk == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target_disk is required"})
		return
	}
	if req.TaskID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "task_id is required"})
		return
	}
	if !strings.HasPrefix(req.TaskID, "install-") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "task_id must start with install-"})
		return
	}

	// Transition to install_disk if not already there (covers both onboarding and Settings paths).
	// Choose() resets install flags as part of the transition.
	if state != onboarding.StateInstallDisk {
		if err := s.onboardingMgr.Choose(onboarding.StateInstallDisk); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
	}

	// Best-effort PCV publish before install (preserves state for carry-over).
	if s.pcvPublisher != nil {
		go s.pcvPublisher.PublishNow(context.Background())
	}

	// Start install in background.
	if err := s.installer.Install(context.Background(), req.TargetDisk, "", req.TaskID); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}

	// On retry (already in install_disk), reset stale flags from the previous
	// attempt. Done after Install() succeeds to avoid clearing flags of an
	// active install when a duplicate request is rejected.
	if state == onboarding.StateInstallDisk {
		if err := s.onboardingMgr.ResetInstallFlags(); err != nil {
			log.Printf("WARN: failed to reset install flags on retry: %v", err)
		}
	}

	c.JSON(http.StatusAccepted, gin.H{
		"task_id": req.TaskID,
		"message": "Install started",
	})
}

// handleInstallProgressStream is the unauthenticated WebSocket progress endpoint
// scoped to install-* task IDs.
// GET /api/v1/system/install-progress/stream
func (s *GinServer) handleInstallProgressStream(c *gin.Context) {
	taskID := strings.TrimSpace(c.Query("task_id"))
	if taskID == "" || !strings.HasPrefix(taskID, "install-") {
		writeGinError(c, http.StatusBadRequest, "task_id must start with install-")
		return
	}
	// Reuse the existing progress stream handler by setting the query param.
	s.handleGinTaskProgressStream(c)
}

// handleOnboardingReboot triggers a system reboot (or power off) after Install to Disk.
// If efibootmgr configured the boot order, reboot is safe (internal disk boots first).
// Otherwise, power off so the user can remove the USB drive before powering on.
// POST /api/v1/system/reboot
func (s *GinServer) handleOnboardingReboot(c *gin.Context) {
	if s.onboardingMgr == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "onboarding not available"})
		return
	}

	state := s.onboardingMgr.State()
	if state != onboarding.StateInstallDisk || !s.onboardingMgr.InstallDone() {
		c.JSON(http.StatusConflict, gin.H{
			"error": "reboot only available after install completes",
		})
		return
	}

	if s.updateManager == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "reboot service unavailable"})
		return
	}

	if s.onboardingMgr.BootOrderConfigured() {
		if err := s.updateManager.Reboot(context.Background()); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "reboot failed: " + err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "reboot initiated"})
	} else {
		if err := s.updateManager.PowerOff(context.Background()); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "power off failed: " + err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "power off initiated"})
	}
}

// isSessionAdmin checks if the current request has an active admin session.
func (s *GinServer) isSessionAdmin(c *gin.Context) bool {
	sess := s.getSessionFromContext(c)
	if sess == nil {
		return false
	}
	return sess.Role == "admin"
}

// publishOnboardingEvent emits an onboarding state change event.
func (s *GinServer) publishOnboardingEvent(prev, current string) {
	if s.events == nil || s.onboardingMgr == nil {
		return
	}
	s.events.Publish(events.Event{
		Topic: events.TopicOnboardingStateChanged,
		Payload: events.OnboardingStateChangedEvent{
			State:         current,
			PreviousState: prev,
			BootMode:      string(s.onboardingMgr.BootMode()),
		},
	})
}
