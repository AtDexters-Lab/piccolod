package identity

import (
	"context"
	"log"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

// Setup discovery (mDNS fallback) — see ~/.claude/plans/purring-launching-blanket.md
// and namek-server/docs/setup-discovery-integration.md.
//
// While first-run setup is incomplete, this device publishes its private LAN
// IPs to namek every 30s. piccolospace.com/setup matches a caller's public IP
// against setup-mode devices and surfaces the LAN IPs so the user can navigate
// to http://<lan_ip>. This closes the gap on networks where mDNS is broken.
//
// Server TTL is 120s, so 3 missed heartbeats drop the device out of discover.
// On completion the loop sends a terminal setup_complete=true heartbeat and
// exits. On service shutdown the loop bails without a terminal send and the
// row ages out via TTL.

// var, not const, so tests can shorten without waiting 30 seconds.
//
// setupHeartbeatInitialDelay is intentionally 15s rather than 10s: the
// existing endpointSyncLoop also uses a 10s initial delay, and its 5min
// (300s) period together with this 30s heartbeat period would phase-align
// every cycle if both started at the same offset, bursting 4 namek
// requests at once and tripping the 2 req/s per-IP rate limit. With a 5s
// offset the two schedules never collide (LCM analysis: 30k+15 ≠ 300m+10
// for any integer k,m).
var (
	setupHeartbeatInterval       = 30 * time.Second
	setupHeartbeatInitialDelay   = 15 * time.Second
	setupHeartbeatTimeout        = 10 * time.Second
	stopSetupHeartbeatWaitBudget = 15 * time.Second
)

// rfc6598CGNAT is RFC 6598 100.64.0.0/10 — used by carrier NAT and not covered
// by stdlib net.IP.IsPrivate(). Server accepts it, so we publish it.
var rfc6598CGNAT = net.IPNet{
	IP:   net.IPv4(100, 64, 0, 0),
	Mask: net.CIDRMask(10, 32),
}

// maybeStartSetupHeartbeat starts the heartbeat loop if a setup-complete check
// is wired and reports false. No-op if already running, no callback, or setup
// is already complete.
func (s *Service) maybeStartSetupHeartbeat() {
	s.mu.RLock()
	cb := s.onSetupCompleteCheck
	s.mu.RUnlock()
	if cb == nil {
		return
	}
	if cb() {
		return
	}
	s.startSetupHeartbeat()
}

// startSetupHeartbeat launches the heartbeat goroutine. Idempotent.
func (s *Service) startSetupHeartbeat() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped.Load() || !s.cfg.Enabled || s.setupHBCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.setupHBCancel = cancel
	s.setupHBDone = make(chan struct{})
	s.setupHBWake = make(chan struct{}, 1)
	s.setupHBFirstLogged.Store(false)
	done := s.setupHBDone
	wake := s.setupHBWake
	go func() {
		defer close(done)
		s.setupHeartbeatLoop(ctx, wake)
	}()
	log.Printf("INFO: identity: setup heartbeat started")
}

// NotifySetupComplete prods the heartbeat loop to re-check completion
// immediately rather than waiting up to one tick. Safe to call from any
// goroutine; no-op if the loop is not running. Coalescing — multiple rapid
// calls collapse into one wake-up.
func (s *Service) NotifySetupComplete() {
	s.mu.RLock()
	wake := s.setupHBWake
	s.mu.RUnlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

// stopSetupHeartbeat cancels the loop and waits for it to exit. Does NOT send
// a terminal heartbeat — shutdown leaves the device row to age out via TTL.
// The wait is bounded so a wedged TPM mutex (held by another component mid-
// quote) cannot stall the caller indefinitely; if the goroutine doesn't
// return within stopSetupHeartbeatWaitBudget we log and return — the goroutine
// will exit later and harmlessly close its own done channel.
func (s *Service) stopSetupHeartbeat() {
	s.mu.Lock()
	cancel := s.setupHBCancel
	done := s.setupHBDone
	s.setupHBCancel = nil
	s.setupHBDone = nil
	s.setupHBWake = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(stopSetupHeartbeatWaitBudget):
		log.Printf("WARN: identity: setup heartbeat did not exit within %v; abandoning wait", stopSetupHeartbeatWaitBudget)
	}
}

func (s *Service) setupHeartbeatLoop(ctx context.Context, wake <-chan struct{}) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(setupHeartbeatInitialDelay):
	}

	if s.checkSetupComplete() {
		s.sendSetupHeartbeat(ctx, true)
		return
	}
	s.sendSetupHeartbeat(ctx, false)

	ticker := time.NewTicker(setupHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			// Caller signaled completion; check now rather than waiting for
			// the next tick. Falls through to send-terminal if complete,
			// else loops back and waits for the next tick (a spurious wake
			// just costs an extra completion check).
			if s.checkSetupComplete() {
				s.sendSetupHeartbeat(ctx, true)
				return
			}
		case <-ticker.C:
			if s.checkSetupComplete() {
				// Sequential loop: previous send already returned, so there
				// is no in-flight heartbeat to race the terminal one (per
				// integration spec §4.3).
				s.sendSetupHeartbeat(ctx, true)
				return
			}
			s.sendSetupHeartbeat(ctx, false)
		}
	}
}

// checkSetupComplete polls the user-supplied callback. Returns true on nil
// callback so a misconfigured loop never runs forever; fail-safe semantics
// for persistence errors live inside the callback itself.
func (s *Service) checkSetupComplete() bool {
	s.mu.RLock()
	cb := s.onSetupCompleteCheck
	s.mu.RUnlock()
	if cb == nil {
		return true
	}
	return cb()
}

func (s *Service) sendSetupHeartbeat(ctx context.Context, complete bool) {
	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()
	if client == nil {
		return
	}

	req := &namekclient.HeartbeatRequest{SetupComplete: complete}
	if !complete {
		ips := collectLANIPs()
		if len(ips) == 0 {
			log.Printf("WARN: identity: setup heartbeat: no usable LAN IPs, skipping tick")
			return
		}
		req.LANIPs = ips
	}

	callCtx, cancel := context.WithTimeout(ctx, setupHeartbeatTimeout)
	defer cancel()

	if err := client.SendHeartbeat(callCtx, req); err != nil {
		// Auth-class failures (device row gone, AK changed, etc.) need the
		// existing recovery path to re-enroll immediately — otherwise the
		// device disappears from setup-discovery for up to 5 minutes until
		// the slower endpoint-sync loop notices the same 401/403.
		if status := extractHTTPStatus(err); status == 401 || status == 403 {
			s.HandleTokenError(err, status)
			return
		}
		if complete {
			log.Printf("WARN: identity: setup heartbeat (terminal) failed: %v", err)
		} else {
			log.Printf("WARN: identity: setup heartbeat failed: %v", err)
		}
		return
	}
	if complete {
		log.Printf("INFO: identity: setup heartbeat sent terminal (setup_complete=true)")
		return
	}
	// One-shot liveness log: every subsequent successful send is silent so
	// the loop doesn't spam at 30s cadence, but the first send going through
	// is the most useful single confirmation that the loop is functional.
	if s.setupHBFirstLogged.CompareAndSwap(false, true) {
		log.Printf("INFO: identity: setup heartbeat first send ok (lan_ips=%v)", req.LANIPs)
	}
}

// maxPublishedLANIPs matches the namek heartbeat endpoint's lan_ips cap.
// Sending more would 400 the request and waste a TPM quote.
const maxPublishedLANIPs = 10

// virtualInterfacePrefixes are interface name prefixes that almost always
// belong to container/VM/VPN stacks rather than the user's actual LAN. Even
// when they carry an RFC1918 address, that address is not browser-reachable
// from the user's device, so publishing it just clutters the discover
// response and risks the user clicking through to an unreachable URL.
// Mirrors the set classified as virtual in internal/mdns/interface.go.
var virtualInterfacePrefixes = []string{
	"podman", "docker", "br-", "veth", "virbr", "vmnet", // containers / VMs
	"cni", "flannel", "cali", "weave", "kube", // k8s CNIs
	"tun", "tap", "wg", "zt", "tailscale", // VPN tunnels
}

// collectLANIPs returns IPv4 addresses from real-LAN interfaces in private
// ranges accepted by namek's heartbeat endpoint (RFC1918 + RFC6598). Filters
// out loopback, point-to-point, down interfaces, and well-known virtual /
// container / VPN interface name prefixes. IPv6 is excluded entirely
// (link-local is unusable for browser navigation; ULA fc00::/7 is rare).
func collectLANIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 || ifi.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		if isVirtualInterfaceName(ifi.Name) {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			if !isAcceptedPrivateIPv4(ip4) {
				continue
			}
			out = append(out, ip4.String())
		}
	}
	// Stable order so the frontend's "primary" pick (lan_ips[0]) doesn't flip
	// across heartbeats when the interface list reorders.
	sort.Strings(out)
	if len(out) > maxPublishedLANIPs {
		out = out[:maxPublishedLANIPs]
	}
	return out
}

func isVirtualInterfaceName(name string) bool {
	for _, p := range virtualInterfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func isAcceptedPrivateIPv4(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip.IsPrivate() {
		return true
	}
	return rfc6598CGNAT.Contains(ip)
}
