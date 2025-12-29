package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"piccolod/internal/api"
	"piccolod/internal/app"
	"piccolod/internal/app/catalog"
	"piccolod/internal/remote"
	"piccolod/internal/services"
)

func determineScheme(flow api.ListenerFlow, protocol api.ListenerProtocol) string {
	switch protocol {
	case api.ListenerProtocolHTTP:
		if flow == api.FlowTLS {
			return "https"
		}
		return "http"
	case api.ListenerProtocolWebsocket:
		if flow == api.FlowTLS {
			return "wss"
		}
		return "ws"
	default:
		if flow == api.FlowTLS {
			return "https"
		}
		return "http"
	}
}

func (s *GinServer) queueAppRemoteCertificates(appName string) {
	if s == nil || s.remoteManager == nil || s.serviceManager == nil {
		return
	}
	status := s.remoteManager.Status()
	if !status.Enabled {
		return
	}
	if !strings.EqualFold(status.Solver, "http-01") {
		return
	}
	tld := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(status.TLD)), ".")
	if tld == "" {
		return
	}
	endpoints, err := s.serviceManager.GetByApp(appName)
	if err != nil {
		log.Printf("WARN: remote: queue certificates for app %s: %v", appName, err)
		return
	}
	hosts := map[string]struct{}{}
	for _, ep := range endpoints {
		if ep.Flow == api.FlowTLS {
			continue
		}
		switch ep.Protocol {
		case api.ListenerProtocolHTTP, api.ListenerProtocolWebsocket:
			// allowed
		default:
			continue
		}
		name := strings.ToLower(strings.TrimSpace(ep.Name))
		if name == "" {
			continue
		}
		if !isValidDNSLabel(name) {
			log.Printf("WARN: remote: skipping remote certificate queue for listener %q on app %q (not DNS-safe)", ep.Name, appName)
			continue
		}
		host := name + "." + tld
		hosts[host] = struct{}{}
	}
	for h := range hosts {
		s.remoteManager.QueueHostnameCertificate(h)
	}
}

func remoteHostsForEndpoints(endpoints []services.ServiceEndpoint, tld string) map[string]struct{} {
	hosts := map[string]struct{}{}
	tld = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(tld)), ".")
	if tld == "" {
		return hosts
	}
	for _, ep := range endpoints {
		if ep.Flow == api.FlowTLS {
			continue
		}
		switch ep.Protocol {
		case api.ListenerProtocolHTTP, api.ListenerProtocolWebsocket:
		default:
			continue
		}
		name := strings.ToLower(strings.TrimSpace(ep.Name))
		if name == "" || !isValidDNSLabel(name) {
			continue
		}
		host := name + "." + tld
		hosts[host] = struct{}{}
	}
	return hosts
}

func isValidDNSLabel(label string) bool {
	if label == "" || len(label) > 63 {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		ch := label[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
			continue
		}
		return false
	}
	return true
}

// APIError represents a structured API error response
type APIError struct {
	Error   string `json:"error"`
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

// GinAppResponse represents the standardized API response format
type GinAppResponse struct {
	Data    interface{} `json:"data,omitempty"`
	Message string      `json:"message,omitempty"`
	Error   *APIError   `json:"error,omitempty"`
}

// writeGinError writes a structured error response using Gin
func writeGinError(c *gin.Context, statusCode int, message string) {
	response := GinAppResponse{
		Error: &APIError{
			Error:   http.StatusText(statusCode),
			Code:    statusCode,
			Message: message,
		},
	}
	c.JSON(statusCode, response)
}

// writeGinSuccess writes a successful response using Gin
func writeGinSuccess(c *gin.Context, data interface{}, message string) {
	response := GinAppResponse{
		Data:    data,
		Message: message,
	}
	c.JSON(http.StatusOK, response)
}

// handleGinAppValidate handles POST /api/v1/apps/validate - Validate app.yaml without installing
func (s *GinServer) handleGinAppValidate(c *gin.Context) {
	contentType := c.GetHeader("Content-Type")
	if !strings.Contains(contentType, "application/x-yaml") && !strings.Contains(contentType, "text/yaml") && !strings.Contains(contentType, "application/json") {
		writeGinError(c, http.StatusUnsupportedMediaType, "Content-Type must be application/x-yaml or text/yaml or application/json")
		return
	}
	var yamlData []byte
	if strings.Contains(contentType, "application/json") {
		// Accept { app_definition: "...yaml..." }
		var req struct {
			AppDefinition string `json:"app_definition"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.AppDefinition) == "" {
			writeGinError(c, http.StatusBadRequest, "Invalid JSON body; expected {app_definition}")
			return
		}
		yamlData = []byte(req.AppDefinition)
	} else {
		body, err := c.GetRawData()
		if err != nil || len(body) == 0 {
			writeGinError(c, http.StatusBadRequest, "Request body cannot be empty")
			return
		}
		yamlData = body
	}
	if _, err := app.ParseAppDefinition(yamlData); err != nil {
		writeGinError(c, http.StatusBadRequest, "Invalid app.yaml: "+err.Error())
		return
	}
	writeGinSuccess(c, gin.H{"valid": true}, "valid")
}

// handleGinCatalogTemplate handles GET /api/v1/catalog/:name/template - return YAML template for a catalog app
func (s *GinServer) handleGinCatalogTemplate(c *gin.Context) {
	if s.catalogManager == nil {
		writeGinError(c, http.StatusInternalServerError, "Catalog manager not initialized")
		return
	}
	name := c.Param("name")
	yaml, err := s.catalogManager.GetAppTemplate(c.Request.Context(), name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeGinError(c, http.StatusNotFound, "template not found")
		} else {
			writeGinError(c, http.StatusInternalServerError, "failed to fetch template: "+err.Error())
		}
		return
	}
	c.Data(http.StatusOK, "application/x-yaml; charset=utf-8", []byte(yaml))
}

// handleGinCatalogConfigure handles GET /api/v1/catalog/:name/configure - return enriched input schema for a catalog app
func (s *GinServer) handleGinCatalogConfigure(c *gin.Context) {
	if s.catalogManager == nil {
		writeGinError(c, http.StatusInternalServerError, "Catalog manager not initialized")
		return
	}
	name := c.Param("name")
	yamlContent, err := s.catalogManager.GetAppTemplate(c.Request.Context(), name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeGinError(c, http.StatusNotFound, "template not found")
		} else {
			writeGinError(c, http.StatusInternalServerError, "failed to fetch template: "+err.Error())
		}
		return
	}

	// Parse schema loose
	def, err := app.ParseAppSchema([]byte(yamlContent))
	if err != nil {
		writeGinError(c, http.StatusInternalServerError, "failed to parse app schema: "+err.Error())
		return
	}

	// Prepare smart defaults
	if err := app.PrepareSmartDefaults(c.Request.Context(), s.appManager, def); err != nil {
		log.Printf("WARN: failed to prepare smart defaults for %s: %v", name, err)
		// Continue anyway, just without smarts
	}

	writeGinSuccess(c, def.Inputs, "Configuration schema prepared")
}

// handleGinAppInstall handles POST /api/v1/apps - Install app from app.yaml upload
func (s *GinServer) handleGinAppInstall(c *gin.Context) {
	// Check Content-Type
	contentType := c.GetHeader("Content-Type")
	if !strings.Contains(contentType, "application/x-yaml") && !strings.Contains(contentType, "text/yaml") && !strings.Contains(contentType, "application/json") {
		writeGinError(c, http.StatusUnsupportedMediaType, "Content-Type must be application/x-yaml or text/yaml or application/json")
		return
	}

	// Read request body
	var yamlData []byte
	var userInputs map[string]interface{}
	displayName := ""

	if strings.Contains(contentType, "application/json") {
		// Accept { app_definition: "...yaml...", inputs: {...} }
		var req struct {
			AppDefinition string                 `json:"app_definition"`
			Inputs        map[string]interface{} `json:"inputs"`
			DisplayName   string                 `json:"display_name"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.AppDefinition) == "" {
			writeGinError(c, http.StatusBadRequest, "Invalid JSON body; expected {app_definition}")
			return
		}
		yamlData = []byte(req.AppDefinition)
		userInputs = req.Inputs
		displayName = req.DisplayName
	} else {
		body, err := c.GetRawData()
		if err != nil {
			writeGinError(c, http.StatusBadRequest, "Failed to read request body: "+err.Error())
			return
		}
		if len(body) == 0 {
			writeGinError(c, http.StatusBadRequest, "Request body cannot be empty")
			return
		}
		yamlData = body
	}

	// Construct system context
	systemContext := map[string]interface{}{
		"Domain":       "local",
		"Architecture": runtime.GOARCH,
	}
	if s.remoteManager != nil {
		status := s.remoteManager.Status()
		if status.Enabled && status.TLD != "" {
			systemContext["Domain"] = strings.TrimSuffix(status.TLD, ".")
		}
	}

	// Render template if inputs provided
	if len(userInputs) > 0 {
		rendered, err := app.RenderManifest(yamlData, userInputs, systemContext)
		if err != nil {
			writeGinError(c, http.StatusBadRequest, "Failed to render manifest template: "+err.Error())
			return
		}
		yamlData = rendered
	}

	// Parse app.yaml
	appDef, err := app.ParseAppDefinition(yamlData)
	if err != nil {
		writeGinError(c, http.StatusBadRequest, "Invalid app.yaml: "+err.Error())
		return
	}

	// Install a new app instance
	appInstance, err := s.appManager.Install(c.Request.Context(), appDef, displayName)
	if err != nil {
		if handleAppManagerError(c, err, "install app") {
			return
		}
		writeGinError(c, http.StatusInternalServerError, "Failed to install app: "+err.Error())
		return
	}

	s.queueAppRemoteCertificates(appInstance.InstanceID)

	response := GinAppResponse{
		Data:    appInstance,
		Message: "App '" + appInstance.InstanceID + "' installed successfully",
	}
	c.JSON(http.StatusCreated, response)
}

// handleGinAppList handles GET /api/v1/apps - List all apps with status
func (s *GinServer) handleGinAppList(c *gin.Context) {
	apps, err := s.appManager.List(c.Request.Context())
	if err != nil {
		if handleAppManagerError(c, err, "list apps") {
			return
		}
		writeGinError(c, http.StatusInternalServerError, "Failed to list apps: "+err.Error())
		return
	}

	writeGinSuccess(c, apps, fmt.Sprintf("Found %d apps", len(apps)))
}

// handleGinAppGet handles GET /api/v1/apps/:name - Get specific app details
func (s *GinServer) handleGinAppGet(c *gin.Context) {
	appName := c.Param("name")

	appInstance, err := s.appManager.Get(c.Request.Context(), appName)
	if err != nil {
		if handleAppManagerError(c, err, "fetch app") {
			return
		}
		if strings.Contains(err.Error(), "not found") {
			writeGinError(c, http.StatusNotFound, err.Error())
		} else {
			writeGinError(c, http.StatusInternalServerError, "Failed to get app: "+err.Error())
		}
		return
	}

	// Include services inline
	services, _ := s.serviceManager.GetByApp(appName)
	serviceStatus := make([]gin.H, 0, len(services))
	var remoteStatus *remote.Status
	if s.remoteManager != nil {
		st := s.remoteManager.Status()
		remoteStatus = &st
	}
	for _, ep := range services {
		serviceStatus = append(serviceStatus, s.formatServiceEndpoint(c, ep, remoteStatus))
	}
	writeGinSuccess(c, gin.H{"app": appInstance, "services": serviceStatus}, "")
}

// handleGinAppUpdateListeners handles PATCH /api/v1/apps/:name/listeners
func (s *GinServer) handleGinAppUpdateListeners(c *gin.Context) {
	appName := c.Param("name")

	var req struct {
		Listeners []api.AppListener `json:"listeners"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeGinError(c, http.StatusBadRequest, "Invalid request body")
		return
	}

	// Get current state to detect container recreation
	oldApp, err := s.appManager.Get(c.Request.Context(), appName)
	if err != nil {
		if handleAppManagerError(c, err, "update listeners") {
			return
		}
		writeGinError(c, http.StatusNotFound, err.Error())
		return
	}

	_, err = s.appManager.UpdateListeners(c.Request.Context(), appName, req.Listeners)
	if err != nil {
		if handleAppManagerError(c, err, "update listeners") {
			return
		}
		// Check for validation errors (e.g., from ValidateAppDefinition)
		if strings.Contains(err.Error(), "invalid listener") || strings.Contains(err.Error(), "invalid app definition") {
			writeGinError(c, http.StatusBadRequest, "Invalid listener configuration: "+err.Error())
			return
		}
		writeGinError(c, http.StatusInternalServerError, "Failed to update listeners: "+err.Error())
		return
	}

	// Queue certs for new listeners
	s.queueAppRemoteCertificates(appName)

	// Fetch updated details
	newApp, err := s.appManager.Get(c.Request.Context(), appName)
	if err != nil {
		writeGinError(c, http.StatusInternalServerError, "Updated successfully but failed to fetch fresh status")
		return
	}

	recreated := oldApp.ContainerID != newApp.ContainerID

	// Include services inline
	services, _ := s.serviceManager.GetByApp(appName)
	serviceStatus := make([]gin.H, 0, len(services))
	var remoteStatus *remote.Status
	if s.remoteManager != nil {
		st := s.remoteManager.Status()
		remoteStatus = &st
	}
	for _, ep := range services {
		serviceStatus = append(serviceStatus, s.formatServiceEndpoint(c, ep, remoteStatus))
	}

	resp := gin.H{
		"instance_id":         newApp.InstanceID,
		"status":              newApp.Status,
		"listeners_updated":   true,
		"container_recreated": recreated,
		"endpoints":           serviceStatus,
	}

	writeGinSuccess(c, resp, "Listeners updated successfully")
}

// handleGinAppUninstall handles DELETE /api/v1/apps/:name - Uninstall app completely
func (s *GinServer) handleGinAppUninstall(c *gin.Context) {
	appName := c.Param("name")
	// Optional purge=true to delete app data
	purge := false
	switch c.Query("purge") {
	case "1", "true", "yes", "on":
		purge = true
	}

	// Capture current remote hosts to clean up after uninstall.
	var hostsToRemove map[string]struct{}
	if s.remoteManager != nil && s.serviceManager != nil {
		st := s.remoteManager.Status()
		if st.TLD != "" {
			if eps, err := s.serviceManager.GetByApp(appName); err == nil {
				hostsToRemove = remoteHostsForEndpoints(eps, st.TLD)
			}
		}
	}

	err := s.appManager.UninstallWithOptions(c.Request.Context(), appName, purge)
	if err != nil {
		if handleAppManagerError(c, err, "uninstall app") {
			return
		}
		if strings.Contains(err.Error(), "not found") {
			writeGinError(c, http.StatusNotFound, err.Error())
		} else {
			writeGinError(c, http.StatusInternalServerError, "Failed to uninstall app: "+err.Error())
		}
		return
	}

	if hostsToRemove != nil && s.remoteManager != nil {
		for h := range hostsToRemove {
			s.remoteManager.RemoveHostnameCertificate(h)
		}
	}

	if purge {
		writeGinSuccess(c, nil, "App '"+appName+"' uninstalled and data purged successfully")
	} else {
		writeGinSuccess(c, nil, "App '"+appName+"' uninstalled successfully")
	}
}

// handleGinAppStart handles POST /api/v1/apps/:name/start - Start app container
func (s *GinServer) handleGinAppStart(c *gin.Context) {
	appName := c.Param("name")
	// Demo mode: simulate success without backend
	if os.Getenv("PICCOLO_DEMO") != "" {
		writeGinSuccess(c, nil, "App '"+appName+"' started successfully")
		return
	}

	err := s.appManager.Start(c.Request.Context(), appName)
	if err != nil {
		if handleAppManagerError(c, err, "start app") {
			return
		}
		if strings.Contains(err.Error(), "not found") {
			writeGinError(c, http.StatusNotFound, err.Error())
		} else {
			writeGinError(c, http.StatusInternalServerError, "Failed to start app: "+err.Error())
		}
		return
	}

	writeGinSuccess(c, nil, "App '"+appName+"' started successfully")
}

// handleGinAppStop handles POST /api/v1/apps/:name/stop - Stop app container
func (s *GinServer) handleGinAppStop(c *gin.Context) {
	appName := c.Param("name")
	// Demo mode: simulate success without backend
	if os.Getenv("PICCOLO_DEMO") != "" {
		writeGinSuccess(c, nil, "App '"+appName+"' stopped successfully")
		return
	}

	err := s.appManager.Stop(c.Request.Context(), appName)
	if err != nil {
		if handleAppManagerError(c, err, "stop app") {
			return
		}
		if strings.Contains(err.Error(), "not found") {
			writeGinError(c, http.StatusNotFound, err.Error())
		} else {
			writeGinError(c, http.StatusInternalServerError, "Failed to stop app: "+err.Error())
		}
		return
	}

	writeGinSuccess(c, nil, "App '"+appName+"' stopped successfully")
}

// handleGinCatalog handles GET /api/v1/catalog - returns curated catalog.
func (s *GinServer) handleGinCatalog(c *gin.Context) {
	if s.catalogManager == nil {
		writeGinError(c, http.StatusInternalServerError, "Catalog manager not initialized")
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	query := c.Query("q")
	category := c.Query("category")

	opts := catalog.FilterOptions{
		Query:         query,
		Category:      category,
		Page:          page,
		PageSize:      pageSize,
		SystemVersion: s.version,
	}

	resp, err := s.catalogManager.GetApps(c.Request.Context(), opts)
	if err != nil {
		writeGinError(c, http.StatusInternalServerError, "failed to list catalog apps: "+err.Error())
		return
	}

	c.JSON(http.StatusOK, resp)
}

// handleGinCatalogCategories handles GET /api/v1/catalog/categories - returns list of categories
func (s *GinServer) handleGinCatalogCategories(c *gin.Context) {
	if s.catalogManager == nil {
		writeGinError(c, http.StatusInternalServerError, "Catalog manager not initialized")
		return
	}

	cats, err := s.catalogManager.GetCategories(c.Request.Context())
	if err != nil {
		writeGinError(c, http.StatusInternalServerError, "failed to list categories: "+err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{"categories": cats})
}

func handleAppManagerError(c *gin.Context, err error, action string) bool {
	if errors.Is(err, app.ErrLocked) {
		msg := fmt.Sprintf("Unable to %s while storage is locked. Unlock Piccolo to continue.", action)
		writeGinError(c, http.StatusLocked, msg)
		return true
	}
	return false
}

// handleGinAppCheckInstance handles GET /api/v1/apps/check-instance?id=<candidate>
// It returns whether an instance ID is available and, if not, a suggested alternative.
func (s *GinServer) handleGinAppCheckInstance(c *gin.Context) {
	candidate := strings.TrimSpace(c.Query("id"))
	if candidate == "" {
		writeGinError(c, http.StatusBadRequest, "Missing query parameter: id")
		return
	}
	if err := app.ValidateInstanceID(candidate); err != nil {
		writeGinError(c, http.StatusBadRequest, "Invalid instance ID: "+err.Error())
		return
	}

	apps, err := s.appManager.List(c.Request.Context())
	if err != nil {
		if handleAppManagerError(c, err, "check instance") {
			return
		}
		writeGinError(c, http.StatusInternalServerError, "Failed to list apps: "+err.Error())
		return
	}

	existing := make([]string, 0, len(apps))
	available := true
	for _, inst := range apps {
		existing = append(existing, inst.InstanceID)
		if inst.InstanceID == candidate {
			available = false
		}
	}

	suggested := candidate
	if !available {
		if alt, err := app.GenerateInstanceID(candidate, existing); err == nil {
			suggested = alt
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"available": available,
		"suggested": suggested,
	})
}
