package autounlock

import (
	"context"
	"sync"
	"time"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

// ManagerOps is the slice of crypt.Manager that the orchestrator depends on.
// Defined as an interface here so the orchestrator can be unit-tested with
// a fake without pulling the full crypt package into tests.
type ManagerOps interface {
	IsLocked() bool
	WrapSDEKForEscrow(F []byte, aad []byte) ([]byte, error)
	UnwrapSDEKWithEscrow(blob []byte, F []byte, aad []byte) error
}

// NamekEscrowClient is the slice of namekclient.Client that the orchestrator
// depends on. Same testability rationale.
type NamekEscrowClient interface {
	DepositUnlockEscrow(ctx context.Context, secret []byte, windowSeconds int) (*namekclient.DepositUnlockEscrowResponse, error)
	PickupUnlockEscrow(ctx context.Context) (*namekclient.PickupUnlockEscrowResponse, error)
	RevokeUnlockEscrow(ctx context.Context) error
}

// AuditEmitter is the audit-event publication callback. Wraps the server's
// publishAuditEvent so the autounlock package doesn't depend on the events
// bus directly. Best-effort — emit failures are swallowed by the underlying
// pipeline.
type AuditEmitter func(kind string, details map[string]any)

// Deps wires the orchestrator to its environment. All callbacks are mandatory
// except Now (defaults to time.Now). Keeping deps as function callbacks (not
// concrete services) avoids circular imports and keeps the package testable.
type Deps struct {
	// Manager provides Wrap/Unwrap and locked-state queries.
	Manager ManagerOps

	// NamekClient supplies the live namek client. Returns nil when the
	// identity service hasn't yet brought the client up — orchestrator
	// methods bail with service_not_ready when that happens.
	NamekClient func() NamekEscrowClient

	// GetDeviceID returns the persisted device.id. Used as part of the AAD
	// passed to Wrap/Unwrap to bind the blob to this device.
	GetDeviceID func() string

	// GetIdentityClass returns the persisted identity_class
	// ("hardware_tpm" / "software_tpm" / ""). Used for posture eligibility.
	GetIdentityClass func() string

	// IsIdentityReady reports whether the identity service is in a state
	// where namek calls will work (enrolled + enabled + not suspended +
	// client non-nil). Ceremony / pickup gate on this.
	IsIdentityReady func() bool

	// PublishAudit emits an audit event. Best-effort.
	PublishAudit AuditEmitter

	// Now is the clock source. Tests inject a fake.
	Now func() time.Time
}

// Orchestrator owns the autounlock package's mutating operations. A single
// instance per piccolod process. The internal mutex serializes ceremony /
// test / posture-change against each other; pickup runs at startup only and
// cannot interleave with ceremony (which runs at shutdown only).
type Orchestrator struct {
	deps Deps
	mu   sync.Mutex
}

// New constructs an Orchestrator and ensures the on-disk state directory
// exists. Returns an error only on filesystem failure.
func New(deps Deps) (*Orchestrator, error) {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if err := EnsureStateDir(); err != nil {
		return nil, err
	}
	return &Orchestrator{deps: deps}, nil
}

// aad assembles the AEAD additional-authenticated-data for wrap/unwrap. Binds
// the blob to a specific device — closes cross-device blob replay.
const aadSuffix = "auto_unlock_v1"

func (o *Orchestrator) aad() []byte {
	return []byte(o.deps.GetDeviceID() + "|" + aadSuffix)
}
