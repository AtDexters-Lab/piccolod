package server

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"piccolod/internal/app"
)

func (s *GinServer) handleGinCapabilityList(c *gin.Context) {
	status, err := s.appManager.ListCapabilities(c.Request.Context())
	if err != nil {
		if !handleAppManagerError(c, err, "list capabilities") {
			writeGinError(c, http.StatusInternalServerError, "Failed to list capabilities: "+err.Error())
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"capabilities": status})
}

func (s *GinServer) handleGinCapabilityDefault(c *gin.Context) {
	var request struct {
		AppInstance               string `json:"app_instance" binding:"required"`
		AcknowledgeProviderChange bool   `json:"acknowledge_provider_change"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		writeGinError(c, http.StatusBadRequest, "app_instance is required")
		return
	}
	selectionCtx, cancel := s.opContext(c, app.ArtifactOperationTimeout)
	defer cancel()
	if err := s.appManager.SelectCapabilityProviderAcknowledged(
		selectionCtx,
		strings.TrimSpace(c.Param("capability")),
		strings.TrimSpace(request.AppInstance),
		request.AcknowledgeProviderChange,
	); err != nil {
		var pending *app.CapabilitySelectionReconcilePendingError
		if errors.As(err, &pending) {
			log.Printf("WARN: capability default committed with runtime repair pending: %v", err)
			c.Status(http.StatusAccepted)
			return
		}
		var confirmationRequired *app.CapabilityProviderChangeConfirmationRequiredError
		if errors.As(err, &confirmationRequired) {
			writeGinError(c, http.StatusConflict, err.Error())
			return
		}
		if !handleAppManagerError(c, err, "select capability provider") {
			writeGinError(c, http.StatusBadRequest, err.Error())
		}
		return
	}
	c.Status(http.StatusNoContent)
}
