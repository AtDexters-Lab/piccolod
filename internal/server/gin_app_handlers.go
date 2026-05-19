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
	"piccolod/internal/remote/nexusclient"
	"piccolod/internal/services"
)

// syncBlockedReason returns the SyncBlockedReason string for the S1' invariant,
// or empty string when sync is permitted.
func syncBlockedReason(usedSecretOnlyInInitScript bool) string {
	if usedSecretOnlyInInitScript {
		return "catalog uses oidc client_secret only in init_script scope; sync disabled to prevent silent rotation on container recreate"
	}
	return ""
}

// Operation timeouts for app lifecycle handlers. These are safety ceilings —
// the real liveness check for pulls is the stall detector in PullImageWithProgress.
const (
	installTimeout        = 45 * time.Minute // image pull + flatten + container create + init scripts
	cloneWorkspaceTimeout = 10 * time.Minute // LV snapshot + anchor pull + container create
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
	snap := s.buildInstallSystemContext()
	return map[string]interface{}{
		"Domain":       snap.Domain,
		"Architecture": snap.Architecture,
		"Timezone":     snap.Timezone,
	}
}

// buildInstallSystemContext returns the typed snapshot of the live host
// system context. This is the source of truth for both the install handler
// (which uses it to render templates) and the catalog manifest sync host
// (which freezes it into install_state.json so sync re-renders are
// deterministic).
func (s *GinServer) buildInstallSystemContext() app.InstallSystemContext {
	snap := app.InstallSystemContext{
		Domain:       "local",
		Architecture: runtime.GOARCH,
		Timezone:     detectHostTimezone(),
		IssuerHint:   s.oidcIssuer(),
	}
	if hosts := s.portalHosts(); len(hosts) > 0 {
		snap.Domain = hosts[0]
	}
	return snap
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

// portalCertEntry describes a portal hostname and its cert metadata for per-app cert queueing.
type portalCertEntry struct {
	Hostname string
	Source   string
	Solver   string
	CertDir  string
}

// portalCertEntries builds the list of active portals that need per-app certs.
// DNS-01 portals have wildcard coverage, so only HTTP-01 portals need per-app certs.
func (s *GinServer) portalCertEntries() []portalCertEntry {
	var entries []portalCertEntry
	rm := s.remoteManager
	if rm != nil {
		st := rm.Status()
		if st.Enabled && strings.EqualFold(st.Solver, "http-01") {
			base := remoteBaseHostname(&st)
			if base != "" {
				entries = append(entries, portalCertEntry{
					Hostname: base, Source: "self-hosted", Solver: "http-01",
				})
			}
		}

		// Portal-targeted alias domains need per-app subdomain certs (like the
		// main portal hostname). App-targeted aliases are flat hostname mappings
		// — their base cert is handled by AddAlias; no subdomain certs needed.
		if st.Enabled || s.namekDomainActive() {
			for _, a := range rm.ListAliases() {
				if a.Listener != "" && a.Listener != nexusclient.PortalHostLabel {
					continue // app-specific alias — no per-app subdomain certs
				}
				h := normalizeHostname(a.Hostname)
				if h != "" {
					entries = append(entries, portalCertEntry{
						Hostname: h, Source: "alias", Solver: "http-01",
					})
				}
			}
		}
	}
	// Namek portals use DNS-01 → wildcard coverage → no per-app certs needed.
	return entries
}

// queueAppRemoteCertificates queues per-host certs for all HTTP-01 portals.
func (s *GinServer) queueAppRemoteCertificates(appName string) {
	if s == nil || s.remoteManager == nil || s.serviceManager == nil {
		return
	}
	entries := s.portalCertEntries()
	if len(entries) == 0 {
		return
	}
	endpoints, err := s.serviceManager.GetByApp(appName)
	if err != nil {
		log.Printf("WARN: remote: queue certificates for app %s: %v", appName, err)
		return
	}
	for _, entry := range entries {
		for _, ep := range endpoints {
			if ep.DerivedHostLabel == "" || ep.Flow == api.FlowTLS {
				continue // flow:tls apps manage their own certificates (RFC 20260316)
			}
			host := services.RemoteServiceHostname(ep.DerivedHostLabel, entry.Hostname)
			if host == "" {
				continue
			}
			s.remoteManager.EnqueueCertIssuance(remote.CertIssuanceRequest{
				ID: "host:" + host, Source: entry.Source,
				Solver: entry.Solver, CertDir: entry.CertDir,
				Domains: []string{host}, CommonName: host,
			})
		}
	}
}

// removePerAppCertsForBase removes per-app host certs (host:<label>.<base>) for all endpoints.
// Called when an alias is deleted to clean up orphaned per-app certs.
func (s *GinServer) removePerAppCertsForBase(base string) {
	rm := s.remoteManager
	sm := s.serviceManager
	if rm == nil || sm == nil || base == "" {
		return
	}
	endpoints := sm.GetAll()
	for _, ep := range endpoints {
		if ep.DerivedHostLabel == "" {
			continue
		}
		rm.RemoveHostnameCertificate(ep.DerivedHostLabel + "." + base)
	}
}

func remoteHostsForEndpoints(endpoints []services.ServiceEndpoint, base string) map[string]struct{} {
	hosts := map[string]struct{}{}
	base = hostname.Normalize(base)
	if base == "" {
		return hosts
	}
	for _, ep := range endpoints {
		// DerivedHostLabel is empty for raw (flow:tcp) and udp listeners.
		// flow:tls listeners have a label (for remote routing) but cert removal is benign (no-op).
		if ep.DerivedHostLabel == "" {
			continue
		}
		host := ep.DerivedHostLabel + "." + base
		hosts[host] = struct{}{}
	}
	return hosts
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

// parseAppDefinitionFromRequest reads and parses an app YAML from the request body.
// Returns nil and writes an error response if parsing fails.
func (s *GinServer) parseAppDefinitionFromRequest(c *gin.Context) *api.AppDefinition {
	contentType := c.GetHeader("Content-Type")
	if !strings.Contains(contentType, "application/x-yaml") && !strings.Contains(contentType, "text/yaml") && !strings.Contains(contentType, "application/json") {
		writeGinError(c, http.StatusUnsupportedMediaType, "Content-Type must be application/x-yaml or text/yaml or application/json")
		return nil
	}
	var yamlData []byte
	if strings.Contains(contentType, "application/json") {
		var req struct {
			AppDefinition string `json:"app_definition"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.AppDefinition) == "" {
			writeGinError(c, http.StatusBadRequest, "Invalid JSON body; expected {app_definition}")
			return nil
		}
		yamlData = []byte(req.AppDefinition)
	} else {
		body, err := c.GetRawData()
		if err != nil || len(body) == 0 {
			writeGinError(c, http.StatusBadRequest, "Request body cannot be empty")
			return nil
		}
		yamlData = body
	}
	def, err := app.ParseAppDefinition(yamlData)
	if err != nil {
		var ve *app.ValidationError
		if errors.As(err, &ve) && ve != nil {
			writeGinErrorWithKey(c, http.StatusBadRequest, "Invalid app.yaml: "+ve.Message, ve.Code)
			return nil
		}
		writeGinError(c, http.StatusBadRequest, "Invalid app.yaml: "+err.Error())
		return nil
	}
	return def
}

// handleGinAppConfigure handles POST /api/v1/apps/configure - parse an arbitrary
// YAML manifest and return its enriched input schema (smart defaults + system
// templates resolved). Accepts either raw YAML or a JSON wrapper with an
// optional catalog_source hint used for __app_address__ default derivation.
func (s *GinServer) handleGinAppConfigure(c *gin.Context) {
	contentType := c.GetHeader("Content-Type")
	if !strings.Contains(contentType, "application/x-yaml") &&
		!strings.Contains(contentType, "text/yaml") &&
		!strings.Contains(contentType, "application/json") {
		writeGinError(c, http.StatusUnsupportedMediaType, "Content-Type must be application/x-yaml or text/yaml or application/json")
		return
	}
	var yamlData []byte
	var catalogSource string
	if strings.Contains(contentType, "application/json") {
		var req struct {
			AppDefinition string `json:"app_definition"`
			CatalogSource string `json:"catalog_source"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.AppDefinition) == "" {
			writeGinError(c, http.StatusBadRequest, "Invalid JSON body; expected {app_definition}")
			return
		}
		yamlData = []byte(req.AppDefinition)
		catalogSource = req.CatalogSource
	} else {
		body, err := c.GetRawData()
		if err != nil || len(body) == 0 {
			writeGinError(c, http.StatusBadRequest, "Request body cannot be empty")
			return
		}
		yamlData = body
	}

	def, err := app.ParseAppSchema(yamlData)
	if err != nil {
		var ve *app.ValidationError
		if errors.As(err, &ve) && ve != nil {
			writeGinErrorWithKey(c, http.StatusBadRequest, "Invalid app.yaml: "+ve.Message, ve.Code)
			return
		}
		writeGinError(c, http.StatusBadRequest, "Invalid app.yaml: "+err.Error())
		return
	}

	if err := app.PrepareSmartDefaults(c.Request.Context(), s.appManager, def, catalogSource); err != nil {
		log.Printf("WARN: failed to prepare smart defaults: %v", err)
		// Continue anyway, just without smarts
	}
	resolveSystemDefaults(def.Inputs, s.buildSystemContext())

	writeGinSuccess(c, def.Inputs, "Configuration schema prepared")
}

// handleGinAppPreflight handles POST /api/v1/apps/preflight - validate manifest and report port claims
func (s *GinServer) handleGinAppPreflight(c *gin.Context) {
	def := s.parseAppDefinitionFromRequest(c)
	if def == nil {
		return
	}

	type portClaimDetail struct {
		Port     int    `json:"port"`
		Protocol string `json:"protocol"`
		Listener string `json:"listener"`
		Flow     string `json:"flow"`
	}
	claims := make([]portClaimDetail, 0)
	conflicts := make([]string, 0)

	// Snapshot the registry once for consistent conflict checks across all listeners.
	var registry map[string]map[string]services.ServiceEndpoint
	if s.serviceManager != nil {
		registry = s.serviceManager.SnapshotRegistry()
	}

	for _, l := range def.Listeners {
		if l.PortClaim == nil {
			continue
		}
		claims = append(claims, portClaimDetail{
			Port:     *l.PortClaim,
			Protocol: l.Flow.TransportProtocol(),
			Listener: l.Name,
			Flow:     l.Flow.String(),
		})
		for appName, endpoints := range registry {
			for _, ep := range endpoints {
				if ep.PortClaim != nil && *ep.PortClaim == *l.PortClaim && ep.Flow.TransportProtocol() == l.Flow.TransportProtocol() {
					conflicts = append(conflicts, fmt.Sprintf("%s port %d claimed by app '%s' listener '%s'",
						l.Flow.TransportProtocol(), *l.PortClaim, appName, ep.Name))
				}
			}
		}
	}

	writeGinSuccess(c, gin.H{
		"valid":     true,
		"claims":    claims,
		"conflicts": conflicts,
	}, "preflight")
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

	systemSnap := s.buildInstallSystemContext()
	clientMgr := s.getOIDCClientManager()
	// Convert *oidc.ClientManager nil to a true-nil interface so the pipeline's
	// `clientGen != nil` check matches Go interface semantics.
	var clientGen app.OIDCClientGenerator
	if clientMgr != nil {
		clientGen = clientMgr
	}

	pipelineRes, err := app.RunInstallPipeline(c.Request.Context(), app.InstallPipelineInput{
		RawTemplate:   yamlData,
		UserInputs:    userInputs,
		SystemContext: systemSnap,
		ExistingOIDC:  nil,
	}, clientGen, s.appManager)
	if err != nil {
		var ve *app.ValidationError
		if errors.As(err, &ve) && ve != nil {
			writeGinErrorWithKey(c, http.StatusBadRequest, "Invalid app.yaml: "+ve.Message, ve.Code)
			return
		}
		// Map well-known pipeline errors to 4xx responses.
		msg := err.Error()
		switch {
		case strings.Contains(msg, "parse schema"),
			strings.Contains(msg, "parse rendered"),
			strings.Contains(msg, "parse re-rendered"),
			strings.Contains(msg, "re-render with creds"),
			strings.Contains(msg, "render manifest"),
			strings.Contains(msg, "validate definition"),
			strings.Contains(msg, "primary listener name"),
			strings.Contains(msg, "invalid app address"),
			strings.Contains(msg, "invalid workspace name"),
			strings.Contains(msg, "input validation failed"),
			strings.Contains(msg, "must have exactly one listener"),
			strings.Contains(msg, "invalid app definition"):
			writeGinError(c, http.StatusBadRequest, "Invalid app.yaml: "+msg)
			return
		case strings.Contains(msg, "already exists") || strings.Contains(msg, "already in use"):
			writeGinError(c, http.StatusConflict, msg)
			return
		default:
			writeGinError(c, http.StatusInternalServerError, "Failed to render manifest: "+msg)
			return
		}
	}

	appDef := pipelineRes.Definition
	var oidcClientID, oidcClientSecret string
	if pipelineRes.OIDCCredentials != nil {
		oidcClientID = pipelineRes.OIDCCredentials.ClientID
		oidcClientSecret = pipelineRes.OIDCCredentials.ClientSecret
	}

	// Install a new app instance.
	// Decouple from HTTP request context — installs must survive connection drops.
	installCtx, cancelInstall := s.opContext(c, installTimeout)
	defer cancelInstall()
	if catalogSource != "" {
		installCtx = app.WithCatalogSource(installCtx, catalogSource)
	}

	// Fetch init script files from the store. Requires catalog_source because
	// the store URL is derived from the catalog index. Non-catalog installs
	// that declare init_script will hit the warning below and the init phase
	// will be silently skipped (since FileContent will be empty).
	// Uses installCtx so the fetch survives client disconnects.
	for svcName, svc := range appDef.Services {
		if svc.InitScript == nil || svc.InitScript.File == "" {
			continue
		}
		if catalogSource == "" || s.catalogManager == nil {
			log.Printf("WARN: services.%s.init_script declared but install has no catalog_source; init will be skipped. Use the JSON install wrapper with catalog_source to enable init scripts.", svcName)
			continue
		}
		content, err := s.catalogManager.FetchAppFile(installCtx, catalogSource, svc.InitScript.File)
		if err != nil {
			writeGinError(c, http.StatusBadRequest, fmt.Sprintf(
				"failed to fetch init script '%s' for service '%s': %v",
				svc.InitScript.File, svcName, err))
			return
		}
		svc.InitScript.FileContent = content
		appDef.Services[svcName] = svc
	}

	// Note: OIDC clients are registered AFTER Install (below). Init scripts
	// can receive OIDC credentials via {{ .System.Auth.* }} but cannot perform
	// full OIDC authorization flows during init — redirect URIs are only
	// resolvable after StoreApp completes. Init scripts should store the
	// credentials for the app to use after install (like Immich does).
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

	// Persist install state for catalog manifest sync. Skip entirely for
	// non-catalog installs (custom Docker Hub flow): they have no
	// CatalogSource so sync would never act on them, but install_state.json
	// would still hold plaintext InstallInputs and OIDC ClientSecret for the
	// app's lifetime — an avoidable secrets footprint.
	if catalogSource != "" {
		initScriptHashes := map[string]string{}
		for svcName, svc := range appDef.Services {
			if svc.InitScript == nil || len(svc.InitScript.FileContent) == 0 {
				continue
			}
			initScriptHashes[svcName] = app.Sha256Hex(svc.InitScript.FileContent)
		}
		persistErr := s.appManager.RecordInstallState(installCtx, appInstance.InstanceID, app.RecordInstallStateInput{
			CatalogManifestHash: pipelineRes.RawTemplateHash,
			InitScriptHashes:    initScriptHashes,
			InstallState: &app.InstallState{
				InstanceID:        appInstance.InstanceID,
				InstallInputs:     userInputs,
				InstallSystemCtx:  &systemSnap,
				OIDCCredentials:   pipelineRes.OIDCCredentials,
				SyncBlocked:       pipelineRes.UsedSecretOnlyInInitScript,
				SyncBlockedReason: syncBlockedReason(pipelineRes.UsedSecretOnlyInInitScript),
			},
		})
		if persistErr != nil {
			// Persistence is best-effort: install already succeeded. Sync
			// will treat the app as legacy on first contact and patch via
			// allowlist.
			log.Printf("WARN: install %s: persist install_state.json: %v", appInstance.InstanceID, persistErr)
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
	portalHosts := s.portalHosts()
	for _, ep := range listeners {
		formatted := s.formatServiceEndpoint(c, ep, portalHosts)
		// Add listener health status (RFC 20260125)
		formatted["health"] = s.computeListenerHealth(ep)
		listenerStatus = append(listenerStatus, formatted)
	}

	containerStatus, err := s.appManager.ContainerStatuses(c.Request.Context(), appName)
	if err != nil {
		log.Printf("WARN: app get %s: container status unavailable: %v", appName, err)
	}
	snapshotAvailable := s.appManager.HasSnapshotAvailable(c.Request.Context(), appName)
	writeGinSuccess(c, gin.H{"app": appInstance, "listeners": listenerStatus, "containers": containerStatus, "snapshot_available": snapshotAvailable}, "")
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

	// Decouple from HTTP request context — listener updates trigger nexus adapter
	// restarts (port claims change), which can sever the connection carrying this request.
	ctx, cancel := s.opContext(c, 2*time.Minute)
	defer cancel()
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

	newApp, err := s.appManager.Get(ctx, appName)
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
	portalHosts := s.portalHosts()
	for _, ep := range services {
		serviceStatus = append(serviceStatus, s.formatServiceEndpoint(c, ep, portalHosts))
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

	ctx, cancel := s.opContext(c, 2*time.Minute)
	defer cancel()
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

	ctx, cancel := s.opContext(c, 2*time.Minute)
	defer cancel()
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

	ctx, cancel := s.opContext(c, 2*time.Minute)
	defer cancel()
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

// handleGinAppUpdate handles POST /api/v1/apps/:name/update - Update app image.
// Uses a background context because image pulls can take a long time and must
// complete even if the HTTP connection times out.
func (s *GinServer) handleGinAppUpdate(c *gin.Context) {
	appName := c.Param("name")

	updateCtx, cancel := s.opContext(c, 30*time.Minute)
	defer cancel()

	err := s.appManager.UpdateImage(updateCtx, appName)
	if err != nil {
		if handleAppManagerError(c, err, "update app") {
			return
		}
		errMsg := err.Error()
		switch {
		case strings.Contains(errMsg, "not found"):
			writeGinError(c, http.StatusNotFound, errMsg)
		case strings.Contains(errMsg, "workspace"):
			writeGinError(c, http.StatusBadRequest, errMsg)
		default:
			writeGinError(c, http.StatusInternalServerError, "Update failed: "+errMsg)
		}
		return
	}

	writeGinSuccess(c, nil, "App updated successfully")
}

// handleGinAppRollback handles POST /api/v1/apps/:name/rollback - Rollback to previous snapshot
// Uses a background context because rollback involves non-interruptible LV rename
// operations that must complete even if the HTTP connection drops.
func (s *GinServer) handleGinAppRollback(c *gin.Context) {
	appName := c.Param("name")

	rollbackCtx, cancel := s.opContext(c, 5*time.Minute)
	defer cancel()

	err := s.appManager.RollbackToSnapshot(rollbackCtx, appName)
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

	ctx, cancel := s.opContext(c, cloneWorkspaceTimeout)
	defer cancel()
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

// handleGinCatalogIcon handles GET /api/v1/catalog/:name/icon - proxy and cache app icons.
// Cache-Control headers are set BEFORE writeGinError / c.Data because Gin flushes
// response headers on body write — c.Header after that is a no-op.
func (s *GinServer) handleGinCatalogIcon(c *gin.Context) {
	if s.catalogManager == nil {
		writeGinError(c, http.StatusInternalServerError, "Catalog manager not initialized")
		return
	}

	name := c.Param("name")
	result, isStale, err := s.catalogManager.GetIconByName(c.Request.Context(), name)
	if err != nil {
		if errors.Is(err, catalog.ErrIconNotFound) || errors.Is(err, catalog.ErrNoIconURL) {
			c.Header("Cache-Control", "private, max-age=300")
			writeGinError(c, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, catalog.ErrSSRFBlocked) {
			c.Header("Cache-Control", "private, max-age=300")
			writeGinError(c, http.StatusForbidden, "icon URL blocked for security reasons")
			return
		}
		c.Header("Cache-Control", "private, max-age=30")
		writeGinError(c, http.StatusBadGateway, "failed to fetch icon")
		return
	}

	// Stale-but-served: short browser cache so the client picks up the background
	// refresh on its next poll instead of holding the stale bytes for another day.
	if isStale {
		c.Header("Cache-Control", "public, max-age=300")
	} else {
		c.Header("Cache-Control", "public, max-age=86400")
	}
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
	if errors.Is(err, app.ErrNotLeader) {
		writeGinError(c, http.StatusServiceUnavailable, "This node is not the cluster leader; retry on the leader.")
		return true
	}
	if errors.Is(err, app.ErrAppUninstalling) {
		writeGinError(c, http.StatusConflict, "App is being uninstalled")
		return true
	}
	if errors.Is(err, app.ErrAppNotFound) {
		writeGinError(c, http.StatusNotFound, err.Error())
		return true
	}
	if errors.Is(err, app.ErrSyncInProgress) {
		writeGinError(c, http.StatusConflict, err.Error())
		return true
	}
	if errors.Is(err, app.ErrSyncHostUnavailable) {
		writeGinError(c, http.StatusServiceUnavailable, err.Error())
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

// handleAppResizeStorage handles POST /api/v1/apps/:name/resize-storage.
// Dispatches by volume type: workspace-mode apps resize their workspace
// rootfs volume; service-mode apps resize the per-app application
// volume (service-data) that holds their /data. Route shape and request
// body are unchanged from before the resource-stewardship change.
func (s *GinServer) handleAppResizeStorage(c *gin.Context) {
	appName := c.Param("name")
	ctx := c.Request.Context()

	var req struct {
		SizeBytes int64 `json:"size_bytes" binding:"required,gt=0"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeGinError(c, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}

	inst, err := s.appManager.Get(ctx, appName)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeGinError(c, http.StatusNotFound, "App not found: "+appName)
		} else {
			writeGinError(c, http.StatusInternalServerError, "Failed to get app: "+err.Error())
		}
		return
	}

	rootfs := s.persistence.Rootfs()
	if rootfs == nil {
		writeGinError(c, http.StatusServiceUnavailable, "Rootfs manager not available")
		return
	}

	// Prefer workspace-volume resize when present (workspace-mode apps).
	// Fall back to the per-app application volume (service-mode).
	workspaceVolumeID := rootfs.RootfsVolumeID("workspace", inst.InstanceID)
	if rootfs.RootfsExists(workspaceVolumeID) {
		if err := rootfs.ResizeWorkspace(ctx, workspaceVolumeID, req.SizeBytes); err != nil {
			log.Printf("WARN: resize workspace %s: %v", workspaceVolumeID, err)
			writeGinError(c, http.StatusInternalServerError, "Resize failed")
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"status":      "resized",
			"volume_id":   workspaceVolumeID,
			"volume_kind": "workspace",
			"size_bytes":  req.SizeBytes,
		})
		return
	}

	// Service-mode app: resize the per-app application volume (app-<instanceID>).
	appVolumeID := "app-" + inst.InstanceID
	if err := rootfs.ResizeApplication(ctx, appVolumeID, req.SizeBytes); err != nil {
		log.Printf("WARN: resize application volume %s: %v", appVolumeID, err)
		writeGinError(c, http.StatusInternalServerError, "Resize failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":      "resized",
		"volume_id":   appVolumeID,
		"volume_kind": "application",
		"size_bytes":  req.SizeBytes,
	})
}
