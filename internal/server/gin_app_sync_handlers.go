package server

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"piccolod/internal/app"
)

const syncTriggerTimeout = 5 * time.Minute

// handleGinAppSyncStatus returns the catalog manifest sync state for an app.
// GET /api/v1/apps/:name/sync/status
func (s *GinServer) handleGinAppSyncStatus(c *gin.Context) {
	appName := c.Param("name")
	st, err := s.appManager.GetSyncStatus(c.Request.Context(), appName)
	if err != nil {
		if handleAppManagerError(c, err, "sync status") {
			return
		}
		writeGinError(c, http.StatusInternalServerError, err.Error())
		return
	}
	writeGinSuccess(c, st, "")
}

// handleGinAppSyncDisable opts an app out of automatic catalog manifest sync.
// POST /api/v1/apps/:name/sync/disable
func (s *GinServer) handleGinAppSyncDisable(c *gin.Context) {
	appName := c.Param("name")
	if err := s.appManager.SetSyncDisabled(c.Request.Context(), appName, true); err != nil {
		if handleAppManagerError(c, err, "disable sync") {
			return
		}
		writeGinError(c, http.StatusInternalServerError, err.Error())
		return
	}
	writeGinSuccess(c, gin.H{"sync_disabled": true}, "Catalog manifest sync disabled for "+appName)
}

// handleGinAppSyncEnable re-enables automatic catalog manifest sync. Clears
// the retry throttle so the next sync pass re-evaluates the catalog hash.
// POST /api/v1/apps/:name/sync/enable
func (s *GinServer) handleGinAppSyncEnable(c *gin.Context) {
	appName := c.Param("name")
	if err := s.appManager.SetSyncDisabled(c.Request.Context(), appName, false); err != nil {
		if handleAppManagerError(c, err, "enable sync") {
			return
		}
		writeGinError(c, http.StatusInternalServerError, err.Error())
		return
	}
	writeGinSuccess(c, gin.H{"sync_disabled": false}, "Catalog manifest sync enabled for "+appName)
}

// handleGinAppSyncTrigger forces a sync attempt synchronously, clearing the
// retry throttle first so the manual trigger always reaches the pipeline.
// POST /api/v1/apps/:name/sync/trigger
func (s *GinServer) handleGinAppSyncTrigger(c *gin.Context) {
	appName := c.Param("name")
	// A catalog candidate can introduce artifact declarations that are not
	// visible from the currently installed definition.
	ctx, cancel := s.opContext(c, app.ArtifactOperationTimeout)
	defer cancel()
	if err := s.appManager.SyncManifest(ctx, appName); err != nil {
		if handleAppManagerError(c, err, "trigger sync") {
			return
		}
		writeGinError(c, http.StatusInternalServerError, "Sync failed: "+err.Error())
		return
	}
	st, _ := s.appManager.GetSyncStatus(ctx, appName)
	writeGinSuccess(c, st, "Sync attempt completed")
}

// handleGinAppSyncRefreshContext re-snapshots the live install system context
// and triggers a sync. AR-1 mitigation for installs whose frozen
// install_state.json system context drifted (portal change, timezone change).
// POST /api/v1/apps/:name/sync/refresh-context
func (s *GinServer) handleGinAppSyncRefreshContext(c *gin.Context) {
	appName := c.Param("name")
	ctx, cancel := s.opContext(c, app.ArtifactOperationTimeout)
	defer cancel()
	if err := s.appManager.RefreshInstallSystemContext(ctx, appName); err != nil {
		if handleAppManagerError(c, err, "refresh sync context") {
			return
		}
		writeGinError(c, http.StatusInternalServerError, "Refresh failed: "+err.Error())
		return
	}
	st, _ := s.appManager.GetSyncStatus(ctx, appName)
	writeGinSuccess(c, st, "System context refreshed and sync triggered")
}
