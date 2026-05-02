package autounlock

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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

	// IsIdentityReady reports whether the identity service is in a state
	// where namek calls will work (enrolled + enabled + not suspended +
	// client non-nil). Ceremony / pickup gate on this.
	IsIdentityReady func() bool

	// PublishAudit emits an audit event. Best-effort.
	PublishAudit AuditEmitter

	// Now is the clock source. Tests inject a fake.
	Now func() time.Time

	// PickupIdentityWaitTimeout overrides the per-pickup wait for identity
	// readiness. Optional; defaults to 60s. Tests inject 100ms so the
	// IsIdentityReady-always-false path doesn't make the suite hang.
	PickupIdentityWaitTimeout time.Duration
}

// Orchestrator owns the autounlock package's mutating operations. A single
// instance per piccolod process. The internal mutex serializes ceremony /
// test / state-change / pickup against each other. Pickup typically runs
// at startup and completes before Stop fires, but a fast Stop catching pickup
// mid-namek-call requires the wiring layer to cancel pickup's context first
// so pickup releases the mutex promptly.
type Orchestrator struct {
	deps Deps
	mu   sync.Mutex

	// inFlight reports whether a pickup attempt is currently running. Read
	// (without taking o.mu) by handleCryptoStatus so the locked-screen UI
	// can show the transient "Auto-unlocking…" state. Set true at RunPickup
	// entry, cleared via defer.
	inFlight atomic.Bool
}

// InFlight reports whether a pickup attempt is currently running. Read by
// handleCryptoStatus to drive the UI's transient "Auto-unlocking" state.
// Lock-free atomic load — safe to call concurrently with RunPickup.
func (o *Orchestrator) InFlight() bool {
	return o.inFlight.Load()
}

// New constructs an Orchestrator and ensures the on-disk state directory
// exists. Returns an error only on filesystem failure.
func New(deps Deps) (*Orchestrator, error) {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.PickupIdentityWaitTimeout == 0 {
		deps.PickupIdentityWaitTimeout = pickupIdentityWaitTimeout
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

// UpdateInput is the partial-update shape consumed by the HTTP PUT handler.
// All fields are optional pointers — nil means "leave unchanged."
type UpdateInput struct {
	Enabled    *bool             `json:"enabled,omitempty"`
	AutoReboot *AutoRebootUpdate `json:"auto_reboot,omitempty"`
}

// AutoRebootUpdate is the partial-update shape for the nested auto_reboot
// block. Same pointer convention.
type AutoRebootUpdate struct {
	Enabled         *bool `json:"enabled,omitempty"`
	WindowStartHour *int  `json:"window_start_hour,omitempty"`
	WindowEndHour   *int  `json:"window_end_hour,omitempty"`
}

// ErrInvalidWindow is returned when a PUT supplies an out-of-range or
// equal-bounds window. Caller maps to 400.
var ErrInvalidWindow = errors.New("autounlock: invalid window — hours must be 0..23, start != end")
