package mdns

import (
	"net"
	"sync/atomic"
	"testing"
)

// TestCheckInterfaceChanges_TransientIPLossDoesNotReconfigure verifies the
// 13-day-silent-failure fix from RFC 20260505: when an interface loses its
// IP transiently (carrier flaps, DHCP renewal), the manager must NOT
// reconfigure the interface every 10s. The dampener requires 3-of-3 ticks.
func TestCheckInterfaceChanges_TransientIPLossDoesNotReconfigure(t *testing.T) {
	interfaceFuncsMu.Lock()
	origList := listNetworkInterfaces
	origAddrs := interfaceAddrs

	iface := net.Interface{Name: "eth0", Flags: net.FlagUp | net.FlagMulticast}
	listNetworkInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{iface}, nil
	}

	// First populate addrs with an IP, then return empty (transient loss).
	var hasIP atomic.Bool
	hasIP.Store(true)
	ipnet := &net.IPNet{IP: net.ParseIP("192.168.1.10"), Mask: net.CIDRMask(24, 32)}
	interfaceAddrs = func(*net.Interface) ([]net.Addr, error) {
		if hasIP.Load() {
			return []net.Addr{ipnet}, nil
		}
		return nil, nil // IP lost
	}
	interfaceFuncsMu.Unlock()

	t.Cleanup(func() {
		interfaceFuncsMu.Lock()
		listNetworkInterfaces = origList
		interfaceAddrs = origAddrs
		interfaceFuncsMu.Unlock()
	})

	mgr := NewManager()
	mgr.ipv4SocketFactory = func(*net.Interface) (*net.UDPConn, error) {
		return net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	}
	mgr.ipv6SocketFactory = func(*net.Interface) (*net.UDPConn, error) { return nil, nil }
	t.Cleanup(func() { _ = mgr.Stop() })

	if err := mgr.discoverInterfaces(); err != nil {
		t.Fatalf("discoverInterfaces: %v", err)
	}

	state, ok := mgr.interfaces["eth0"]
	if !ok {
		t.Fatal("expected eth0 to be set up")
	}
	originalConn := state.IPv4Conn
	if originalConn == nil {
		t.Fatal("expected IPv4Conn to be open after discovery")
	}

	// Now simulate IP loss while interface flag still Up. Run 2 ticks —
	// less than 3-of-3 — and verify connection is NOT reconfigured.
	hasIP.Store(false)
	mgr.checkInterfaceChanges()
	mgr.checkInterfaceChanges()
	if state.IPv4Conn != originalConn {
		t.Errorf("connection reconfigured after only 2 ticks of IP loss; want stable until 3-of-3")
	}
	if state.IPLossTicks != 2 {
		t.Errorf("IPLossTicks = %d, want 2", state.IPLossTicks)
	}

	// Third consecutive tick crosses the threshold → reconfigure path
	// (counter resets to 0 once reconfigure fires; conn closure observable
	// via the original conn becoming unwritable).
	mgr.checkInterfaceChanges()
	if state.IPLossTicks != 0 {
		t.Errorf("IPLossTicks after 3rd tick = %d, want 0 (reconfigure path)", state.IPLossTicks)
	}
	if err := pingConn(originalConn); err == nil {
		t.Errorf("expected original connection to be closed after reconfigure")
	}
}

// pingConn returns an error if the conn is closed (a write to a closed conn
// returns "use of closed network connection").
func pingConn(c *net.UDPConn) error {
	_, err := c.WriteTo([]byte("ping"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1})
	return err
}

// TestCheckInterfaceChanges_IPRecoveryResetsCounter verifies that an IP
// recovery within the 3-tick window resets the dampener — short flaps do
// not accrue toward the 3-of-3 threshold.
func TestCheckInterfaceChanges_IPRecoveryResetsCounter(t *testing.T) {
	interfaceFuncsMu.Lock()
	origList := listNetworkInterfaces
	origAddrs := interfaceAddrs

	iface := net.Interface{Name: "eth0", Flags: net.FlagUp | net.FlagMulticast}
	listNetworkInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{iface}, nil
	}

	var hasIP atomic.Bool
	hasIP.Store(true)
	ipnet := &net.IPNet{IP: net.ParseIP("192.168.1.10"), Mask: net.CIDRMask(24, 32)}
	interfaceAddrs = func(*net.Interface) ([]net.Addr, error) {
		if hasIP.Load() {
			return []net.Addr{ipnet}, nil
		}
		return nil, nil
	}
	interfaceFuncsMu.Unlock()

	t.Cleanup(func() {
		interfaceFuncsMu.Lock()
		listNetworkInterfaces = origList
		interfaceAddrs = origAddrs
		interfaceFuncsMu.Unlock()
	})

	mgr := NewManager()
	mgr.ipv4SocketFactory = func(*net.Interface) (*net.UDPConn, error) {
		return net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	}
	mgr.ipv6SocketFactory = func(*net.Interface) (*net.UDPConn, error) { return nil, nil }
	t.Cleanup(func() { _ = mgr.Stop() })

	if err := mgr.discoverInterfaces(); err != nil {
		t.Fatalf("discoverInterfaces: %v", err)
	}
	state := mgr.interfaces["eth0"]

	hasIP.Store(false)
	mgr.checkInterfaceChanges()
	mgr.checkInterfaceChanges()
	if state.IPLossTicks != 2 {
		t.Errorf("IPLossTicks = %d, want 2", state.IPLossTicks)
	}
	hasIP.Store(true)
	mgr.checkInterfaceChanges()
	if state.IPLossTicks != 0 {
		t.Errorf("IPLossTicks after recovery = %d, want 0", state.IPLossTicks)
	}
}
