package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"piccolod/internal/events"
	"piccolod/internal/health"
	"piccolod/internal/storage"
)

// handleEmergencyStatus: GET /api/v1/system/emergency
func (s *GinServer) handleEmergencyStatus(c *gin.Context) {
	if s.storageMgr == nil || !s.storageMgr.IsEmergencyMode() {
		c.JSON(http.StatusOK, gin.H{
			"emergency": false,
		})
		return
	}

	errMsg := ""
	if err := s.storageMgr.EmergencyError(); err != nil {
		errMsg = err.Error()
	}
	c.JSON(http.StatusOK, gin.H{
		"emergency": true,
		"level":     string(s.storageMgr.GetEmergencyLevel()),
		"error":     errMsg,
	})
}

// emergencyMiddleware blocks API requests when storage is in emergency mode.
//
// Hard emergency (missing core subvolume, first-boot failure):
//
//	Only health, emergency, and portal (static assets) endpoints pass through.
//
// Soft emergency (transient failure on previously-set-up device):
//
//	Also allows crypto/unlock so the user can attempt recovery.
func (s *GinServer) emergencyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.storageMgr == nil || !s.storageMgr.IsEmergencyMode() {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		level := s.storageMgr.GetEmergencyLevel()

		// Always allow health, emergency, and version endpoints.
		if isEmergencyAllowed(path) {
			c.Next()
			return
		}

		// Soft emergency: also allow crypto endpoints (unlock, status, setup).
		if level == storage.EmergencySoft && isEmergencySoftAllowed(path) {
			c.Next()
			return
		}

		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":     "storage emergency",
			"level":     string(level),
			"message":   "Device is in storage emergency mode. Limited API access available.",
		})
		c.Abort()
	}
}

// isEmergencyAllowed returns true for endpoints that are always accessible
// regardless of emergency level. Uses prefix matching for directory-like paths
// (trailing slash) and exact matching otherwise.
func isEmergencyAllowed(path string) bool {
	allowed := []string{
		"/api/v1/health/",
		"/api/v1/system/emergency",
		"/api/v1/system/boot",
		"/api/v1/system/diagnostic-log",
		"/api/v1/system/admin/diagnostic-log",
		"/api/v1/system/ca.crt",
		// Auth state endpoints — needed by the portal UI to render the unlock page.
		"/api/v1/auth/session",
		"/api/v1/auth/initialized",
		"/api/v1/auth/login",
		"/api/v1/crypto/status",
		// Network peers — gateway UI needs peer discovery to redirect to the correct device.
		"/api/v1/network/peers",
		// Onboarding endpoints — needed to make the initial USB boot choice.
		"/api/v1/system/onboarding",
		"/api/v1/storage/disks",
		"/version",
	}
	for _, prefix := range allowed {
		if strings.HasPrefix(path, prefix) || path == prefix {
			return true
		}
	}
	// Static assets (portal UI) are always served.
	return !strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/oauth/")
}

// isEmergencySoftAllowed returns true for endpoints additionally permitted
// during soft emergency. Crypto unlock, recovery, and setup are allowed
// because setup is idempotent (uses UnlockDataVolume which handles existing
// volumes gracefully) and is needed for partial-setup recovery after reboot.
func isEmergencySoftAllowed(path string) bool {
	allowed := []string{
		"/api/v1/crypto/unlock",
		"/api/v1/crypto/setup",
		"/api/v1/crypto/reset-password",
		"/api/v1/crypto/recovery-key",
	}
	for _, a := range allowed {
		if path == a {
			return true
		}
	}
	return false
}

// observeStorageEvents subscribes to storage lifecycle events and updates health.
func (s *GinServer) observeStorageEvents(bus *events.Bus) {
	if bus == nil || s.healthTracker == nil {
		return
	}

	phase1Ch := bus.Subscribe(events.TopicStoragePhase1Complete, 4)
	emergCh := bus.Subscribe(events.TopicStorageEmergency, 4)
	initCh := bus.Subscribe(events.TopicStoragePoolInitialized, 4)
	unlockCh := bus.Subscribe(events.TopicStoragePoolActivated, 4)
	lockCh := bus.Subscribe(events.TopicStorageLocked, 4)

	go func() {
		for evt := range phase1Ch {
			payload, ok := evt.Payload.(events.StoragePhase1Complete)
			if !ok {
				continue
			}
			if payload.Success {
				s.healthTracker.Setf("storage", health.LevelOK, "disk preparation complete")
			}
		}
	}()
	go func() {
		for evt := range emergCh {
			payload, ok := evt.Payload.(events.StorageEmergencyEvent)
			if !ok {
				continue
			}
			s.healthTracker.Setf("storage", health.LevelError, "emergency ("+payload.Level+"): "+payload.Error)
		}
	}()
	go func() {
		for range initCh {
			s.healthTracker.Setf("storage", health.LevelOK, "storage initialized")
		}
	}()
	go func() {
		for range unlockCh {
			s.healthTracker.Setf("storage", health.LevelOK, "storage activated")
		}
	}()
	go func() {
		for range lockCh {
			s.healthTracker.Setf("storage", health.LevelWarn, "storage locked")
		}
	}()
}
