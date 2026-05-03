// Package lifecycle owns the composite system-readiness state for piccolod.
//
// Background. Before this package, callers that needed to know "is the system
// ready for unlock-dependent work?" had to derive the answer from a cascade of
// independent signals — cryptoManager.IsLocked() (SDEK loaded?), then a
// persistence query (store unlocked?), sometimes also a session check. The
// signals flip at different points during /crypto/unlock's post-decrypt chain,
// so the cascade exposed a multi-second window where the layers disagreed: a
// /system/boot poll landing in that window saw the device as "unlocked" but
// queried persistence and got ErrLocked, which was silently mapped to
// "(false, nil)" by isSetupComplete and routed an already-provisioned device
// to the first-run setup wizard.
//
// This package collapses the cascade into a single composite state owned by
// the unlock orchestration layer. Every external caller asks "is the system
// Ready?" against one source of truth, and the SDEK / persistence / data
// volume / etc. layers can flip in any order internally without exposing an
// inconsistent state to consumers.
//
// Symmetry with internal/provisioning. The provisioning package solved the
// exact-same fragmentation for "has this device finished first-run setup?";
// see internal/provisioning/state.go for the reasoning. lifecycle is the
// symmetric consolidation for "is this device unlocked & ready?", which
// provisioning explicitly does NOT cover.
//
// Ordering convention for callers. The Coordinator is the source of truth
// for composite-readiness consumers (requireUnlocked middleware, /system/boot
// routing, /auth/session.volumes_locked, /crypto/status.locked). To preserve
// that invariant, every site that mutates an underlying layer (cryptoManager
// SDEK, persistence lock-state, data-volume LUKS) AND the lifecycle must
// order them so external observers never see "Ready" while any underlying
// layer isn't:
//
//   - Moving AWAY from Ready (Lock): call Coordinator.Lock() FIRST, then
//     mutate the underlying layers. Briefly claiming "not ready" while
//     the SDEK is still loaded is safe (more conservative than reality);
//     claiming "ready" while the SDEK is gone is not.
//
//   - Moving TOWARD Ready (Unlock / Setup): mutate the underlying layers
//     FIRST, then call MarkReady. Until every layer is up, the chain
//     stays in StateUnlocking and IsReady() returns false.
//
// completeUnlockChain in gin_crypto_handlers.go demonstrates the toward-
// Ready pattern (BeginUnlock at entry, MarkReady at the end of the chain
// body). handleCryptoLock demonstrates the away-from-Ready pattern
// (Coordinator.Lock() before cryptoManager.Lock()).
package lifecycle

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// State enumerates the composite system-readiness states. The string forms
// returned by String() are part of the /system/boot wire contract — Flutter
// consumes them to drive screen sub-routing (e.g., "unlocking" → spinner).
type State int32

const (
	// StateNotInitialized: no keyset on disk. The device has never been set
	// up; UI must show the first-run setup wizard.
	StateNotInitialized State = iota
	// StateLocked: keyset present, SDEK absent. UI must show the unlock
	// password prompt (or the auto-unlocking spinner if a pickup is about
	// to begin).
	StateLocked
	// StateUnlocking: SDEK loaded, post-decrypt chain in flight. The
	// persistence store may not yet be loaded; queries can return
	// ErrLocked. UI must show a transient spinner; consumers gating on
	// "system ready" must treat this as not-ready.
	StateUnlocking
	// StateReady: post-decrypt chain complete; persistence loaded; data
	// volume mounted; queries against any unlock-dependent subsystem are
	// safe. This is the one state in which IsReady() returns true.
	StateReady
	// StateFailed: post-decrypt chain returned a non-recoverable error.
	// Typically a LUKS data volume failure on a device whose control plane
	// was already up. The UI surfaces the failure reason; the operator can
	// retry by re-entering the password.
	StateFailed
)

// String returns the wire token consumed by /system/boot. These tokens are
// part of the wire contract — changing them requires a coordinated Flutter
// update.
func (s State) String() string {
	switch s {
	case StateNotInitialized:
		return "not_initialized"
	case StateLocked:
		return "locked"
	case StateUnlocking:
		return "unlocking"
	case StateReady:
		return "ready"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// ErrInvalidTransition is returned when a state-machine transition is
// rejected. Callers can errors.Is-check this to distinguish state-machine
// programming errors from operational failures.
var ErrInvalidTransition = errors.New("lifecycle: invalid transition")

// Coordinator owns the composite system-readiness state for the daemon.
// One instance per process. State reads are lock-free (atomic.Int32);
// transitions serialize on a mutex so transition validity checks compose
// atomically with the state write.
type Coordinator struct {
	mu      sync.Mutex
	state   atomic.Int32
	failure error
}

// New constructs a Coordinator pre-populated with the bootstrap state.
// Caller derives the bootstrap from durable signals (cryptoManager
// initialization status, etc.) before any transitions can occur.
//
// Typical bootstraps:
//   - cryptoManager.IsInitialized() == false → StateNotInitialized
//   - cryptoManager.IsInitialized() == true   → StateLocked (auto-unlock
//     pickup, if any, will transition via BeginUnlock when it starts)
func New(initial State) *Coordinator {
	c := &Coordinator{}
	c.state.Store(int32(initial))
	return c
}

// State reports the current lifecycle state. Lock-free; safe to call from
// any goroutine including hot paths like /system/boot.
func (c *Coordinator) State() State {
	if c == nil {
		return StateNotInitialized
	}
	return State(c.state.Load())
}

// IsReady reports whether the system is in StateReady. This is the canonical
// "is the post-unlock plane fully usable?" query; replaces composite-readiness
// uses of cryptoManager.IsLocked() across the codebase.
func (c *Coordinator) IsReady() bool {
	return c.State() == StateReady
}

// FailureReason returns the error that drove the coordinator into
// StateFailed. Returns nil for any other state. The failure reason is
// cleared on the next successful BeginUnlock — historical failures are not
// surfaced once a fresh unlock attempt begins.
func (c *Coordinator) FailureReason() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if State(c.state.Load()) != StateFailed {
		return nil
	}
	return c.failure
}

// BeginUnlock claims ownership of the post-decrypt chain by transitioning
// to StateUnlocking. The caller is responsible for calling MarkReady or
// MarkFailed to leave the Unlocking state.
//
// Legal source states:
//   - StateLocked         (manual /crypto/unlock; auto-unlock pickup)
//   - StateNotInitialized (first-run setup, where /crypto/setup both
//                          installs the SDEK and runs the post-decrypt chain)
//   - StateFailed         (retry after a previous failed attempt)
//
// Rejected sources:
//   - StateUnlocking — a concurrent unlock attempt is already in flight;
//     caller should treat the rejection as "lost the race, bail."
//   - StateReady     — system is already up; caller should treat as a
//     no-op success rather than an error condition.
//
// Callers that race against another unlock path (manual + auto) should
// inspect State() after a rejection to distinguish the two cases.
func (c *Coordinator) BeginUnlock() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := State(c.state.Load())
	switch cur {
	case StateLocked, StateNotInitialized, StateFailed:
		c.state.Store(int32(StateUnlocking))
		c.failure = nil
		return nil
	default:
		return fmt.Errorf("%w: BeginUnlock from %s", ErrInvalidTransition, cur)
	}
}

// MarkReady transitions StateUnlocking → StateReady. Called after the
// post-decrypt chain succeeds (persistence loaded, data volume mounted).
// Rejects from any state other than Unlocking — a successful unlock chain
// must always pair with a prior BeginUnlock.
func (c *Coordinator) MarkReady() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := State(c.state.Load())
	if cur != StateUnlocking {
		return fmt.Errorf("%w: MarkReady from %s (expected %s)", ErrInvalidTransition, cur, StateUnlocking)
	}
	c.state.Store(int32(StateReady))
	c.failure = nil
	return nil
}

// MarkFailed transitions StateUnlocking → StateFailed with the given
// reason. The reason is stored for later FailureReason() retrieval. Rejects
// from any state other than Unlocking — a failure must always pair with a
// prior BeginUnlock.
//
// reason is required (a nil reason is replaced with a generic sentinel so
// FailureReason() never reports "failed but unknown why").
func (c *Coordinator) MarkFailed(reason error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := State(c.state.Load())
	if cur != StateUnlocking {
		return fmt.Errorf("%w: MarkFailed from %s (expected %s)", ErrInvalidTransition, cur, StateUnlocking)
	}
	if reason == nil {
		reason = errors.New("lifecycle: unspecified failure")
	}
	c.failure = reason
	c.state.Store(int32(StateFailed))
	return nil
}

// Lock transitions to StateLocked. Used by /crypto/lock (admin re-lock) and
// by daemon shutdown.
//
// Legal source states:
//   - StateReady, StateFailed → StateLocked (clean re-lock after a unlock
//                                            attempt's terminal state).
//   - StateLocked            → no-op (idempotent).
//
// Rejected sources:
//   - StateNotInitialized — there is nothing to lock on a fresh device.
//   - StateUnlocking      — the in-flight chain must reach a terminal state
//                            (Ready or Failed) before re-locking.
func (c *Coordinator) Lock() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := State(c.state.Load())
	switch cur {
	case StateReady, StateFailed:
		c.state.Store(int32(StateLocked))
		c.failure = nil
		return nil
	case StateLocked:
		return nil
	default:
		return fmt.Errorf("%w: Lock from %s", ErrInvalidTransition, cur)
	}
}
