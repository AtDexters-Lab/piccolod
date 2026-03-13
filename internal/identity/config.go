package identity

const defaultNamekURL = "https://namek.piccolo.cloud"

// Config holds the persisted identity state for namek-managed remote access.
// Stored at {coreRoot}/network-bootstrap/remote/identity.json.
type Config struct {
	Enabled        bool     `json:"enabled"`
	NamekURL       string   `json:"namek_url"`
	DeviceID       string   `json:"device_id,omitempty"`
	Hostname       string   `json:"hostname,omitempty"`          // full FQDN: slug.baseDomain
	BaseDomain     string   `json:"base_domain,omitempty"`       // extracted from Hostname
	CustomHostname string   `json:"custom_hostname,omitempty"`   // label only (e.g., "mydevice")
	IdentityClass  string   `json:"identity_class,omitempty"`
	NexusEndpoints []string `json:"nexus_endpoints,omitempty"`
}

// CustomFQDN returns the fully qualified custom hostname, or empty if not set.
func (c Config) CustomFQDN() string {
	if c.CustomHostname != "" && c.BaseDomain != "" {
		return c.CustomHostname + "." + c.BaseDomain
	}
	return ""
}
