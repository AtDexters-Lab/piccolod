package autounlock

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

// ManagerOps is the slice of crypt.Manager that the orchestrator depends on.
// Defined as an interface here so the orchestrator can be unit-tested with
// a fake without pulling the full crypt package into tests.
type ManagerOps interface {
	SDEKLoaded() bool
	WrapSDEKForEscrow(F []byte, aad []byte) ([]byte, error)
	UnwrapSDEKWithEscrow(blob []byte, F []byte, aad []byte) error
}

// NamekEscrowClient is the slice of namekclient.Client that the orchestrator
// depends on. Same testability rationale.
type NamekEscrowClient interface {
	DepositUnlockEscrow(ctx context.Context, secret []byte, windowSeconds int) (*namekclient.DepositUnlockEscrowResponse, error)
	PickupUnlockEscrow(ctx context.Context) (*namekclient.PickupUnlockEscrowResponse, error)
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

	// RecoveryProvider and RecoveryProviderID are the provider-neutral
	// injection point. Existing production wiring may leave them nil; New then
	// adapts NamekClient as the v1 provider without changing its wire format.
	RecoveryProvider   func() RecoveryFactorProvider
	RecoveryProviderID string

	// GetDeviceID returns the persisted device.id. Used as part of the AAD
	// passed to Wrap/Unwrap to bind the blob to this device.
	GetDeviceID func() string

	// IsIdentityReady reports whether the identity service is in a state
	// where namek calls will work (enrolled + enabled + not suspended +
	// client non-nil). Ceremony / pickup gate on this.
	IsIdentityReady func() bool

	// WaitForIdentityReady blocks until IsIdentityReady() returns true,
	// timeout elapses, or ctx is cancelled. Returns the final readiness.
	// Production wiring at gin_server.go subscribes to TopicIdentityReady +
	// TopicIdentityChanged so pickup wakes immediately on enrollment
	// completion (vs the prior 1s-poll which added per-boot UX latency).
	// Optional; when nil, pickup falls back to 1s polling on
	// IsIdentityReady. Tests inject a fake.
	WaitForIdentityReady func(ctx context.Context, timeout time.Duration) bool

	// Provider readiness defaults to the identity callbacks above for the
	// Namek v1 adapter. A future provider can supply independent readiness
	// callbacks without leaking its transport into the continuity core.
	IsRecoveryProviderReady      func() bool
	WaitForRecoveryProviderReady func(ctx context.Context, timeout time.Duration) bool

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
// instance per piccolod process. A context-aware token gate serializes
// ceremony / prepare / pickup / test / settings / cleanup against each other.
// Critical recovery callers can therefore honor their absolute deadline while
// waiting instead of blocking indefinitely behind provider I/O.
type Orchestrator struct {
	deps Deps
	gate chan struct{}

	// restartHandoffClaimDigest is volatile ownership of the exact encrypted
	// handoff committed by a successful graceful or fatal restart prepare. It
	// is read and written only while holding gate. Binding the claim to the raw
	// blob digest prevents a later replacement from inheriting stale ownership.
	restartHandoffClaimDigest string

	// taskWarningHandoffClaimDigest is volatile ownership of the exact blob
	// created by task-Warning preparation. Normal pressure may clean up only
	// when the current raw blob still matches this digest. Pre-existing or
	// later replacement handoffs therefore cannot inherit Warning cleanup.
	taskWarningHandoffClaimDigest string

	// inFlight reports whether a pickup attempt is currently running. Read
	// (without taking the operation gate) by handleCryptoStatus so the locked-screen UI
	// can show the transient "Auto-unlocking…" state. Set true at RunPickup
	// entry, cleared via defer.
	inFlight atomic.Bool

	// lastTestAt timestamps the most recent RunTest invocation. Read while the
	// operation gate is held to enforce the plan-mandated ≥5s gap between
	// successive Test calls per device. Closes the "admin script clicks
	// Test in a loop" surface that bombards namek and starves the
	// continuity operation gate.
	lastTestAt time.Time
}

// InFlight reports whether a pickup attempt is currently running. Read by
// handleCryptoStatus to drive the UI's transient "Auto-unlocking" state.
// Lock-free atomic load — safe to call concurrently with RunPickup.
func (o *Orchestrator) InFlight() bool {
	return o.inFlight.Load()
}

// New constructs an Orchestrator and ensures the on-disk state directory
// exists. Custom recovery providers must use their own non-empty protocol ID.
func New(deps Deps) (*Orchestrator, error) {
	if deps.RecoveryProvider != nil && (deps.RecoveryProviderID == "" || deps.RecoveryProviderID == namekV1ProviderID) {
		return nil, ErrInvalidRecoveryProviderID
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.PickupIdentityWaitTimeout == 0 {
		deps.PickupIdentityWaitTimeout = pickupIdentityWaitTimeout
	}
	if err := EnsureStateDir(); err != nil {
		return nil, err
	}
	o := &Orchestrator{
		deps: deps,
		gate: make(chan struct{}, 1),
	}
	o.gate <- struct{}{}
	return o, nil
}

func (o *Orchestrator) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.gate:
		if err := ctx.Err(); err != nil {
			o.release()
			return err
		}
		return nil
	}
}

func (o *Orchestrator) release() {
	o.gate <- struct{}{}
}

// aad assembles the AEAD additional-authenticated-data for wrap/unwrap. Binds
// the blob to a specific device — closes cross-device blob replay.
const aadSuffix = "auto_unlock_v1"

// ErrAADIncomplete signals an empty device.id; AAD would not be device-bound.
// Production callers gate via IsIdentityReady so reaching this is defense in
// depth against a future bypass.
var ErrAADIncomplete = errors.New("autounlock: device.id empty — AAD would be constant")

func (o *Orchestrator) aad() ([]byte, error) {
	id := o.deps.GetDeviceID()
	if id == "" {
		return nil, ErrAADIncomplete
	}
	return []byte(id + "|" + aadSuffix), nil
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
