package app

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"strings"
	"text/template"

	"piccolod/internal/api"

	"gopkg.in/yaml.v3"
)

var (
	// Valid app name pattern: lowercase letters, numbers, hyphens
	// Must start with letter, end with letter or number
	appNameRegex = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$|^[a-z]$`)
)

// PiccoloMode represents the execution mode for an app as defined in x-piccolo.mode.
type PiccoloMode string

const (
	// ModeService indicates a stateless/replaceable container mode.
	// Container filesystem is ephemeral and not preserved across reinstalls.
	ModeService PiccoloMode = "service"

	// ModeWorkspace indicates a persistent workspace mode.
	// Container filesystem changes are preserved via snapshots across reinstalls (without purge).
	ModeWorkspace PiccoloMode = "workspace"

	// ModeUnknown indicates the mode could not be determined.
	ModeUnknown PiccoloMode = ""
)

func hasTopLevelKey(node *yaml.Node, key string) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k := node.Content[i]
		if k != nil && k.Kind == yaml.ScalarNode && k.Value == key {
			return true
		}
	}
	return false
}

func topLevelMapping(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	return node
}

func topLevelValue(node *yaml.Node, key string) *yaml.Node {
	m := topLevelMapping(node)
	if m == nil {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i]
		v := m.Content[i+1]
		if k != nil && k.Kind == yaml.ScalarNode && k.Value == key {
			return v
		}
	}
	return nil
}

func validateRawServicesBlocks(root *yaml.Node) error {
	services := topLevelValue(root, "services")
	if services == nil {
		return nil
	}
	if services.Kind != yaml.MappingNode {
		return fmt.Errorf("services must be a mapping")
	}

	allowedServiceKeys := map[string]struct{}{
		"image":       {},
		"after":       {},
		"bind_ports":  {},
		"environment": {},
		"storage":     {},
		"resources":   {},
		"oidc_client": {},
	}

	for i := 0; i+1 < len(services.Content); i += 2 {
		svcKey := services.Content[i]
		svcVal := services.Content[i+1]
		if svcKey == nil || svcKey.Kind != yaml.ScalarNode {
			continue
		}
		name := strings.TrimSpace(svcKey.Value)
		if svcVal == nil || svcVal.Kind != yaml.MappingNode {
			return fmt.Errorf("services.%s must be a mapping", name)
		}
		for j := 0; j+1 < len(svcVal.Content); j += 2 {
			fieldKey := svcVal.Content[j]
			if fieldKey == nil || fieldKey.Kind != yaml.ScalarNode {
				continue
			}
			field := strings.TrimSpace(fieldKey.Value)
			if field == "wait_for" {
				return fmt.Errorf("services.%s.wait_for is not supported yet", name)
			}
			if _, ok := allowedServiceKeys[field]; !ok {
				return fmt.Errorf("services.%s contains unsupported field '%s'", name, field)
			}
		}
	}

	return nil
}

// ParseAppSchema parses the YAML to extract metadata and inputs without strict validation.
// This is used to read the manifest before variable substitution.
func ParseAppSchema(content []byte) (*api.AppDefinition, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	if hasTopLevelKey(&root, "filesystem") {
		return nil, fmt.Errorf("filesystem is no longer supported; use x-piccolo.mode")
	}
	if hasTopLevelKey(&root, "build") {
		return nil, fmt.Errorf("build is not supported; specify image")
	}
	if hasTopLevelKey(&root, "depends_on") {
		return nil, fmt.Errorf("depends_on is not supported; dependencies must be packaged as sidecars")
	}
	var app api.AppDefinition
	if err := root.Decode(&app); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	// We do NOT call SetDefaults or ValidateAppDefinition here because
	// fields like 'name' might contain "{{ .Inputs... }}" which would fail validation.
	return &app, nil
}

// RenderManifest applies user inputs to the app manifest template.
func RenderManifest(rawYaml []byte, userInputs map[string]interface{}, systemContext map[string]interface{}) ([]byte, error) {
	// Prepare data for template
	data := map[string]interface{}{
		"Inputs": userInputs,
		"System": systemContext,
	}

	// Parse and Execute Template
	// Option "missingkey=error" ensures we fail if the manifest references an input we didn't provide
	tmpl, err := template.New("manifest").Option("missingkey=error").Parse(string(rawYaml))
	if err != nil {
		return nil, fmt.Errorf("failed to parse manifest template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("failed to execute manifest template: %w", err)
	}

	return buf.Bytes(), nil
}

// ParseAppDefinition parses YAML content into AppDefinition struct with validation
func ParseAppDefinition(content []byte) (*api.AppDefinition, error) {
	var app api.AppDefinition

	// Parse YAML
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	if hasTopLevelKey(&root, "filesystem") {
		return nil, fmt.Errorf("filesystem is no longer supported; use x-piccolo.mode")
	}
	if hasTopLevelKey(&root, "build") {
		return nil, fmt.Errorf("build is not supported; specify image")
	}
	if hasTopLevelKey(&root, "depends_on") {
		return nil, fmt.Errorf("depends_on is not supported; dependencies must be packaged as sidecars")
	}
	if err := validateRawServicesBlocks(&root); err != nil {
		return nil, err
	}
	if err := root.Decode(&app); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	// Set defaults
	SetDefaults(&app)

	// Validate
	if err := ValidateAppDefinition(&app); err != nil {
		return nil, err
	}

	return &app, nil
}

// SerializeAppDefinition serializes AppDefinition to YAML bytes
func SerializeAppDefinition(app *api.AppDefinition) ([]byte, error) {
	data, err := yaml.Marshal(app)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal YAML: %w", err)
	}
	return data, nil
}

// SetDefaults sets default values for AppDefinition fields
func SetDefaults(app *api.AppDefinition) {
	// Default type is "user"
	if app.Type == "" {
		app.Type = "user"
	}

	// Listeners defaults
	for i := range app.Listeners {
		if app.Listeners[i].Flow == api.FlowUnknown {
			app.Listeners[i].Flow = api.FlowTCP
		}
		if app.Listeners[i].Protocol == api.ListenerProtocolUnknown {
			app.Listeners[i].Protocol = api.ListenerProtocolRaw
		}
	}
}

// ValidateAppDefinition validates an AppDefinition struct
func ValidateAppDefinition(app *api.AppDefinition) error {
	// Validate name
	if err := validateName(app.Name); err != nil {
		return err
	}

	// Validate type
	if err := validateType(app.Type); err != nil {
		return err
	}

	// RFC 20260112: App-level auth block is removed.
	if app.Auth != nil {
		return newValidationError("APP_AUTH_DEPRECATED", "app-level auth block is deprecated; use listeners[].auth and services[].oidc_client")
	}

	// Validate Piccolo-specific extensions (required mode + consistency checks).
	// We validate this early because mode impacts several other validations.
	if err := validatePiccoloExtensions(app); err != nil {
		return err
	}
	mode := piccoloModeFromExtensions(app.Extensions)

	// Validate container model (single vs multi-container) and disallowed fields.
	if err := validateContainerModel(app, mode); err != nil {
		return err
	}

	// Validate listeners (service-oriented)
	// Workspace mode apps are allowed to have no listeners
	if err := validateListeners(app.Listeners, mode); err != nil {
		return err
	}

	// Validate storage
	if err := validateStorage(app.Storage); err != nil {
		return err
	}

	// Validate resources
	if err := validateResources(app.Resources); err != nil {
		return err
	}

	// Validate permissions
	if err := validatePermissions(app.Permissions); err != nil {
		return err
	}

	// RFC 20260112: oidc_passthrough requires at least one service to declare oidc_client.
	if usesOIDCPassthrough(app.Listeners) && !hasOIDCClient(app.Services) {
		return newValidationError("OIDC_CLIENT_REQUIRED", "oidc_passthrough strategy requires at least one service to declare oidc_client")
	}

	return nil
}

func piccoloModeFromExtensions(extensions map[string]interface{}) PiccoloMode {
	if extensions == nil {
		return ModeUnknown
	}
	raw, ok := extensions["mode"]
	if !ok {
		return ModeUnknown
	}
	modeStr, ok := raw.(string)
	if !ok {
		return ModeUnknown
	}
	modeStr = strings.TrimSpace(modeStr)
	switch PiccoloMode(modeStr) {
	case ModeService:
		return ModeService
	case ModeWorkspace:
		return ModeWorkspace
	default:
		return ModeUnknown
	}
}

func validatePiccoloExtensions(app *api.AppDefinition) error {
	if app == nil {
		return nil
	}

	mode := piccoloModeFromExtensions(app.Extensions)
	if mode == ModeUnknown {
		if app.Extensions == nil {
			return fmt.Errorf("x-piccolo is required; set x-piccolo.mode to 'service' or 'workspace'")
		}
		// Check if mode key exists but has invalid value
		if raw, ok := app.Extensions["mode"]; ok {
			return fmt.Errorf("x-piccolo.mode must be 'service' or 'workspace', got '%v'", raw)
		}
		return fmt.Errorf("x-piccolo.mode is required; must be 'service' or 'workspace'")
	}

	return nil
}

const defaultPrimaryServiceName = "main"

func validateContainerModel(app *api.AppDefinition, mode PiccoloMode) error {
	if app == nil {
		return nil
	}

	switch mode {
	case ModeService:
		if len(app.Services) == 0 {
			return fmt.Errorf("services is required for service mode apps")
		}

		// Service-mode apps must define container fields per-service only.
		if strings.TrimSpace(app.Image) != "" {
			return fmt.Errorf("image must be specified per-service under services for service mode apps")
		}
		if app.Environment != nil {
			return fmt.Errorf("environment must be specified per-service under services for service mode apps")
		}
		if app.Storage != nil {
			return fmt.Errorf("storage must be specified per-service under services for service mode apps")
		}
		if app.Resources != nil {
			return fmt.Errorf("resources must be specified per-service under services for service mode apps")
		}

		primary := strings.TrimSpace(app.PrimaryService)
		if primary == "" {
			primary = defaultPrimaryServiceName
			if len(app.Services) == 1 {
				for name := range app.Services {
					primary = name
				}
			}
		}
		if _, ok := app.Services[primary]; !ok {
			return fmt.Errorf("primary_service '%s' not found in services", primary)
		}

		return validateServices(app.Services, primary, app.Listeners)
	case ModeWorkspace:
		if len(app.Services) == 0 {
			return fmt.Errorf("services is required for workspace mode apps")
		}
		if len(app.Services) != 1 {
			return fmt.Errorf("workspace mode apps must define exactly one service")
		}
		// Workspace-mode apps must define container fields per-service only.
		if strings.TrimSpace(app.Image) != "" {
			return fmt.Errorf("image must be specified per-service under services for workspace mode apps")
		}
		if app.Environment != nil {
			return fmt.Errorf("environment must be specified per-service under services for workspace mode apps")
		}
		if app.Storage != nil {
			return fmt.Errorf("storage must be specified per-service under services for workspace mode apps")
		}
		if app.Resources != nil {
			return fmt.Errorf("resources must be specified per-service under services for workspace mode apps")
		}

		primary := strings.TrimSpace(app.PrimaryService)
		if primary == "" {
			primary = defaultPrimaryServiceName
		}
		if _, ok := app.Services[primary]; !ok {
			return fmt.Errorf("primary_service '%s' not found in services", primary)
		}
		return validateServices(app.Services, primary, app.Listeners)
	}

	return nil
}

func validateServiceName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("service name cannot be empty")
	}
	if !appNameRegex.MatchString(name) {
		return fmt.Errorf("service name '%s' must contain only lowercase letters, numbers, and hyphens, and must start with a letter", name)
	}
	return nil
}

func validateServices(services map[string]api.AppService, primary string, listeners []api.AppListener) error {
	oidcEnv := make(map[string]string)

	// Validate service specs first (names, images, per-service storage/resources).
	for name, svc := range services {
		if err := validateServiceName(name); err != nil {
			return err
		}
		if strings.TrimSpace(svc.Image) == "" {
			return fmt.Errorf("services.%s.image is required", name)
		}
		// bind_ports is required (may be empty) so Piccolo can validate shared port namespace collisions.
		if svc.BindPorts == nil {
			return fmt.Errorf("services.%s.bind_ports is required (may be empty)", name)
		}
		if err := validateResources(svc.Resources); err != nil {
			return fmt.Errorf("services.%s.resources invalid: %w", name, err)
		}
		if err := validateStorage(svc.Storage); err != nil {
			return fmt.Errorf("services.%s.storage invalid: %w", name, err)
		}

		if svc.OIDCClient != nil {
			if len(svc.OIDCClient.Env) == 0 {
				return newValidationError("OIDC_ENV_REQUIRED", "oidc_client.env must not be empty")
			}
			if strings.TrimSpace(svc.OIDCClient.CAMountPath) == "" {
				return newValidationError("OIDC_CA_PATH_REQUIRED", "oidc_client.ca_mount_path is required")
			}
			for k, v := range svc.OIDCClient.Env {
				if prior, ok := oidcEnv[k]; ok && prior != v {
					return fmt.Errorf("oidc_client.env conflicts for key '%s' across services", k)
				}
				oidcEnv[k] = v
			}
			for _, redirectURI := range svc.OIDCClient.RedirectURIs {
				if err := validateOIDCRedirectURI(redirectURI); err != nil {
					return newValidationError("INVALID_REDIRECT_URI", fmt.Sprintf("redirect_uri \"%s\" must be localhost, loopback (127.0.0.1, ::1), or custom scheme", redirectURI))
				}
			}
		}
	}

	// Validate after graph (unknown refs + cycles) and ensure stable order exists.
	if _, err := serviceStartOrder(services); err != nil {
		return err
	}

	// Validate bind_ports across shared network namespace.
	portOwners := make(map[int]string)
	for name, svc := range services {
		seen := make(map[int]struct{}, len(svc.BindPorts))
		for _, port := range svc.BindPorts {
			if port < 1 || port > 65535 {
				return fmt.Errorf("services.%s.bind_ports contains invalid port %d", name, port)
			}
			if _, ok := seen[port]; ok {
				return fmt.Errorf("services.%s.bind_ports contains duplicate port %d", name, port)
			}
			seen[port] = struct{}{}
			if other, ok := portOwners[port]; ok {
				return fmt.Errorf("bind_ports collision: port %d declared by both services '%s' and '%s'", port, other, name)
			}
			portOwners[port] = name
		}
	}

	// Primary service must declare all listener guest ports (v1 listeners target primary by default).
	// Skip this check for single-service apps since port conflicts are impossible.
	if len(services) > 1 {
		primarySvc := services[primary]
		primaryPorts := make(map[int]struct{}, len(primarySvc.BindPorts))
		for _, p := range primarySvc.BindPorts {
			primaryPorts[p] = struct{}{}
		}
		for _, l := range listeners {
			if _, ok := primaryPorts[l.GuestPort]; !ok {
				return fmt.Errorf("primary service '%s' must declare listener guest_port %d in bind_ports", primary, l.GuestPort)
			}
		}
	}

	// Explicit persistent volume sharing rules.
	type volumeRef struct {
		service   string
		sizeLimit string
		shared    bool
	}
	refs := make(map[string][]volumeRef)
	for svcName, svc := range services {
		if svc.Storage == nil || svc.Storage.Persistent == nil {
			continue
		}
		for volName, vol := range svc.Storage.Persistent {
			size := strings.TrimSpace(vol.SizeLimit)
			refs[volName] = append(refs[volName], volumeRef{
				service:   svcName,
				sizeLimit: size,
				shared:    vol.Shared,
			})
		}
	}

	for volName, volRefs := range refs {
		if len(volRefs) <= 1 {
			continue
		}
		wantSize := volRefs[0].sizeLimit
		for _, r := range volRefs {
			if r.sizeLimit != wantSize {
				return fmt.Errorf("persistent volume '%s' has conflicting size_limit across services", volName)
			}
			if !r.shared {
				return fmt.Errorf("persistent volume '%s' is referenced by multiple services; set shared: true for service '%s'", volName, r.service)
			}
		}
	}

	return nil
}

// validateName validates app name follows naming conventions
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}

	if len(name) > 50 {
		return fmt.Errorf("name must be 50 characters or less")
	}

	if !appNameRegex.MatchString(name) {
		return fmt.Errorf("name must contain only lowercase letters, numbers, and hyphens, and must start with a letter")
	}

	// Reserved names check
	reserved := []string{"api", "www", "admin", "root", "system", "piccolo"}
	for _, r := range reserved {
		if name == r {
			return fmt.Errorf("name '%s' is reserved", name)
		}
	}

	return nil
}

// validateType validates app type field
func validateType(appType string) error {
	validTypes := []string{"user", "system"}
	for _, valid := range validTypes {
		if appType == valid {
			return nil
		}
	}
	return fmt.Errorf("type must be either 'user' or 'system', got '%s'", appType)
}

// validateListeners validates listener configurations.
// Workspace mode apps are allowed to have no listeners (blank environments).
func validateListeners(listeners []api.AppListener, mode PiccoloMode) error {
	if len(listeners) == 0 {
		// Workspace mode apps can have no listeners - they're blank environments
		// where users add ports as needed via Edit Listeners
		if mode == ModeWorkspace {
			return nil
		}
		return fmt.Errorf("listeners are required; legacy ports are no longer supported")
	}

	names := make(map[string]struct{})
	guestPorts := make(map[int]string)

	for i, l := range listeners {
		// name required
		if strings.TrimSpace(l.Name) == "" {
			return fmt.Errorf("listener[%d] name is required", i)
		}
		// unique name per app
		if _, ok := names[l.Name]; ok {
			return fmt.Errorf("duplicate listener name '%s'", l.Name)
		}
		names[l.Name] = struct{}{}

		// guest_port required and valid
		if l.GuestPort < 1 || l.GuestPort > 65535 {
			return fmt.Errorf("listener '%s' guest_port must be between 1 and 65535", l.Name)
		}
		if existing, ok := guestPorts[l.GuestPort]; ok {
			return fmt.Errorf("guest_port %d used by both '%s' and '%s'", l.GuestPort, existing, l.Name)
		}
		guestPorts[l.GuestPort] = l.Name

		if l.Flow != api.FlowTCP && l.Flow != api.FlowTLS {
			return fmt.Errorf("listener '%s' flow must be 'tcp' or 'tls'", l.Name)
		}

		switch l.Protocol {
		case api.ListenerProtocolRaw, api.ListenerProtocolHTTP, api.ListenerProtocolWebsocket:
			// ok
		default:
			return fmt.Errorf("listener '%s' protocol '%s' not supported in v1", l.Name, l.Protocol.String())
		}

		// middleware entries: ensure names present
		for j, m := range l.Middleware {
			if strings.TrimSpace(m.Name) == "" {
				return fmt.Errorf("listener '%s' middleware[%d] name is required", l.Name, j)
			}
		}

		// RFC 20260112: Listener-level auth rules (path-based).
		if l.Auth != nil {
			if l.Flow != api.FlowTCP || (l.Protocol != api.ListenerProtocolHTTP && l.Protocol != api.ListenerProtocolWebsocket) {
				if l.Flow == api.FlowTLS || l.Protocol == api.ListenerProtocolRaw {
					return newValidationError("INVALID_AUTH_PROTOCOL", "auth block not supported on flow: tls or protocol: raw")
				}
				return newValidationError("INVALID_AUTH_FLOW", "auth block requires flow: tcp with protocol: http or websocket")
			}

			for _, rule := range l.Auth.Rules {
				ruleType := strings.TrimSpace(rule.Type)
				if ruleType == "" {
					return newValidationError("INVALID_MATCH_TYPE", "auth.rules[].type is required and must be one of: exact, prefix, pattern")
				}
				switch ruleType {
				case "exact", "prefix", "pattern":
					// ok
				default:
					return newValidationError("INVALID_MATCH_TYPE", "auth.rules[].type is required and must be one of: exact, prefix, pattern")
				}

				strategy := strings.TrimSpace(rule.Strategy)
				switch strategy {
				case "oidc_passthrough", "headers", "protected", "public":
					// ok
				default:
					return newValidationError("INVALID_STRATEGY", fmt.Sprintf("invalid strategy \"%s\", must be one of: oidc_passthrough, headers, protected, public", strategy))
				}

				rulePath := strings.TrimSpace(rule.Path)
				if rulePath == "" {
					return newValidationError("INVALID_PATH", fmt.Sprintf("invalid path \"%s\" for type \"%s\": path is required", rule.Path, rule.Type))
				}
				switch ruleType {
				case "prefix":
					if !strings.HasSuffix(rulePath, "/") {
						return newValidationError("INVALID_PATH", fmt.Sprintf("invalid path \"%s\" for type \"%s\": prefix paths must end with '/'", rule.Path, rule.Type))
					}
				case "pattern":
					if _, err := path.Match(rulePath, "/"); err != nil {
						return newValidationError("INVALID_PATH", fmt.Sprintf("invalid path \"%s\" for type \"%s\": %s", rule.Path, rule.Type, err.Error()))
					}
				}
			}
		}
	}
	return nil
}

func usesOIDCPassthrough(listeners []api.AppListener) bool {
	for _, l := range listeners {
		if l.Auth == nil {
			continue
		}
		for _, rule := range l.Auth.Rules {
			if strings.TrimSpace(rule.Strategy) == "oidc_passthrough" {
				return true
			}
		}
	}
	return false
}

func hasOIDCClient(services map[string]api.AppService) bool {
	for _, svc := range services {
		if svc.OIDCClient != nil {
			return true
		}
	}
	return false
}

func validateOIDCRedirectURI(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if strings.TrimSpace(u.Scheme) == "" {
		return fmt.Errorf("scheme required")
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		host := strings.TrimSpace(u.Hostname())
		if host == "" {
			return fmt.Errorf("host required")
		}

		if strings.EqualFold(host, "localhost") {
			return nil
		}

		if ip := net.ParseIP(host); ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				if ip4[0] == 127 && ip4[1] == 0 && ip4[2] == 0 && ip4[3] == 1 {
					return nil
				}
				return fmt.Errorf("host must be localhost or loopback")
			}
			if ip.Equal(net.IPv6loopback) {
				return nil
			}
		}
		return fmt.Errorf("host must be localhost or loopback")
	default:
		// Custom scheme redirect URIs are allowed.
		return nil
	}
}

// validateStorage validates storage configuration
func validateStorage(storage *api.AppStorage) error {
	if storage == nil {
		return nil // Storage is optional
	}

	// Validate persistent storage
	if err := validateStorageVolumes(storage.Persistent, "persistent"); err != nil {
		return err
	}

	// Validate temporary storage
	if err := validateStorageVolumes(storage.Temporary, "temporary"); err != nil {
		return err
	}

	// Prevent conflicting mountpoints inside the container.
	seen := make(map[string]string)
	for name, vol := range storage.Persistent {
		if vol.Container == "" {
			continue
		}
		if prev, ok := seen[vol.Container]; ok {
			return fmt.Errorf("storage volume 'persistent.%s' container path %s conflicts with %s", name, vol.Container, prev)
		}
		seen[vol.Container] = fmt.Sprintf("persistent.%s", name)
	}
	for name, vol := range storage.Temporary {
		if vol.Container == "" {
			continue
		}
		if prev, ok := seen[vol.Container]; ok {
			return fmt.Errorf("storage volume 'temporary.%s' container path %s conflicts with %s", name, vol.Container, prev)
		}
		seen[vol.Container] = fmt.Sprintf("temporary.%s", name)
	}

	return nil
}

// validateStorageVolumes validates a map of storage volumes
func validateStorageVolumes(volumes map[string]api.AppVolume, storageType string) error {
	if volumes == nil {
		return nil
	}

	for name, volume := range volumes {
		if name == "" {
			return fmt.Errorf("%s storage volume name cannot be empty", storageType)
		}

		if volume.Container == "" {
			return fmt.Errorf("%s storage volume '%s' must specify container path", storageType, name)
		}

		// Host paths are not user-configurable in Piccolo (all host paths live under the app's encrypted volume).
		if strings.TrimSpace(volume.Host) != "" {
			return fmt.Errorf("%s storage volume '%s' must not specify host; Piccolo manages host paths", storageType, name)
		}

		// Validate container path is absolute
		if !strings.HasPrefix(volume.Container, "/") {
			return fmt.Errorf("%s storage volume '%s' container path must be absolute", storageType, name)
		}

		// Validate size limit format if specified
		if volume.SizeLimit != "" {
			if err := validateSizeLimit(volume.SizeLimit); err != nil {
				return fmt.Errorf("%s storage volume '%s' size limit invalid: %w", storageType, name, err)
			}
		}
	}

	return nil
}

// validateResources validates resource limits
func validateResources(resources *api.AppResources) error {
	if resources == nil || resources.Limits == nil {
		return nil // Resources are optional
	}

	limits := resources.Limits

	// Validate memory limit
	if limits.Memory != "" {
		if err := validateSizeLimit(limits.Memory); err != nil {
			return fmt.Errorf("invalid memory limit: %w", err)
		}
	}

	// Validate CPU limit
	if limits.CPU < 0 {
		return fmt.Errorf("CPU limit must be non-negative")
	}
	if limits.CPU > 64 { // Reasonable upper limit
		return fmt.Errorf("CPU limit cannot exceed 64 cores")
	}

	// Validate storage limit
	if limits.Storage != "" {
		if err := validateSizeLimit(limits.Storage); err != nil {
			return fmt.Errorf("invalid storage limit: %w", err)
		}
	}

	return nil
}

// validatePermissions validates permissions configuration
func validatePermissions(permissions *api.AppPermissions) error {
	if permissions == nil {
		return nil // Permissions are optional
	}

	// Validate network permissions
	if permissions.Network != nil {
		if err := validateNetworkPermissions(permissions.Network); err != nil {
			return err
		}
	}

	// Validate resource permissions
	if permissions.Resources != nil {
		if err := validateResourcePermissions(permissions.Resources); err != nil {
			return err
		}
	}

	return nil
}

// validateNetworkPermissions validates network permission settings
func validateNetworkPermissions(network *api.AppNetworkPermissions) error {
	validValues := []string{"allow", "deny", ""}

	for _, field := range []struct {
		name  string
		value string
	}{
		{"internet", network.Internet},
		{"local_network", network.LocalNetwork},
		{"dns", network.DNS},
	} {
		if field.value != "" {
			found := false
			for _, valid := range validValues {
				if field.value == valid {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("network.%s must be 'allow' or 'deny', got '%s'", field.name, field.value)
			}
		}
	}

	return nil
}

// validateResourcePermissions validates resource permission settings
func validateResourcePermissions(resources *api.AppResourcePermissions) error {
	if resources.MaxProcesses < 0 {
		return fmt.Errorf("max_processes must be non-negative")
	}
	if resources.MaxOpenFiles < 0 {
		return fmt.Errorf("max_open_files must be non-negative")
	}
	return nil
}

// validateSizeLimit validates size limit format (e.g., "1GB", "500MB")
func validateSizeLimit(limit string) error {
	if limit == "" {
		return nil
	}

	// Simple validation for size format
	validSuffixes := []string{"B", "KB", "MB", "GB", "TB"}

	for _, suffix := range validSuffixes {
		if strings.HasSuffix(strings.ToUpper(limit), suffix) {
			// Extract number part and validate it's positive
			numPart := strings.TrimSuffix(strings.ToUpper(limit), suffix)
			if numPart == "" {
				return fmt.Errorf("size limit must have a numeric value")
			}
			// Basic check - should be more thorough with strconv.ParseFloat
			if strings.Contains(numPart, "-") {
				return fmt.Errorf("size limit must be positive")
			}
			return nil
		}
	}

	return fmt.Errorf("size limit must end with B, KB, MB, GB, or TB")
}
