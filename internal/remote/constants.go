package remote

// CertStatus represents the status of a certificate in the inventory.
type CertStatus string

const (
	CertStatusPending CertStatus = "pending"
	CertStatusOK      CertStatus = "ok"
	CertStatusError   CertStatus = "error"
)

// RemoteState represents the aggregate remote access state.
type RemoteState string

const (
	RemoteStateActive            RemoteState = "active"
	RemoteStateWarning           RemoteState = "warning"
	RemoteStateDisabled          RemoteState = "disabled"
	RemoteStateStopped           RemoteState = "stopped"
	RemoteStatePreflightRequired RemoteState = "preflight_required"
	RemoteStateError             RemoteState = "error"
)

// PreflightStatus represents the result of a single preflight check.
type PreflightStatus string

const (
	PreflightPass PreflightStatus = "pass"
	PreflightFail PreflightStatus = "fail"
	PreflightWarn PreflightStatus = "warn"
)
