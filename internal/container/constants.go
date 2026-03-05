package container

// Shared constants for per-app user isolation and rootless Podman configuration.
const (
	// RuntimeUsername is the system user under which all rootless Podman
	// commands execute. The daemon requires this user to exist at startup.
	RuntimeUsername = "piccolo-runtime"

	// AppUserPrefix is the username prefix for per-app Linux users.
	// The full username is pa-<instanceID> (truncated + hashed if too long).
	AppUserPrefix = "pa-"

	// SubUIDBase is the starting offset for per-app subuid/subgid ranges.
	// Above piccolo-runtime's 100000:65536.
	SubUIDBase = uint32(200000)

	// SubUIDRangeSize is the number of subordinate UIDs/GIDs per app user.
	SubUIDRangeSize = uint32(65536)
)
