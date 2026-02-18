package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Container represents the data structure for a container in our public API.
type Container struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Image string `json:"image"`
	State string `json:"state"`
}

// CreateContainerRequest defines the JSON payload for creating a new container.
type CreateContainerRequest struct {
	Name      string    `json:"name"`
	Image     string    `json:"image"`
	Resources Resources `json:"resources,omitempty"`
}

// Resources defines CPU, RAM, and other resource quotas for a container.
type Resources struct {
	CPU    float64 `json:"cpu_cores,omitempty"` // e.g., 0.5 for half a core
	Memory int64   `json:"memory_mb,omitempty"` // Memory in Megabytes
}

// ListenerFlow enumerates supported transport flows for an app listener.
type ListenerFlow uint8

const (
	FlowUnknown ListenerFlow = iota
	FlowTCP
	FlowTLS
)

var flowToString = map[ListenerFlow]string{
	FlowTCP: "tcp",
	FlowTLS: "tls",
}

var flowFromString = map[string]ListenerFlow{
	"tcp": FlowTCP,
	"tls": FlowTLS,
}

// String returns the token representation of the flow.
func (f ListenerFlow) String() string {
	if s, ok := flowToString[f]; ok {
		return s
	}
	return ""
}

// MarshalJSON converts the flow enum back to its token.
func (f ListenerFlow) MarshalJSON() ([]byte, error) {
	if f == FlowUnknown {
		return json.Marshal("")
	}
	return json.Marshal(f.String())
}

// UnmarshalJSON parses a flow token.
func (f *ListenerFlow) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	flow, err := parseListenerFlow(raw)
	if err != nil {
		return err
	}
	*f = flow
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (f ListenerFlow) MarshalYAML() (interface{}, error) {
	if f == FlowUnknown {
		return nil, nil
	}
	return f.String(), nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (f *ListenerFlow) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		return err
	}
	flow, err := parseListenerFlow(raw)
	if err != nil {
		return err
	}
	*f = flow
	return nil
}

func parseListenerFlow(raw string) (ListenerFlow, error) {
	token := strings.ToLower(strings.TrimSpace(raw))
	if token == "" {
		return FlowUnknown, nil
	}
	if flow, ok := flowFromString[token]; ok {
		return flow, nil
	}
	return FlowUnknown, fmt.Errorf("invalid listener flow '%s'", raw)
}

// ListenerProtocol enumerates supported protocols for an app listener.
type ListenerProtocol uint8

const (
	ListenerProtocolUnknown ListenerProtocol = iota
	ListenerProtocolRaw
	ListenerProtocolHTTP
	ListenerProtocolWebsocket
)

var protocolToString = map[ListenerProtocol]string{
	ListenerProtocolRaw:       "raw",
	ListenerProtocolHTTP:      "http",
	ListenerProtocolWebsocket: "websocket",
}

var protocolFromString = map[string]ListenerProtocol{
	"raw":       ListenerProtocolRaw,
	"http":      ListenerProtocolHTTP,
	"websocket": ListenerProtocolWebsocket,
}

// String returns the token representation of the protocol.
func (p ListenerProtocol) String() string {
	if s, ok := protocolToString[p]; ok {
		return s
	}
	return ""
}

// MarshalJSON converts the protocol enum back to its token.
func (p ListenerProtocol) MarshalJSON() ([]byte, error) {
	if p == ListenerProtocolUnknown {
		return json.Marshal("")
	}
	return json.Marshal(p.String())
}

// UnmarshalJSON parses a protocol token.
func (p *ListenerProtocol) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	proto, err := parseListenerProtocol(raw)
	if err != nil {
		return err
	}
	*p = proto
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (p ListenerProtocol) MarshalYAML() (interface{}, error) {
	if p == ListenerProtocolUnknown {
		return nil, nil
	}
	return p.String(), nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (p *ListenerProtocol) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		return err
	}
	proto, err := parseListenerProtocol(raw)
	if err != nil {
		return err
	}
	*p = proto
	return nil
}

func parseListenerProtocol(raw string) (ListenerProtocol, error) {
	token := strings.ToLower(strings.TrimSpace(raw))
	if token == "" {
		return ListenerProtocolUnknown, nil
	}
	if proto, ok := protocolFromString[token]; ok {
		return proto, nil
	}
	return ListenerProtocolUnknown, fmt.Errorf("invalid listener protocol '%s'", raw)
}

// DiskInfo provides detailed, human-readable information about a physical disk.
type DiskInfo struct {
	Path      string `json:"path"`  // e.g., /dev/sda
	Model     string `json:"model"` // e.g., "Samsung SSD 970 EVO"
	SizeBytes int64  `json:"size_bytes"`
	IsSSD     bool   `json:"is_ssd"`
}

// StoragePoolInfo represents the status of the main storage pool.
type StoragePoolInfo struct {
	TotalBytes     int64    `json:"total_bytes"`
	UsedBytes      int64    `json:"used_bytes"`
	FreeBytes      int64    `json:"free_bytes"`
	ComponentDisks []string `json:"component_disks"`
}

// BackupTarget defines a destination for a backup.
type BackupTarget struct {
	Type string `json:"type"`           // e.g., "local_drive", "google_drive", "piccolo_central"
	Path string `json:"path,omitempty"` // For local_drive, e.g., "/media/my-usb"
}

// AppDefinition represents an app.yaml definition file
type AppDefinition struct {
	// WorkspaceName is required when listeners is empty (workspace mode without network services).
	// Mutually exclusive with listeners - if listeners is non-empty, WorkspaceName must be empty.
	// RFC 20260130: Unified App Identity Model
	WorkspaceName string `yaml:"workspace_name,omitempty" json:"workspace_name,omitempty"`
	Image         string `yaml:"image,omitempty" json:"image,omitempty"`
	Type          string `yaml:"type,omitempty" json:"type,omitempty"` // "system" or "user"
	// Inputs definition for dynamic configuration
	Inputs map[string]AppInput `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	// Compose-style multi-container apps (service mode only)
	PrimaryService string                `yaml:"primary_service,omitempty" json:"primary_service,omitempty"`
	Services       map[string]AppService `yaml:"services,omitempty" json:"services,omitempty"`
	// Service-oriented listener configuration (v1)
	Listeners   []AppListener          `yaml:"listeners,omitempty" json:"listeners,omitempty"`
	Storage     *AppStorage            `yaml:"storage,omitempty" json:"storage,omitempty"`
	Permissions *AppPermissions        `yaml:"permissions,omitempty" json:"permissions,omitempty"`
	Environment map[string]string      `yaml:"environment,omitempty" json:"environment,omitempty"`
	Resources   *AppResources          `yaml:"resources,omitempty" json:"resources,omitempty"`
	HealthCheck *AppHealthCheck        `yaml:"healthcheck,omitempty" json:"healthcheck,omitempty"`
	AppConfig   interface{}            `yaml:"app_config,omitempty" json:"app_config,omitempty"`
	Extensions  map[string]interface{} `yaml:"x-piccolo,omitempty" json:"x-piccolo,omitempty"`
	// Auth defines how the app integrates with Piccolo's authentication system
	Auth *AppAuth `yaml:"auth,omitempty" json:"auth,omitempty"`
}

// AppListener defines a named service exposed by the app (service-oriented model)
type AppListener struct {
	Name        string                  `yaml:"name" json:"name"`
	GuestPort   int                     `yaml:"guest_port" json:"guest_port"`
	Flow        ListenerFlow            `yaml:"flow,omitempty" json:"flow,omitempty"`
	Protocol    ListenerProtocol        `yaml:"protocol,omitempty" json:"protocol,omitempty"`
	Primary     bool                    `yaml:"primary,omitempty" json:"primary,omitempty"` // Set programmatically when __primary is substituted. RFC 20260130.
	Middleware  []AppProtocolMiddleware `yaml:"protocol_middleware,omitempty" json:"protocol_middleware,omitempty"`
	RemotePorts []int                   `yaml:"remote_ports,omitempty" json:"remote_ports,omitempty"`
	Auth        *ListenerAuth           `yaml:"auth,omitempty" json:"auth,omitempty"`
}

// ListenerAuth configures path-based auth strategies for an HTTP-visible listener.
type ListenerAuth struct {
	Rules []ListenerAuthRule `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// ListenerAuthRule matches a request path and selects an auth strategy.
type ListenerAuthRule struct {
	Path     string `yaml:"path" json:"path"`
	Type     string `yaml:"type" json:"type"`         // exact | prefix | pattern
	Strategy string `yaml:"strategy" json:"strategy"` // oidc_passthrough | headers | protected | public
}

// AppProtocolMiddleware defines protocol-specific middleware entry
type AppProtocolMiddleware struct {
	Name   string                 `yaml:"name" json:"name"`
	Params map[string]interface{} `yaml:"params,omitempty" json:"params,omitempty"`
}

// ServiceOIDCClient configures OIDC credential injection scoped to a single service.
type ServiceOIDCClient struct {
	// RedirectURIPaths declares the callback path segments for OIDC redirects.
	// Piccolo generates full redirect URIs by combining all valid access origins
	// (Remote, Alias, LAN host-based, LAN port-based) with these paths.
	// Required and must be non-empty. Paths must start with "/".
	// Example: ["/callback", "/oauth/callback"]
	// Note: Comparison is case-sensitive with no normalization per RFC 3986 §6.2.1.
	RedirectURIPaths []string `yaml:"redirect_uri_paths" json:"redirect_uri_paths"`

	// RedirectURIs declares explicit redirect URIs for native/desktop apps.
	// Only localhost (127.0.0.1, ::1) or custom scheme URIs are allowed (RFC 8252).
	// These are used as-is without origin expansion.
	// Example: ["myapp://callback", "http://localhost:8081/callback"]
	RedirectURIs []string `yaml:"redirect_uris,omitempty" json:"redirect_uris,omitempty"`

	CAMountPath string            `yaml:"ca_mount_path" json:"ca_mount_path"`
	Env         map[string]string `yaml:"env" json:"env"`
}

// AppService defines a single container within a compose-style app (service mode).
type AppService struct {
	Image       string             `yaml:"image" json:"image"`
	Init        string             `yaml:"init,omitempty" json:"init,omitempty"`
	After       []string           `yaml:"after,omitempty" json:"after,omitempty"`
	BindPorts   []int              `yaml:"bind_ports" json:"bind_ports"`
	Environment map[string]string  `yaml:"environment,omitempty" json:"environment,omitempty"`
	Storage     *AppStorage        `yaml:"storage,omitempty" json:"storage,omitempty"`
	Resources   *AppResources      `yaml:"resources,omitempty" json:"resources,omitempty"`
	OIDCClient  *ServiceOIDCClient `yaml:"oidc_client,omitempty" json:"oidc_client,omitempty"`
}

// AppStorage defines storage configuration
type AppStorage struct {
	Persistent map[string]AppVolume `yaml:"persistent,omitempty" json:"persistent,omitempty"`
	Temporary  map[string]AppVolume `yaml:"temporary,omitempty" json:"temporary,omitempty"`
}

// AppPermissions defines security permissions
type AppPermissions struct {
	Network    *AppNetworkPermissions    `yaml:"network,omitempty" json:"network,omitempty"`
	Resources  *AppResourcePermissions   `yaml:"resources,omitempty" json:"resources,omitempty"`
	Filesystem *AppFilesystemPermissions `yaml:"filesystem,omitempty" json:"filesystem,omitempty"`
	Preset     string                    `yaml:"preset,omitempty" json:"preset,omitempty"`
}

type AppNetworkPermissions struct {
	Internet       string   `yaml:"internet,omitempty" json:"internet,omitempty"` // "allow" or "deny"
	LocalNetwork   string   `yaml:"local_network,omitempty" json:"local_network,omitempty"`
	DNS            string   `yaml:"dns,omitempty" json:"dns,omitempty"`
	AllowedDomains []string `yaml:"allowed_domains,omitempty" json:"allowed_domains,omitempty"`
	AllowedIPs     []string `yaml:"allowed_ips,omitempty" json:"allowed_ips,omitempty"`
}

type AppResourcePermissions struct {
	MaxProcesses int  `yaml:"max_processes,omitempty" json:"max_processes,omitempty"`
	MaxOpenFiles int  `yaml:"max_open_files,omitempty" json:"max_open_files,omitempty"`
	Privileged   bool `yaml:"privileged,omitempty" json:"privileged,omitempty"`
}

type AppFilesystemPermissions struct {
	ReadOnlyRoot bool   `yaml:"read_only_root,omitempty" json:"read_only_root,omitempty"`
	DeviceAccess string `yaml:"device_access,omitempty" json:"device_access,omitempty"` // "allow" or "deny"
}

// AppResources defines resource limits
type AppResources struct {
	Limits *AppResourceLimits `yaml:"limits,omitempty" json:"limits,omitempty"`
}

type AppResourceLimits struct {
	Memory  string  `yaml:"memory,omitempty" json:"memory,omitempty"`
	CPU     float64 `yaml:"cpu,omitempty" json:"cpu,omitempty"`
	Storage string  `yaml:"storage,omitempty" json:"storage,omitempty"`
}

// AppHealthCheck defines health monitoring
type AppHealthCheck struct {
	HTTP *AppHTTPHealthCheck `yaml:"http,omitempty" json:"http,omitempty"`
}

type AppHTTPHealthCheck struct {
	Path    string `yaml:"path" json:"path"`
	Port    string `yaml:"port" json:"port"`
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Retries int    `yaml:"retries,omitempty" json:"retries,omitempty"`
}

// (legacy AppPort removed)

// AppVolume defines volume mapping for an application
type AppVolume struct {
	Container string `yaml:"container" json:"container"`
	Host      string `yaml:"host,omitempty" json:"host,omitempty"` // Auto-generated if not specified
	SizeLimit string `yaml:"size_limit,omitempty" json:"size_limit,omitempty"`
	Shared    bool   `yaml:"shared,omitempty" json:"shared,omitempty"`
}

// InstallAppRequest defines the request to install an app
type InstallAppRequest struct {
	AppDefinition string `json:"app_definition"` // YAML content as string
}

// CatalogItem represents an available application in the external catalog
type CatalogItem struct {
	Name          string   `json:"name" yaml:"name"`
	Description   string   `json:"description" yaml:"description"`
	Icon          string   `json:"icon" yaml:"icon"`
	Version       string   `json:"version" yaml:"version"`
	Category      string   `json:"category" yaml:"category"`
	Compatibility string   `json:"compatibility" yaml:"compatibility"` // e.g. ">= 0.1.0"
	Path          string   `json:"path" yaml:"path"`
	Maintainer    string   `json:"maintainer" yaml:"maintainer"`
	Tags          []string `json:"tags" yaml:"tags"`
	SourceURL     string   `json:"source_url,omitempty" yaml:"source_url,omitempty"`
}

// CatalogResponse defines the response for the catalog endpoint with pagination
type CatalogResponse struct {
	Apps       []CatalogItem `json:"apps"`
	Page       int           `json:"page"`
	PageSize   int           `json:"page_size"`
	Total      int           `json:"total"`
	TotalPages int           `json:"total_pages"`
}

// AppInput defines a user input field for dynamic configuration
type AppInput struct {
	Type        string              `yaml:"type" json:"type"`
	Label       string              `yaml:"label" json:"label"`
	Description string              `yaml:"description" json:"description"`
	Default     interface{}         `yaml:"default,omitempty" json:"default,omitempty"`
	Required    bool                `yaml:"required,omitempty" json:"required,omitempty"`
	Generate    bool                `yaml:"generate,omitempty" json:"generate,omitempty"`
	Validation  *AppInputValidation `yaml:"validation,omitempty" json:"validation,omitempty"`
}

// AppInputValidation defines validation rules for an input
type AppInputValidation struct {
	Regex   string `yaml:"regex" json:"regex"`
	Message string `yaml:"message" json:"message"`
}

// AppAuth defines authentication configuration for an app.
type AppAuth struct {
	// Strategy defines how the app authenticates users.
	// Values: "oidc", "headers", "none" (default: "none")
	Strategy string `yaml:"strategy,omitempty" json:"strategy,omitempty"`

	// Injection defines how OIDC credentials are injected into the app.
	Injection *AppAuthInjection `yaml:"injection,omitempty" json:"injection,omitempty"`
}

// AppAuthInjection defines how auth credentials are passed to the app.
type AppAuthInjection struct {
	// Custom mount path for CA cert
	CAMountPath string `yaml:"ca_mount_path,omitempty" json:"ca_mount_path,omitempty"`

	// Env maps environment variable names to template values.
	// Example: {"ISSUER_URL": "{{ .Auth.Issuer }}", "CLIENT_ID": "{{ .Auth.ClientID }}"}
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
}

// AuthTemplateContext provides auth-related values for app manifest templating.
type AuthTemplateContext struct {
	// Issuer is the OIDC issuer URL (e.g., "https://piccolo-<machineId>.local")
	Issuer string

	// ClientID is the dynamically registered OIDC client ID for this app
	ClientID string

	// ClientSecret is the OIDC client secret (only returned once during registration)
	ClientSecret string
}
