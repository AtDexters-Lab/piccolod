// Package nmagent implements an org.freedesktop.NetworkManager.SecretAgent
// service that piccolod registers with NM's AgentManager. It answers
// GetSecrets calls during connection activation when NM falls through
// to the need-auth path — the documented escape hatch for cases where
// inline secrets in AddAndActivateConnection's dict are not honored
// (observed on brcmfmac AP→STA mode transitions on RPi 400; see RFC
// docs/rfc/20260507-wifi-secret-agent.md).
//
// The agent is additive defense-in-depth, not a replacement for the
// inline-PSK path. nmclient.Connect continues to set psk in the dict;
// the agent only fires when NM enters need-auth.
package nmagent

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"

	"piccolod/internal/network/nmclient"
)

// SecretAgent D-Bus interface, well-known object path, and the
// AgentManager interface piccolod calls to register itself.
const (
	secretAgentInterface = "org.freedesktop.NetworkManager.SecretAgent"
	secretAgentPath      = dbus.ObjectPath("/org/freedesktop/NetworkManager/SecretAgent")

	agentManagerBus      = "org.freedesktop.NetworkManager"
	agentManagerPath     = dbus.ObjectPath("/org/freedesktop/NetworkManager/AgentManager")
	agentManagerIface    = "org.freedesktop.NetworkManager.AgentManager"
	agentRegisterMethod  = agentManagerIface + ".RegisterWithCapabilities"
	agentUnregisterCall  = agentManagerIface + ".Unregister"

	agentIdentifier = "piccolod-wifi"

	wifiSecuritySetting  = "802-11-wireless-security"
	wifiSetting          = "802-11-wireless"

	// NM_SECRET_AGENT_GET_SECRETS_FLAG_*
	flagRequestNew uint32 = 0x2

	// AgentManager.RegisterWithCapabilities capabilities flag — none
	// (we do not handle VPN hints or other extensions).
	agentCapabilitiesNone uint32 = 0

	// Lifecycle backoff schedule
	backoffMin = 1 * time.Second
	backoffMax = 30 * time.Second
)

type agentState int32

const (
	stateLost agentState = iota
	stateRegistered
)

// busDialer abstracts opening a private system bus so tests can swap it.
type busDialer func() (*dbus.Conn, error)

// realBusDialer opens a real private system bus connection via nmclient's
// shared helper. The private connection is required so signals from this
// agent's lifecycle goroutine don't fan out across nmclient's shared bus.
func realBusDialer() (*dbus.Conn, error) {
	return nmclient.OpenPrivateSystemBus()
}

// Agent is the NetworkManager SecretAgent owned by piccolod.
//
// Lifecycle is two-phase per RFC D7:
//   New(): Phase 1 — open private bus, subscribe to NameOwnerChanged and
//          disconnect signal, export the SecretAgent object. Always succeeds
//          when D-Bus is reachable. Does NOT call AgentManager.Register.
//   Start(): Phase 2 — launch the lifecycle goroutine which attempts
//            AgentManager.RegisterWithCapabilities and drives recovery on
//            NM-name churn / bus-disconnect.
//
// Lifecycle invariants:
//   - Start MUST precede Stop. Stop without prior Start is supported but
//     Start-after-Stop is not — the lifecycle goroutine launched after
//     Stop has closed the bus would spin against a dead connection
//     forever. The Manager owner pairs them correctly via stopOnce.
//   - All cache and lifecycle methods are safe for concurrent use.
type Agent struct {
	dialer busDialer

	// Lifecycle goroutine context.
	lcCtx    context.Context
	lcCancel context.CancelFunc
	lcDone   chan struct{}

	// Mutates only on the lifecycle goroutine (or while it is not running).
	// Read by GetSecrets via stateLoad which uses an atomic underneath.
	state atomic.Int32

	// Mutex protects bus + sigCh during teardown/rebuild.
	busMu sync.Mutex
	conn  *dbus.Conn
	sigCh chan *dbus.Signal

	cacheMu sync.Mutex
	cache   map[string]string

	connectInFlight atomic.Bool

	// startOnce gates Start; stopOnce gates Stop.
	startOnce sync.Once
	stopOnce  sync.Once
}

// New opens a private system-bus connection, subscribes to the relevant
// signals, and exports the SecretAgent methods. Returns an error only if
// D-Bus itself is unreachable. Does not call AgentManager.Register —
// that is Start's job, and Start can recover from initial registration
// failures via the lifecycle goroutine.
func New() (*Agent, error) {
	return newWithDialer(realBusDialer)
}

func newWithDialer(d busDialer) (*Agent, error) {
	a := &Agent{
		dialer: d,
		cache:  make(map[string]string),
		lcDone: make(chan struct{}),
	}
	a.state.Store(int32(stateLost))

	// Disable coredumps process-wide. The cache holds plaintext PSKs
	// during a Connect window; a SIGSEGV mid-Connect would otherwise
	// write the secret to a coredump file. The systemd unit is expected
	// to set LimitCORE=0, but the unit ships from the OS image (not this
	// repo) and a runtime soft-disable here closes the gap regardless.
	// Best-effort: a setrlimit failure (e.g., ulimit -c hard cap > 0
	// inherited and capped) is logged but does not block agent startup.
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0}); err != nil {
		log.Printf("WARN: nmagent: setrlimit RLIMIT_CORE=0 failed: %v (cached PSK could leak via coredump)", err)
	}

	if err := a.openBus(); err != nil {
		return nil, err
	}
	return a, nil
}

// openBus dials a fresh private bus, subscribes to NameOwnerChanged, and
// exports the SecretAgent methods on it. Closes any existing connection
// before reopening. On error a.sigCh is left as nil so the lifecycle
// loop's `case <-a.sigCh` is disabled (Go's nil-channel-in-select
// semantics) and the retry timer drives recovery — without this, a
// previously-installed-and-now-closed sigCh would always win the select
// with `nil` and spin the goroutine. busMu must NOT be held by the caller.
//
// Bus-disconnect detection: godbus delivers the local signal
// org.freedesktop.DBus.Local.Disconnected on the regular signal channel
// when the bus connection drops (dbus daemon restart, broken pipe). The
// lifecycle loop filters for it; no separate channel.
func (a *Agent) openBus() error {
	a.busMu.Lock()
	defer a.busMu.Unlock()

	if a.conn != nil {
		_ = a.conn.Close()
	}
	// Reset both before attempting; if dialer/auth/export fails we leave
	// the agent in a "no bus" state — sigCh nil disables the signal case
	// in the lifecycle loop's select; the retry timer reopens later.
	a.conn = nil
	a.sigCh = nil

	conn, err := a.dialer()
	if err != nil {
		return err
	}

	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchSender("org.freedesktop.DBus"),
		dbus.WithMatchArg(0, agentManagerBus),
	); err != nil {
		conn.Close()
		return fmt.Errorf("nmagent: add NameOwnerChanged match: %w", err)
	}

	sigCh := make(chan *dbus.Signal, 16)
	conn.Signal(sigCh)

	if err := conn.Export(a, secretAgentPath, secretAgentInterface); err != nil {
		conn.Close()
		return fmt.Errorf("nmagent: export SecretAgent: %w", err)
	}

	a.conn = conn
	a.sigCh = sigCh
	return nil
}

// Start launches the lifecycle goroutine that attempts AgentManager.Register
// and drives recovery on NM-name churn / bus-disconnect. Safe to call once;
// subsequent calls are no-ops.
func (a *Agent) Start(ctx context.Context) {
	a.startOnce.Do(func() {
		a.lcCtx, a.lcCancel = context.WithCancel(ctx)
		go a.lifecycleLoop()
	})
}

// Stop best-effort unregisters from AgentManager and closes the private bus.
// Idempotent.
func (a *Agent) Stop(ctx context.Context) {
	a.stopOnce.Do(func() {
		if a.lcCancel != nil {
			a.lcCancel()
			<-a.lcDone
		}
		// Best-effort unregister; ignore errors — bus may already be dead.
		// Skip the D-Bus call entirely when we're in Lost: NM is unreachable,
		// the call would just time out at 2s for nothing, and we're about to
		// close the bus anyway. Saves shutdown latency on appliances where
		// piccolod is shut down concurrently with NM going away.
		a.busMu.Lock()
		conn := a.conn
		a.busMu.Unlock()
		if conn != nil {
			if a.IsRegistered() {
				obj := conn.Object(agentManagerBus, agentManagerPath)
				ctxCall, cancel := context.WithTimeout(ctx, 2*time.Second)
				_ = obj.CallWithContext(ctxCall, agentUnregisterCall, 0).Err
				cancel()
			}
			_ = conn.Close()
		}
	})
}

// Stash inserts a passphrase into the cache keyed by SSID. Empty values
// are rejected with a WARN log (defense against an empty-Stash bug at
// the call site). Safe for concurrent use.
func (a *Agent) Stash(ssid, psk string) {
	if ssid == "" || psk == "" {
		log.Printf("WARN: nmagent: Stash rejected — empty %s",
			emptyField(ssid, psk))
		return
	}
	a.cacheMu.Lock()
	a.cache[ssid] = psk
	a.cacheMu.Unlock()
}

func emptyField(ssid, psk string) string {
	switch {
	case ssid == "" && psk == "":
		return "ssid + psk"
	case ssid == "":
		return "ssid"
	default:
		return "psk"
	}
}

// Drain removes a cache entry. No-op if absent. Safe for concurrent use.
func (a *Agent) Drain(ssid string) {
	a.cacheMu.Lock()
	delete(a.cache, ssid)
	a.cacheMu.Unlock()
}

// SetConnectInFlight toggles the predicate read by GetSecrets's
// single-cache-entry-fallback (RFC D6). Caller pairs true with eventual
// false (Manager.Connect uses defer). Safe for concurrent use.
func (a *Agent) SetConnectInFlight(v bool) {
	a.connectInFlight.Store(v)
}

// IsRegistered reports whether the agent is currently registered with NM.
// Used by tests and diagnostics; the GetSecrets handler reads the same
// state directly.
func (a *Agent) IsRegistered() bool {
	return agentState(a.state.Load()) == stateRegistered
}

// GetSecrets is the SecretAgent D-Bus method NM calls during need-auth.
// Per RFC D6 the response is gated by, in order:
//  1. Lifecycle: state must be Registered.
//  2. setting_name == "802-11-wireless-security" (we only handle WiFi PSK).
//  3. flags & REQUEST_NEW == 0 (don't loop on a stale secret).
//  4. SSID lookup: exact-byte match against the cache, OR
//     single-cache-entry fallback gated on connectInFlight==true (handles
//     SSID byte/UTF-8 divergence at the cost of a defined-window leak).
//  5. Cached PSK non-empty.
//
// Any failure returns an empty result so NM falls through cleanly.
func (a *Agent) GetSecrets(
	settings map[string]map[string]dbus.Variant,
	path dbus.ObjectPath,
	settingName string,
	hints []string,
	flags uint32,
) (map[string]map[string]dbus.Variant, *dbus.Error) {
	ssid := nmclient.SSIDFromSettings(settings)

	if agentState(a.state.Load()) != stateRegistered {
		log.Printf("INFO: nmagent: GetSecrets ssid_len=%d setting=%q served=false reason=not_registered",
			len(ssid), settingName)
		return emptyResponse(), nil
	}
	if settingName != wifiSecuritySetting {
		log.Printf("INFO: nmagent: GetSecrets ssid_len=%d setting=%q served=false reason=wrong_setting",
			len(ssid), settingName)
		return emptyResponse(), nil
	}
	if flags&flagRequestNew != 0 {
		log.Printf("INFO: nmagent: GetSecrets ssid_len=%d setting=%q served=false reason=request_new",
			len(ssid), settingName)
		return emptyResponse(), nil
	}

	psk, reason := a.lookup(ssid)
	if psk == "" {
		log.Printf("INFO: nmagent: GetSecrets ssid_len=%d setting=%q served=false reason=%s",
			len(ssid), settingName, reason)
		return emptyResponse(), nil
	}

	log.Printf("INFO: nmagent: GetSecrets ssid_len=%d setting=%q served=true reason=ok",
		len(ssid), settingName)
	return map[string]map[string]dbus.Variant{
		wifiSecuritySetting: {
			"psk": dbus.MakeVariant(psk),
		},
	}, nil
}

// lookup consults the cache. Returns (psk, "") on hit. On miss, returns
// ("", reason) where reason is one of: empty_cache, unknown_ssid.
// Implements the single-cache-entry-fallback per D6.
func (a *Agent) lookup(ssid string) (string, string) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()

	if len(a.cache) == 0 {
		return "", "empty_cache"
	}

	if ssid != "" {
		if psk, ok := a.cache[ssid]; ok {
			return psk, ""
		}
	}

	// Single-cache-entry-fallback: covers SSID byte/UTF-8 divergence cases
	// (trailing whitespace, non-NFC normalization, hidden-SSID broadcast
	// as empty, embedded non-UTF-8 bytes). Only fires when piccolod has
	// exactly one Connect in flight (atomic predicate).
	if len(a.cache) == 1 && a.connectInFlight.Load() {
		for _, psk := range a.cache {
			return psk, ""
		}
	}

	return "", "unknown_ssid"
}

// CancelGetSecrets is invoked by NM if it loses interest before our
// response. We don't run async secret-fetch — Stash is synchronous and
// completes before AddAndActivate — so this is a no-op.
func (a *Agent) CancelGetSecrets(path dbus.ObjectPath, settingName string) *dbus.Error {
	log.Printf("INFO: nmagent: CancelGetSecrets path=%s setting=%q (no-op)", path, settingName)
	return nil
}

// SaveSecrets is called by NM if it wants the agent to persist secrets.
// We don't maintain shadow storage — NM persists to its own keyfile —
// so this is a no-op.
func (a *Agent) SaveSecrets(settings map[string]map[string]dbus.Variant, path dbus.ObjectPath) *dbus.Error {
	log.Printf("INFO: nmagent: SaveSecrets path=%s (no-op)", path)
	return nil
}

// DeleteSecrets is called by NM if it wants the agent to forget secrets.
// We don't maintain shadow storage; cache is per-Connect-attempt and
// drained by Manager.Connect.
func (a *Agent) DeleteSecrets(settings map[string]map[string]dbus.Variant, path dbus.ObjectPath) *dbus.Error {
	log.Printf("INFO: nmagent: DeleteSecrets path=%s (no-op)", path)
	return nil
}

func emptyResponse() map[string]map[string]dbus.Variant {
	return map[string]map[string]dbus.Variant{}
}

// lifecycleLoop runs on a goroutine started by Start. It drives the
// state machine in RFC D7: tries Register; reacts to NM-name churn and
// bus-disconnect; backs off on transient failures.
//
// The retryTimer path owns recovery for both "Register failed against a
// healthy bus" (just retry tryRegister) AND "openBus failed" (a.conn is
// nil → retry openBus before tryRegister). Folding both into one path
// guarantees a single backoff schedule and a single re-entry point —
// without this, a transient openBus failure inside the bus-disconnect
// handler would leave the agent unrecoverable until process restart.
//
// `a.sigCh` may be nil between a failed openBus and a successful retry;
// Go's nil-channel-in-select semantics disable that case automatically.
func (a *Agent) lifecycleLoop() {
	defer close(a.lcDone)

	backoff := backoffMin

	// First attempt: bus is already open from New(); register immediately.
	if a.tryRegister() {
		backoff = backoffMin
	}

	for {
		// Snapshot the channel each iteration; openBus replaces it on
		// bus-disconnect. nil disables the signal case via Go's
		// nil-channel-in-select semantics.
		a.busMu.Lock()
		sigCh := a.sigCh
		busAlive := a.conn != nil
		a.busMu.Unlock()

		// retryTimer fires whenever recovery work is pending: state is
		// Lost (Register hasn't succeeded) OR the bus itself is gone
		// (openBus needs a retry). Healthy + Registered = no timer.
		var retryTimer <-chan time.Time
		if !busAlive || agentState(a.state.Load()) == stateLost {
			retryTimer = time.After(backoff)
		}

		select {
		case <-a.lcCtx.Done():
			return

		case sig, ok := <-sigCh:
			if !ok {
				// godbus closes every registered signal channel via
				// defaultSignalHandler.Terminate() when the bus
				// connection drops (dbus daemon restart, broken pipe,
				// godbus internal failure). This is the primary
				// bus-disconnect mechanism; godbus v5 does NOT emit a
				// synthetic Local.Disconnected signal struct. Treat the
				// closed channel as bus-death: transition to Lost and
				// clear bus state so retryTimer drives reopen.
				a.handleBusDisconnect()
				backoff = backoffMin
				continue
			}
			if sig == nil {
				// Defensive: nil-Signal on an open channel shouldn't
				// happen with godbus v5, but skip rather than fault.
				continue
			}
			if isLocalDisconnected(sig) {
				// Defense-in-depth: if a future godbus version starts
				// emitting Local.Disconnected as a real signal struct
				// instead of (or in addition to) closing the channel,
				// handle it the same way as the closed-channel path.
				a.handleBusDisconnect()
				backoff = backoffMin
				continue
			}
			if isNameOwnerChangedForNM(sig) {
				_, _, newOwner := parseNameOwnerChanged(sig)
				if newOwner == "" {
					a.transitionLost("nm_owner_lost")
					backoff = backoffMin
				} else if a.tryRegister() {
					backoff = backoffMin
				}
			}

		case <-retryTimer:
			// Reopen the bus first if it's gone; only attempt Register
			// when the bus is healthy.
			if !busAlive {
				if err := a.openBus(); err != nil {
					log.Printf("WARN: nmagent: bus reopen failed: %v (will retry in %v)", err, backoff)
					backoff = nextBackoff(backoff)
					continue
				}
				log.Printf("INFO: nmagent: bus reopened")
			}
			if a.tryRegister() {
				backoff = backoffMin
			} else {
				backoff = nextBackoff(backoff)
			}
		}
	}
}

// tryRegister attempts AgentManager.RegisterWithCapabilities. Updates state
// on success. Returns true on success. Does not log on success vs failure
// transition equally — register-success and register-failure each get one
// INFO log; register-failure-while-already-Lost is silent backoff churn.
func (a *Agent) tryRegister() bool {
	a.busMu.Lock()
	conn := a.conn
	a.busMu.Unlock()
	if conn == nil {
		return false
	}

	obj := conn.Object(agentManagerBus, agentManagerPath)
	ctx, cancel := context.WithTimeout(a.lcCtx, 5*time.Second)
	defer cancel()

	err := obj.CallWithContext(ctx, agentRegisterMethod, 0,
		agentIdentifier, agentCapabilitiesNone).Err
	if err != nil {
		// Don't log on every backoff retry; only on transitions.
		if agentState(a.state.Load()) == stateRegistered {
			log.Printf("WARN: nmagent: registration lost: %v", err)
			a.state.Store(int32(stateLost))
		}
		return false
	}

	if agentState(a.state.Load()) != stateRegistered {
		log.Printf("INFO: nmagent: registered with NM as %q", agentIdentifier)
		a.state.Store(int32(stateRegistered))
	}
	return true
}

// transitionLost moves to Lost state and logs once on transition.
func (a *Agent) transitionLost(reason string) {
	if agentState(a.state.Load()) != stateLost {
		log.Printf("INFO: nmagent: state -> Lost (%s)", reason)
		a.state.Store(int32(stateLost))
	}
}

// handleBusDisconnect tears down the dead bus connection so the retry
// timer in lifecycleLoop can reopen it on the next iteration. State is
// transitioned BEFORE the close so GetSecrets fails closed against any
// handler dispatching on the about-to-die conn. Do NOT call openBus
// inline — coupling reopen to bus-death recovery would let a transient
// openBus failure leave us spinning on a stale (closed) signal channel.
func (a *Agent) handleBusDisconnect() {
	a.transitionLost("bus_disconnect")
	a.busMu.Lock()
	if a.conn != nil {
		_ = a.conn.Close()
	}
	a.conn = nil
	a.sigCh = nil
	a.busMu.Unlock()
}

// nextBackoff doubles backoff up to backoffMax.
func nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > backoffMax {
		return backoffMax
	}
	return next
}

// isLocalDisconnected reports whether the signal is godbus's local
// disconnect notification (org.freedesktop.DBus.Local.Disconnected,
// emitted when the bus connection drops).
func isLocalDisconnected(sig *dbus.Signal) bool {
	return sig.Name == "org.freedesktop.DBus.Local.Disconnected"
}

// isNameOwnerChangedForNM reports whether the signal is the NameOwnerChanged
// for the org.freedesktop.NetworkManager bus name. Defensive against the
// match rule above being too permissive on some bus implementations.
func isNameOwnerChangedForNM(sig *dbus.Signal) bool {
	if sig.Name != "org.freedesktop.DBus.NameOwnerChanged" {
		return false
	}
	if len(sig.Body) < 1 {
		return false
	}
	name, ok := sig.Body[0].(string)
	if !ok {
		return false
	}
	return name == agentManagerBus
}

// parseNameOwnerChanged returns (name, oldOwner, newOwner). Returns empty
// strings on parse failure; callers should still operate (newOwner == ""
// means "name has no owner" which matches the NM-disappeared case).
func parseNameOwnerChanged(sig *dbus.Signal) (string, string, string) {
	if len(sig.Body) < 3 {
		return "", "", ""
	}
	name, _ := sig.Body[0].(string)
	oldOwner, _ := sig.Body[1].(string)
	newOwner, _ := sig.Body[2].(string)
	return name, oldOwner, newOwner
}
