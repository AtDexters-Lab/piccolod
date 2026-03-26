package services

import (
	"net"
	"testing"
)

func TestMakeFlowKey(t *testing.T) {
	t.Run("ipv4_deterministic", func(t *testing.T) {
		addr := &net.UDPAddr{IP: net.ParseIP("192.168.1.100"), Port: 12345}
		k1 := makeFlowKey(addr)
		k2 := makeFlowKey(addr)
		if k1 != k2 {
			t.Error("same address should produce same key")
		}
	})

	t.Run("ipv4_different_port", func(t *testing.T) {
		a := &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 1000}
		b := &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 1001}
		if makeFlowKey(a) == makeFlowKey(b) {
			t.Error("different ports should produce different keys")
		}
	})

	t.Run("ipv4_different_ip", func(t *testing.T) {
		a := &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 53}
		b := &net.UDPAddr{IP: net.ParseIP("10.0.0.2"), Port: 53}
		if makeFlowKey(a) == makeFlowKey(b) {
			t.Error("different IPs should produce different keys")
		}
	})

	t.Run("ipv6_deterministic", func(t *testing.T) {
		addr := &net.UDPAddr{IP: net.ParseIP("::1"), Port: 8080}
		k1 := makeFlowKey(addr)
		k2 := makeFlowKey(addr)
		if k1 != k2 {
			t.Error("same address should produce same key")
		}
	})

	t.Run("ipv4_mapped_ipv6_matches_ipv4", func(t *testing.T) {
		// net.IPv4() and net.ParseIP() both return IPv4-mapped-IPv6 internally.
		// To16() normalizes both to the same 16-byte form.
		v4 := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 80}
		v4mapped := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 80}
		if makeFlowKey(v4) != makeFlowKey(v4mapped) {
			t.Error("IPv4 and IPv4-mapped-IPv6 should produce the same key")
		}
	})

	t.Run("ipv4_does_not_collide_with_ipv6", func(t *testing.T) {
		// IPv4 1.2.3.4 (To16: ::ffff:1.2.3.4) must not collide with
		// IPv6 102:304:: which has a similar raw byte prefix.
		v4 := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 53}
		v6 := &net.UDPAddr{IP: net.ParseIP("102:304::"), Port: 53}
		if makeFlowKey(v4) == makeFlowKey(v6) {
			t.Error("IPv4 and IPv6 with similar prefix should not collide")
		}
	})
}
