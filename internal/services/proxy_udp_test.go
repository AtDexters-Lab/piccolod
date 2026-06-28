package services

import (
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"piccolod/internal/services/middleware"
)

func TestMakeFlowKey(t *testing.T) {
	t.Run("ipv4_deterministic", func(t *testing.T) {
		addr := &net.UDPAddr{IP: net.ParseIP("192.168.1.100"), Port: 12345}
		k1 := makeFlowKey(addr, udpPacketInfo{})
		k2 := makeFlowKey(addr, udpPacketInfo{})
		if k1 != k2 {
			t.Error("same address should produce same key")
		}
	})

	t.Run("ipv4_different_port", func(t *testing.T) {
		a := &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 1000}
		b := &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 1001}
		if makeFlowKey(a, udpPacketInfo{}) == makeFlowKey(b, udpPacketInfo{}) {
			t.Error("different ports should produce different keys")
		}
	})

	t.Run("ipv4_different_ip", func(t *testing.T) {
		a := &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 53}
		b := &net.UDPAddr{IP: net.ParseIP("10.0.0.2"), Port: 53}
		if makeFlowKey(a, udpPacketInfo{}) == makeFlowKey(b, udpPacketInfo{}) {
			t.Error("different IPs should produce different keys")
		}
	})

	t.Run("ipv6_deterministic", func(t *testing.T) {
		addr := &net.UDPAddr{IP: net.ParseIP("::1"), Port: 8080}
		k1 := makeFlowKey(addr, udpPacketInfo{})
		k2 := makeFlowKey(addr, udpPacketInfo{})
		if k1 != k2 {
			t.Error("same address should produce same key")
		}
	})

	t.Run("ipv4_mapped_ipv6_matches_ipv4", func(t *testing.T) {
		// net.IPv4() and net.ParseIP() both return IPv4-mapped-IPv6 internally.
		// To16() normalizes both to the same 16-byte form.
		v4 := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 80}
		v4mapped := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 80}
		if makeFlowKey(v4, udpPacketInfo{}) != makeFlowKey(v4mapped, udpPacketInfo{}) {
			t.Error("IPv4 and IPv4-mapped-IPv6 should produce the same key")
		}
	})

	t.Run("ipv4_does_not_collide_with_ipv6", func(t *testing.T) {
		// IPv4 1.2.3.4 (To16: ::ffff:1.2.3.4) must not collide with
		// IPv6 102:304:: which has a similar raw byte prefix.
		v4 := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 53}
		v6 := &net.UDPAddr{IP: net.ParseIP("102:304::"), Port: 53}
		if makeFlowKey(v4, udpPacketInfo{}) == makeFlowKey(v6, udpPacketInfo{}) {
			t.Error("IPv4 and IPv6 with similar prefix should not collide")
		}
	})

	t.Run("local_destination_distinguishes_flows", func(t *testing.T) {
		src := &net.UDPAddr{IP: net.ParseIP("192.168.0.144"), Port: 46967}
		a := udpPacketInfo{localIP: net.ParseIP("192.168.0.200"), ifIndex: 2}
		b := udpPacketInfo{localIP: net.ParseIP("192.168.0.201"), ifIndex: 2}
		if makeFlowKey(src, a) == makeFlowKey(src, b) {
			t.Error("same source to different local IPs should produce different keys")
		}
	})

	t.Run("local_interface_distinguishes_flows", func(t *testing.T) {
		src := &net.UDPAddr{IP: net.ParseIP("192.168.0.144"), Port: 46967}
		a := udpPacketInfo{localIP: net.ParseIP("192.168.0.201"), ifIndex: 2}
		b := udpPacketInfo{localIP: net.ParseIP("192.168.0.201"), ifIndex: 3}
		if makeFlowKey(src, a) == makeFlowKey(src, b) {
			t.Error("same source to same local IP on different interfaces should produce different keys")
		}
	})
}

func TestUDPPacketConnPreservesIPv4LocalDestination(t *testing.T) {
	raw, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("local UDP sockets unavailable: %v", err)
		}
		t.Fatalf("listen udp4: %v", err)
	}
	conn, err := wrapUDPPacketConn(raw, "udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		_ = raw.Close()
		t.Skipf("IPv4 packet info unavailable: %v", err)
	}
	defer conn.Close()

	port := raw.LocalAddr().(*net.UDPAddr).Port
	targetIP := net.IPv4(127, 0, 0, 2)
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("local UDP sockets unavailable: %v", err)
		}
		t.Fatalf("listen client udp4: %v", err)
	}
	defer client.Close()

	deadline := time.Now().Add(2 * time.Second)
	_ = raw.SetReadDeadline(deadline)
	_ = client.SetReadDeadline(deadline)

	if _, err := client.WriteToUDP([]byte("ping"), &net.UDPAddr{IP: targetIP, Port: port}); err != nil {
		t.Skipf("loopback alias %s unavailable: %v", targetIP, err)
	}

	buf := make([]byte, 16)
	n, src, info, err := conn.ReadFromUDP(buf)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Skipf("loopback alias %s did not route to wildcard UDP socket", targetIP)
		}
		t.Fatalf("read server datagram: %v", err)
	}
	if got := string(buf[:n]); got != "ping" {
		t.Fatalf("server payload = %q, want ping", got)
	}
	if !info.localIP.Equal(targetIP) {
		t.Fatalf("packet local IP = %v, want %v", info.localIP, targetIP)
	}

	if _, err := conn.WriteToUDP([]byte("pong"), src, info); err != nil {
		t.Fatalf("write server reply: %v", err)
	}
	n, from, err := client.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read client reply: %v", err)
	}
	if got := string(buf[:n]); got != "pong" {
		t.Fatalf("client payload = %q, want pong", got)
	}
	if !from.IP.Equal(targetIP) {
		t.Fatalf("reply source IP = %v, want %v", from.IP, targetIP)
	}
}

func TestIPv4ControlMessageSourceSelection(t *testing.T) {
	restoreAddrs := func(addrs []net.Addr) func() {
		resetUDPInterfaceAddrCacheForTest()
		old := udpInterfaceAddrsByIndex
		udpInterfaceAddrsByIndex = func(int) ([]net.Addr, error) {
			return addrs, nil
		}
		return func() {
			udpInterfaceAddrsByIndex = old
			resetUDPInterfaceAddrCacheForTest()
		}
	}

	t.Run("uses_assigned_unicast_destination_as_source", func(t *testing.T) {
		restore := restoreAddrs([]net.Addr{
			&net.IPNet{IP: net.ParseIP("192.168.0.201"), Mask: net.CIDRMask(24, 32)},
		})
		defer restore()

		cm := (udpPacketInfo{localIP: net.ParseIP("192.168.0.201"), ifIndex: 2}).ipv4ControlMessage()
		if cm == nil {
			t.Fatal("control message = nil, want source/interface hints")
		}
		if !cm.Src.Equal(net.ParseIP("192.168.0.201").To4()) {
			t.Fatalf("Src = %v, want 192.168.0.201", cm.Src)
		}
		if cm.IfIndex != 2 {
			t.Fatalf("IfIndex = %d, want 2", cm.IfIndex)
		}
	})

	t.Run("skips_limited_broadcast_source", func(t *testing.T) {
		restore := restoreAddrs([]net.Addr{
			&net.IPNet{IP: net.ParseIP("192.168.0.201"), Mask: net.CIDRMask(24, 32)},
		})
		defer restore()

		cm := (udpPacketInfo{localIP: net.IPv4(255, 255, 255, 255), ifIndex: 2}).ipv4ControlMessage()
		if cm == nil {
			t.Fatal("control message = nil, want interface hint")
		}
		if cm.Src != nil {
			t.Fatalf("Src = %v, want nil for limited broadcast", cm.Src)
		}
		if cm.IfIndex != 2 {
			t.Fatalf("IfIndex = %d, want 2", cm.IfIndex)
		}
	})

	t.Run("skips_directed_broadcast_source", func(t *testing.T) {
		restore := restoreAddrs([]net.Addr{
			&net.IPNet{IP: net.ParseIP("192.168.0.200"), Mask: net.CIDRMask(24, 32)},
			&net.IPNet{IP: net.ParseIP("192.168.0.201"), Mask: net.CIDRMask(24, 32)},
		})
		defer restore()

		cm := (udpPacketInfo{localIP: net.ParseIP("192.168.0.255"), ifIndex: 2}).ipv4ControlMessage()
		if cm == nil {
			t.Fatal("control message = nil, want interface hint")
		}
		if cm.Src != nil {
			t.Fatalf("Src = %v, want nil for directed broadcast", cm.Src)
		}
		if cm.IfIndex != 2 {
			t.Fatalf("IfIndex = %d, want 2", cm.IfIndex)
		}
	})

	t.Run("skips_multicast_source", func(t *testing.T) {
		restore := restoreAddrs([]net.Addr{
			&net.IPNet{IP: net.ParseIP("192.168.0.201"), Mask: net.CIDRMask(24, 32)},
		})
		defer restore()

		cm := (udpPacketInfo{localIP: net.ParseIP("224.0.0.251"), ifIndex: 2}).ipv4ControlMessage()
		if cm == nil {
			t.Fatal("control message = nil, want interface hint")
		}
		if cm.Src != nil {
			t.Fatalf("Src = %v, want nil for multicast", cm.Src)
		}
		if cm.IfIndex != 2 {
			t.Fatalf("IfIndex = %d, want 2", cm.IfIndex)
		}
	})
}

func TestUDPPacketInfoCachesInterfaceAddressOwnership(t *testing.T) {
	resetUDPInterfaceAddrCacheForTest()
	defer resetUDPInterfaceAddrCacheForTest()

	old := udpInterfaceAddrsByIndex
	calls := 0
	udpInterfaceAddrsByIndex = func(int) ([]net.Addr, error) {
		calls++
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("192.168.0.201"), Mask: net.CIDRMask(24, 32)},
		}, nil
	}
	defer func() {
		udpInterfaceAddrsByIndex = old
	}()

	first := makeUDPPacketInfo(net.ParseIP("192.168.0.201"), 2)
	second := makeUDPPacketInfo(net.ParseIP("192.168.0.201"), 2)
	if !first.sourceIP.Equal(net.ParseIP("192.168.0.201")) {
		t.Fatalf("first sourceIP = %v, want 192.168.0.201", first.sourceIP)
	}
	if !second.sourceIP.Equal(net.ParseIP("192.168.0.201")) {
		t.Fatalf("second sourceIP = %v, want 192.168.0.201", second.sourceIP)
	}
	if calls != 1 {
		t.Fatalf("interface address lookups = %d, want 1", calls)
	}
}

func TestUDPPacketInfoFromContextCarriesKnownNilReplySource(t *testing.T) {
	resetUDPInterfaceAddrCacheForTest()
	defer resetUDPInterfaceAddrCacheForTest()

	old := udpInterfaceAddrsByIndex
	udpInterfaceAddrsByIndex = func(int) ([]net.Addr, error) {
		t.Fatal("udpPacketInfoFromContext recomputed interface ownership")
		return nil, nil
	}
	defer func() {
		udpInterfaceAddrsByIndex = old
	}()

	info := udpPacketInfoFromContext(middleware.UDPContext{
		Local:              &net.UDPAddr{IP: net.ParseIP("192.168.0.255"), Port: 53},
		LocalIfIndex:       2,
		ReplySourceIPKnown: true,
	})
	cm := info.ipv4ControlMessage()
	if cm == nil {
		t.Fatal("control message = nil, want interface hint")
	}
	if cm.Src != nil {
		t.Fatalf("Src = %v, want nil for known unsafe reply source", cm.Src)
	}
	if cm.IfIndex != 2 {
		t.Fatalf("IfIndex = %d, want 2", cm.IfIndex)
	}
}

func resetUDPInterfaceAddrCacheForTest() {
	udpInterfaceAddrCache.Lock()
	udpInterfaceAddrCache.byIndex = nil
	udpInterfaceAddrCache.Unlock()
}
