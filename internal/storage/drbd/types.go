package drbd

import "piccolod/internal/state/paths"

// ResourceConfig configures a single DRBD resource.
type ResourceConfig struct {
	// Name is the DRBD resource name (e.g., "piccolo-vol-abc123").
	Name string

	// BackingDevice is the path to the lower block device (/dev/nbdN).
	BackingDevice string

	// MetaDir is the directory for external metadata files.
	// Default: paths.CoreJoin("drbd-meta")
	MetaDir string

	// ConfDir is the directory for DRBD resource config files.
	// Default: /etc/drbd.d/
	ConfDir string

	// NodeID is this node's DRBD node-id (0 for single-node).
	NodeID int
}

// ResourceState represents the current state of a DRBD resource.
type ResourceState struct {
	Name       string // resource name
	Role       string // Primary, Secondary, Unknown
	Connection string // StandAlone, Connected, Connecting, etc.
	DiskState  string // UpToDate, Diskless, Inconsistent, etc.
	DevicePath string // /dev/drbd/by-res/<name>
}

// Resource represents a DRBD resource with its configuration and state.
type Resource struct {
	Config ResourceConfig
	State  ResourceState
}

// DefaultMetaDir returns the standard DRBD metadata directory.
func DefaultMetaDir() string {
	return paths.CoreJoin("drbd-meta")
}
