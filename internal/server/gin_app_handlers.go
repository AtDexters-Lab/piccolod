package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/gin-gonic/gin"
	"piccolod/internal/api"
	"piccolod/internal/app"
	"piccolod/internal/app/catalog"
	"piccolod/internal/events"
	"piccolod/internal/hostname"
	"piccolod/internal/remote"
	"piccolod/internal/services"
)

var (
	cachedHostTimezone string
	hostTimezoneOnce   sync.Once
)

// detectHostTimezone returns the host's IANA timezone (e.g. "America/New_York").
// Result is cached for the process lifetime. Falls back to "Etc/UTC" if detection fails.
func detectHostTimezone() string {
	hostTimezoneOnce.Do(func() {
		cachedHostTimezone = probeHostTimezone()
	})
	return cachedHostTimezone
}

func probeHostTimezone() string {
	// Try /etc/localtime symlink (most Linux distros)
	if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		const prefix = "/usr/share/zoneinfo/"
		if idx := strings.Index(target, prefix); idx != -1 {
			if tz := target[idx+len(prefix):]; isValidTimezone(tz) {
				return tz
			}
		}
	}
	// Try /etc/timezone (Debian/Ubuntu)
	if data, err := os.ReadFile("/etc/timezone"); err == nil {
		if tz := strings.TrimSpace(string(data)); tz != "" && isValidTimezone(tz) {
			return tz
		}
	}
	// Try TZ environment variable
	if tz := os.Getenv("TZ"); tz != "" && isValidTimezone(tz) {
		return tz
	}
	return "Etc/UTC"
}

func isValidTimezone(tz string) bool {
	_, err := time.LoadLocation(tz)
	return err == nil
}

// buildSystemContext returns the template context for {{ .System.* }} variables.
func (s *GinServer) buildSystemContext() map[string]interface{} {
	ctx := map[string]interface{}{
		"Domain":       "local",
		"Architecture": runtime.GOARCH,
		"Timezone":     detectHostTimezone(),
	}
	if s.remoteManager != nil {
		status := s.remoteManager.Status()
		if status.Enabled && strings.TrimSpace(status.PortalHostname) != "" {
			ctx["Domain"] = strings.TrimSuffix(strings.TrimSpace(status.PortalHostname), ".")
		}
	}
	return ctx
}

// resolveSystemDefaults renders {{ .System.* }} expressions in input default values
// so the UI displays concrete values instead of raw template strings.
// Only defaults containing ".System." are resolved; other template expressions
// (e.g. {{ .Inputs.* }}) are left untouched to avoid corrupting non-system defaults.
func resolveSystemDefaults(inputs map[string]api.AppInput, systemCtx map[string]interface{}) {
	data := map[string]interface{}{"System": systemCtx}
	for name, input := range inputs {
		defaultStr, ok := input.Default.(string)
		if !ok || !strings.Contains(defaultStr, ".System.") {
			continue
		}
		tmpl, err := template.New("default").Option("missingkey=error").Parse(defaultStr)
		if err != nil {
			continue
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			continue // template references non-system keys — leave as-is
		}
		input.Default = buf.String()
		inputs[name] = input
	}
}

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
	if !status.Enabled || !strings.EqualFold(status.Solver, "http-01") {
		return
	}
	base := remoteBaseHostname(&status)
	if base == "" {
		return
	}
	endpoints, err := s.serviceManager.GetByApp(appName)
	if err != nil {
		log.Printf("WARN: remote: queue certificates for app %s: %v", appName, err)
		return
	}
	queueEndpointHostCerts(s.remoteManager, endpoints, base)
}

func remoteHostsForEndpoints(endpoints []services.ServiceEndpoint, base string) map[string]struct{} {
	hosts := map[string]struct{}{}
	base = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(base)), ".")
	if base == "" {
		return hosts
	}
	for _, ep := range endpoints {
		// Only include HTTP/WS listeners that have host-based routing
		// DerivedHostLabel is empty for raw/tls listeners (per RFC 20260114)
		if ep.DerivedHostLabel == "" {
			continue
		}
		host := ep.DerivedHostLabel + "." + base
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
	Key     string `json:"key,omitempty"`
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
	writeGinErrorWithKey(c, statusCode, message, "")
}

func writeGinErrorWithKey(c *gin.Context, statusCode int, message, key string) {
	response := GinAppResponse{
		Error: &APIError{
			Error:   http.StatusText(statusCode),
			Code:    statusCode,
			Key:     key,
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
		var ve *app.ValidationError
		if errors.As(err, &ve) && ve != nil {
			writeGinErrorWithKey(c, http.StatusBadRequest, "Invalid app.yaml: "+ve.Message, ve.Code)
			return
		}
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

	// Prepare smart defaults (pass catalog item name for __app_address__ default)
	if err := app.PrepareSmartDefaults(c.Request.Context(), s.appManager, def, name); err != nil {
		log.Printf("WARN: failed to prepare smart defaults for %s: %v", name, err)
		// Continue anyway, just without smarts
	}

	// Resolve {{ .System.* }} expressions in input defaults so the UI shows
	// concrete values (e.g. "America/New_York") instead of raw template strings.
	resolveSystemDefaults(def.Inputs, s.buildSystemContext())

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

	var catalogSource string
	if strings.Contains(contentType, "application/json") {
		// Accept { app_definition: "...yaml...", inputs: {...}, catalog_source: "..." }
		var req struct {
			AppDefinition string                 `json:"app_definition"`
			Inputs        map[string]interface{} `json:"inputs"`
			CatalogSource string                 `json:"catalog_source"` // Optional: tracks which catalog item this was installed from
		}
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.AppDefinition) == "" {
			writeGinError(c, http.StatusBadRequest, "Invalid JSON body; expected {app_definition}")
			return
		}
		yamlData = []byte(req.AppDefinition)
		userInputs = req.Inputs
		catalogSource = req.CatalogSource
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

	systemContext := s.buildSystemContext()

	// Check for service-level oidc_client in loose schema to pre-generate credentials.
	// We do this before rendering so we can inject the credentials into the template.
	var oidcClientID, oidcClientSecret string
	if looseDef, err := app.ParseAppSchema(yamlData); err == nil && func() bool {
		for _, svc := range looseDef.Services {
			if svc.OIDCClient != nil {
				return true
			}
		}
		return false
	}() {
		clientMgr := s.getOIDCClientManager()
		if clientMgr != nil {
			var credErr error
			oidcClientID, oidcClientSecret, credErr = clientMgr.GenerateCredentials()
			if credErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate OIDC credentials: " + credErr.Error()})
				return
			}

			// Inject Auth context for templating (machine-specific issuer for back-channel).
			issuer := s.oidcIssuer()

			systemContext["Auth"] = map[string]string{
				"Issuer":       issuer,
				"ClientID":     oidcClientID,
				"ClientSecret": oidcClientSecret,
			}
		}
	}

	// Render template if inputs provided
	if len(userInputs) > 0 || oidcClientID != "" {
		rendered, err := app.RenderManifest(yamlData, userInputs, systemContext)
		if err != nil {
			writeGinError(c, http.StatusBadRequest, "Failed to render manifest template: "+err.Error())
			return
		}
		yamlData = rendered
	}

	// RFC 20260130: Parse YAML first, then handle __primary substitution via struct manipulation.
	// This is safer than regex replacement which could corrupt comments/descriptions/env vars.
	looseDef, err := app.ParseAppSchema(yamlData)
	if err != nil {
		writeGinError(c, http.StatusBadRequest, "Invalid app.yaml: "+err.Error())
		return
	}

	// RFC 20260130: Find __primary listener and substitute with user-provided value
	var primaryListenerName string
	hasPrimaryMarker := false
	for i := range looseDef.Listeners {
		if hostname.IsPrimaryMarker(looseDef.Listeners[i].Name) {
			hasPrimaryMarker = true
			// Get user-provided app address
			if appAddress, ok := userInputs["__app_address__"].(string); ok && strings.TrimSpace(appAddress) != "" {
				primaryListenerName = strings.TrimSpace(appAddress)
				// Validate the provided name before using it
				if err := app.ValidateInstanceID(primaryListenerName); err != nil {
					writeGinError(c, http.StatusBadRequest, "Invalid app address: "+err.Error())
					return
				}
				// RFC 20260130: Check for collision with existing app instance IDs
				existingApps, err := s.appManager.List(c.Request.Context())
				if err != nil {
					writeGinError(c, http.StatusInternalServerError, "Failed to check existing apps: "+err.Error())
					return
				}
				existingIDs := make([]string, len(existingApps))
				for i, a := range existingApps {
					existingIDs[i] = a.InstanceID
				}
				if err := app.ValidatePrimaryNameAvailable(primaryListenerName, existingIDs); err != nil {
					writeGinError(c, http.StatusConflict, err.Error())
					return
				}
				// Substitute the listener name and mark as primary
				looseDef.Listeners[i].Name = primaryListenerName
				looseDef.Listeners[i].Primary = true
			} else {
				writeGinError(c, http.StatusBadRequest, "App requires '__app_address__' input for primary listener name")
				return
			}
			break
		}
	}

	// RFC 20260130: All apps with listeners MUST use __primary marker
	if !hasPrimaryMarker && len(looseDef.Listeners) > 0 {
		writeGinError(c, http.StatusBadRequest, "Apps with listeners must have exactly one listener named '__primary'; update your app.yaml to use the __primary marker")
		return
	}

	// Workspace apps: handle workspace_name identity.
	// This block fires when the app has no listeners AND either:
	//   - the template already contains workspace_name (custom Docker Hub flow), or
	//   - the user supplied __app_address__ via inputs (catalog flow where
	//     PrepareSmartDefaults injected the synthetic input).
	appAddr, _ := userInputs["__app_address__"].(string)
	appAddr = strings.TrimSpace(appAddr)
	if len(looseDef.Listeners) == 0 && (looseDef.WorkspaceName != "" || appAddr != "") {
		wsName := looseDef.WorkspaceName // default: template's workspace_name (custom Docker Hub flow)

		// Catalog flow: substitute __app_address__ into workspace_name
		if appAddr != "" {
			wsName = appAddr
		}

		// Validate and check collision (both flows)
		if err := app.ValidateInstanceID(wsName); err != nil {
			writeGinError(c, http.StatusBadRequest, "Invalid workspace name: "+err.Error())
			return
		}
		existingApps, err := s.appManager.List(c.Request.Context())
		if err != nil {
			writeGinError(c, http.StatusInternalServerError, "Failed to check existing apps: "+err.Error())
			return
		}
		existingIDs := make([]string, len(existingApps))
		for i, a := range existingApps {
			existingIDs[i] = a.InstanceID
		}
		if err := app.ValidatePrimaryNameAvailable(wsName, existingIDs); err != nil {
			writeGinError(c, http.StatusConflict, err.Error())
			return
		}
		looseDef.WorkspaceName = wsName
	}

	// Set defaults and validate
	app.SetDefaults(looseDef)
	if err := app.ValidateAppDefinition(looseDef); err != nil {
		var ve *app.ValidationError
		if errors.As(err, &ve) && ve != nil {
			writeGinErrorWithKey(c, http.StatusBadRequest, "Invalid app.yaml: "+ve.Message, ve.Code)
			return
		}
		writeGinError(c, http.StatusBadRequest, "Invalid app.yaml: "+err.Error())
		return
	}
	appDef := looseDef

	// Install a new app instance.
	// Use a background context with a generous timeout instead of the HTTP request context.
	// The request context is canceled by the server's WriteTimeout (60s) or remote tunnel
	// disconnects, which kills podman pull processes mid-download for large images.
	// The install must survive connection drops.
	installCtx, cancelInstall := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancelInstall()
	installCtx = app.WithTaskID(installCtx, c.GetHeader("X-Piccolo-Task-ID"))
	if catalogSource != "" {
		installCtx = app.WithCatalogSource(installCtx, catalogSource)
	}
	appInstance, err := s.appManager.Install(installCtx, appDef)
	if err != nil {
		if handleAppManagerError(c, err, "install app") {
			return
		}
		writeGinError(c, http.StatusInternalServerError, "Failed to install app: "+err.Error())
		return
	}

	// Persist OIDC client if generated
	if oidcClientID != "" {
		clientMgr := s.getOIDCClientManager()
		if err := clientMgr.CreateClient(installCtx, oidcClientID, oidcClientSecret, appInstance.InstanceID); err != nil {
			log.Printf("ERROR: failed to persist OIDC client for %s: %v. Rolling back install.", appInstance.InstanceID, err)
			// Rollback: uninstall the app
			if rbErr := s.appManager.Uninstall(installCtx, appInstance.InstanceID); rbErr != nil {
				log.Printf("CRITICAL: failed to rollback uninstall for %s: %v", appInstance.InstanceID, rbErr)
			}
			writeGinError(c, http.StatusInternalServerError, "Failed to register OIDC client: "+err.Error())
			return
		}
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

	// Filter for standard users
	if sess := s.getSessionFromContext(c); sess != nil && sess.Role != "admin" {
		if s.userManager != nil {
			user, err := s.userManager.Get(c.Request.Context(), sess.UserID)
			if err != nil {
				// User not found or error, return empty list
				writeGinSuccess(c, []*AppInstanceWithHealth{}, "Found 0 apps")
				return
			}
			filtered := make([]*app.AppInstance, 0)
			for _, a := range apps {
				if isIDAllowed(user.AllowedApps, a.InstanceID) {
					filtered = append(filtered, a)
				}
			}
			apps = filtered
		}
	}

	// Wrap apps with health data (RFC 20260125 §7.2)
	result := make([]*AppInstanceWithHealth, len(apps))
	for i, inst := range apps {
		result[i] = &AppInstanceWithHealth{
			AppInstance:           inst,
			PrimaryListenerHealth: s.deriveAppHealth(inst),
		}
	}

	writeGinSuccess(c, result, fmt.Sprintf("Found %d apps", len(result)))
}

// handleGinAppGet handles GET /api/v1/apps/:name - Get specific app details
func (s *GinServer) handleGinAppGet(c *gin.Context) {
	appName := c.Param("name")

	// Check access for standard users
	if sess := s.getSessionFromContext(c); sess != nil && sess.Role != "admin" {
		allowed, err := s.userManager.IsAppAllowed(c.Request.Context(), sess.UserID, appName)
		if err != nil || !allowed {
			writeGinError(c, http.StatusForbidden, "Access denied")
			return
		}
	}

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

	// Include listener endpoints inline (keyed as "listeners" to avoid colliding with manifest services).
	listeners, _ := s.serviceManager.GetByApp(appName)
	listenerStatus := make([]gin.H, 0, len(listeners))
	var remoteStatus *remote.Status
	if s.remoteManager != nil {
		st := s.remoteManager.Status()
		remoteStatus = &st
	}
	for _, ep := range listeners {
		formatted := s.formatServiceEndpoint(c, ep, remoteStatus)
		// Add listener health status (RFC 20260125)
		formatted["health"] = s.computeListenerHealth(ep)
		listenerStatus = append(listenerStatus, formatted)
	}

	containerStatus, err := s.appManager.ContainerStatuses(c.Request.Context(), appName)
	if err != nil {
		log.Printf("WARN: app get %s: container status unavailable: %v", appName, err)
	}
	writeGinSuccess(c, gin.H{"app": appInstance, "listeners": listenerStatus, "containers": containerStatus}, "")
}

// handleGinAppLogs returns recent container logs for an app instance.
// GET /api/v1/apps/:name/logs?tail=200
func (s *GinServer) handleGinAppLogs(c *gin.Context) {
	appName := c.Param("name")

	// Check access for standard users
	if sess := s.getSessionFromContext(c); sess != nil && sess.Role != "admin" {
		allowed, err := s.userManager.IsAppAllowed(c.Request.Context(), sess.UserID, appName)
		if err != nil || !allowed {
			writeGinError(c, http.StatusForbidden, "Access denied")
			return
		}
	}

	tail := parseLogTail(c, 200)
	service := strings.TrimSpace(c.Query("service"))

	lines, err := s.appManager.LogsForService(c.Request.Context(), appName, service, tail)
	if err != nil {
		if handleAppManagerError(c, err, "fetch logs") {
			return
		}
		if strings.Contains(err.Error(), "not found") {
			writeGinError(c, http.StatusNotFound, err.Error())
			return
		}
		writeGinError(c, http.StatusInternalServerError, "Failed to fetch logs: "+err.Error())
		return
	}
	writeGinSuccess(c, gin.H{"lines": lines}, "")
}

// handleGinAppUpdateListeners handles PATCH /api/v1/apps/:name/listeners
func (s *GinServer) handleGinAppUpdateListeners(c *gin.Context) {
	appName := c.Param("name")

	// Only Admin can update listeners (enforced by middleware), but double check if needed.
	// Since we moved this to requireAdmin group, no extra check needed here.

	var req struct {
		Listeners []api.AppListener `json:"listeners"`
	}
	// ... (rest of function)
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

	ctx := app.WithTaskID(c.Request.Context(), c.GetHeader("X-Piccolo-Task-ID"))
	_, err = s.appManager.UpdateListeners(ctx, appName, req.Listeners)
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

	// RFC 20260122 §5.3: Register/cleanup proxy OIDC client based on updated listener auth.
	// No post-persist event exists for listener updates, so this is done explicitly here.
	// Install and restore paths are handled by observeProxyOIDCClients.
	// Use context.Background() so OIDC registration outlives the HTTP request.
	if s.requiresProxyOIDCClient(newApp.Definition) {
		if err := s.registerProxyOIDCClient(context.Background(), appName); err != nil {
			log.Printf("WARN: failed to register proxy OIDC client for %s: %v", appName, err)
		}
	} else {
		s.deleteProxyOIDCClient(context.Background(), appName)
	}

	recreated := oldApp.PrimaryContainerID() != newApp.PrimaryContainerID()

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

	ctx := app.WithTaskID(c.Request.Context(), c.GetHeader("X-Piccolo-Task-ID"))
	err := s.appManager.Uninstall(ctx, appName)
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

	// Delete all OIDC clients (passthrough + proxy) for this app on uninstall (best-effort).
	if clientMgr := s.getOIDCClientManager(); clientMgr != nil {
		if err := clientMgr.DeleteClientsByAppID(ctx, appName); err != nil {
			log.Printf("WARN: failed to delete OIDC clients for %s: %v", appName, err)
		}
	}

	writeGinSuccess(c, nil, "App '"+appName+"' uninstalled successfully")
}

// handleGinAppStart handles POST /api/v1/apps/:name/start - Start app container
func (s *GinServer) handleGinAppStart(c *gin.Context) {
	appName := c.Param("name")
	// Demo mode: simulate success without backend
	if os.Getenv("PICCOLO_DEMO") != "" {
		writeGinSuccess(c, nil, "App '"+appName+"' started successfully")
		return
	}

	ctx := app.WithTaskID(c.Request.Context(), c.GetHeader("X-Piccolo-Task-ID"))
	err := s.appManager.Start(ctx, appName)
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

	ctx := app.WithTaskID(c.Request.Context(), c.GetHeader("X-Piccolo-Task-ID"))
	err := s.appManager.Stop(ctx, appName)
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

// handleGinAppRollback handles POST /api/v1/apps/:name/rollback - Rollback to previous snapshot
func (s *GinServer) handleGinAppRollback(c *gin.Context) {
	appName := c.Param("name")

	ctx := app.WithTaskID(c.Request.Context(), c.GetHeader("X-Piccolo-Task-ID"))
	err := s.appManager.RollbackToSnapshot(ctx, appName)
	if err != nil {
		if handleAppManagerError(c, err, "rollback app") {
			return
		}
		errMsg := err.Error()
		switch {
		case strings.Contains(errMsg, "not found"):
			writeGinError(c, http.StatusNotFound, errMsg)
		case strings.Contains(errMsg, "no snapshot available") || strings.Contains(errMsg, "no tuple state"):
			writeGinError(c, http.StatusConflict, "No snapshot available for rollback")
		default:
			writeGinError(c, http.StatusInternalServerError, "Rollback failed: "+errMsg)
		}
		return
	}

	writeGinSuccess(c, nil, "App '"+appName+"' rolled back successfully")
}

// handleGinAppClone handles POST /api/v1/apps/:name/clone - Clone a workspace app
func (s *GinServer) handleGinAppClone(c *gin.Context) {
	originName := c.Param("name")

	var body struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		writeGinError(c, http.StatusBadRequest, "Request body must include 'name' for the clone")
		return
	}
	cloneName := strings.TrimSpace(body.Name)

	ctx := app.WithTaskID(c.Request.Context(), c.GetHeader("X-Piccolo-Task-ID"))
	inst, err := s.appManager.CloneWorkspace(ctx, originName, cloneName)
	if err != nil {
		if handleAppManagerError(c, err, "clone workspace") {
			return
		}
		errMsg := err.Error()
		switch {
		case strings.Contains(errMsg, "not found"):
			writeGinError(c, http.StatusNotFound, errMsg)
		case strings.Contains(errMsg, "not a workspace"):
			writeGinError(c, http.StatusBadRequest, errMsg)
		case strings.Contains(errMsg, "must be stopped"):
			writeGinError(c, http.StatusConflict, errMsg)
		case strings.Contains(errMsg, "already exists") || strings.Contains(errMsg, "invalid name"):
			writeGinError(c, http.StatusBadRequest, errMsg)
		default:
			writeGinError(c, http.StatusInternalServerError, "Failed to clone workspace: "+errMsg)
		}
		return
	}

	c.JSON(http.StatusCreated, GinAppResponse{Data: inst, Message: "Workspace cloned"})
}

// handleGinAppListClones handles GET /api/v1/apps/:name/clones - List workspace clones
func (s *GinServer) handleGinAppListClones(c *gin.Context) {
	originName := c.Param("name")

	clones, err := s.appManager.ListWorkspaceClones(c.Request.Context(), originName)
	if err != nil {
		if handleAppManagerError(c, err, "list clones") {
			return
		}
		if strings.Contains(err.Error(), "not found") {
			writeGinError(c, http.StatusNotFound, err.Error())
			return
		}
		writeGinError(c, http.StatusInternalServerError, "Failed to list clones: "+err.Error())
		return
	}

	msg := fmt.Sprintf("Found %d clones", len(clones))
	writeGinSuccess(c, clones, msg)
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

// handleGinCatalogIcon handles GET /api/v1/catalog/:name/icon - proxy and cache app icons
func (s *GinServer) handleGinCatalogIcon(c *gin.Context) {
	if s.catalogManager == nil {
		writeGinError(c, http.StatusInternalServerError, "Catalog manager not initialized")
		return
	}

	name := c.Param("name")
	result, err := s.catalogManager.GetIconByName(c.Request.Context(), name)
	if err != nil {
		if errors.Is(err, catalog.ErrIconNotFound) || errors.Is(err, catalog.ErrNoIconURL) {
			writeGinError(c, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, catalog.ErrSSRFBlocked) {
			writeGinError(c, http.StatusForbidden, "icon URL blocked for security reasons")
			return
		}
		writeGinError(c, http.StatusBadGateway, "failed to fetch icon")
		return
	}

	// Set cache and security headers
	c.Header("Cache-Control", "public, max-age=86400")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	c.Data(http.StatusOK, result.ContentType, result.Data)
}

// handleActiveTasks handles GET /api/v1/tasks/active - List in-progress tasks
func (s *GinServer) handleActiveTasks(c *gin.Context) {
	var tasks []events.TaskProgressEvent
	if s.progress != nil {
		tasks = s.progress.ActiveTasks()
	}
	if tasks == nil {
		tasks = []events.TaskProgressEvent{}
	}
	writeGinSuccess(c, tasks, fmt.Sprintf("Found %d active tasks", len(tasks)))
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

	available := true
	for _, inst := range apps {
		if inst.InstanceID == candidate {
			available = false
			break
		}
	}

	// RFC 20260130: We no longer suggest alternatives; users must choose a unique name
	c.JSON(http.StatusOK, gin.H{
		"available": available,
	})
}

// isIDAllowed checks if the given app ID is in the user's allowed apps list.
func isIDAllowed(allowedApps []string, appID string) bool {
	if len(allowedApps) == 0 {
		return false
	}
	for _, allowed := range allowedApps {
		if allowed == appID {
			return true
		}
	}
	return false
}

// handleGinListenerHealth handles GET /api/v1/apps/:name/listeners/:listener/health
// Returns the health status for a specific listener including certificate and backend status.
func (s *GinServer) handleGinListenerHealth(c *gin.Context) {
	appName := c.Param("name")
	listenerName := c.Param("listener")

	// Check access for standard users
	if sess := s.getSessionFromContext(c); sess != nil && sess.Role != "admin" {
		if s.userManager != nil {
			allowed, err := s.userManager.IsAppAllowed(c.Request.Context(), sess.UserID, appName)
			if err != nil || !allowed {
				writeGinError(c, http.StatusForbidden, "Access denied")
				return
			}
		}
	}

	// Get the endpoint
	endpoints, err := s.serviceManager.GetByApp(appName)
	if err != nil {
		writeGinError(c, http.StatusNotFound, "App not found: "+err.Error())
		return
	}

	// Find the specific listener
	var endpoint *services.ServiceEndpoint
	for i := range endpoints {
		if endpoints[i].Name == listenerName {
			endpoint = &endpoints[i]
			break
		}
	}
	if endpoint == nil {
		writeGinError(c, http.StatusNotFound, "Listener not found: "+listenerName)
		return
	}

	// Compute listener health
	health := s.computeListenerHealth(*endpoint)

	c.JSON(http.StatusOK, gin.H{
		"app":      appName,
		"listener": listenerName,
		"health":   health,
	})
}

// computeListenerHealth computes the health status for a listener by delegating
// to ServiceManager.GetListenerHealth, which aggregates certificate status and backend connectivity.
func (s *GinServer) computeListenerHealth(ep services.ServiceEndpoint) services.ListenerHealth {
	return s.serviceManager.GetListenerHealth(ep)
}

// AppInstanceWithHealth is an API response DTO that adds derived health state
// to the persisted AppInstance. This keeps ephemeral/derived data out of the
// on-disk app state (RFC 20260125 §7.2).
type AppInstanceWithHealth struct {
	*app.AppInstance
	PrimaryListenerHealth *services.ListenerHealth `json:"primary_listener_health,omitempty"`
}

// deriveAppHealth computes the primary listener health for an app instance.
// Returns nil for stopped apps or apps without a primary listener (raw-only).
func (s *GinServer) deriveAppHealth(inst *app.AppInstance) *services.ListenerHealth {
	// Stopped apps don't have health (expected state, not an error)
	if inst.Status == "stopped" {
		return nil
	}

	// Starting apps show recovering status
	if inst.Status == "starting" {
		h := services.ListenerHealth{
			Status:         services.ListenerHealthRecovering,
			ReasonCode:     "app_starting",
			Reason:         "App is starting up",
			Recoverable:    true,
			ActionRequired: false,
			LastChecked:    time.Now(),
		}
		return &h
	}

	// Error apps show error status
	if inst.Status == "error" {
		h := services.ListenerHealth{
			Status:         services.ListenerHealthError,
			ReasonCode:     "app_error",
			Reason:         "App failed to start",
			Recoverable:    false,
			ActionRequired: true,
			LastChecked:    time.Now(),
		}
		return &h
	}

	// Need definition for listener resolution
	if inst.Definition == nil {
		return nil
	}

	// Raw-only apps have no primary listener
	primaryName, _ := hostname.ResolvePrimaryListener(inst.Definition.Listeners)
	if primaryName == "" {
		return nil
	}

	// Find the endpoint for this listener
	if s.serviceManager == nil {
		return nil
	}
	eps, err := s.serviceManager.GetByApp(inst.InstanceID)
	if err != nil {
		return nil
	}

	for _, ep := range eps {
		if ep.Name == primaryName {
			h := s.computeListenerHealth(ep)
			return &h
		}
	}
	return nil
}
