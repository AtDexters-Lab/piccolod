package middleware

import (
	"net"
	"testing"
)

func TestEffectiveSourceIP_directTCP(t *testing.T) {
	ctx := ConnContext{
		SourceAddr:  &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5000},
		SourceTrust: Direct,
	}
	got := EffectiveSourceIP(ctx)
	want := net.ParseIP("192.0.2.10").To4()
	if !got.Equal(want) {
		t.Fatalf("direct TCP: want %v; got %v", want, got)
	}
	if len(got) != 4 {
		t.Fatalf("want canonical 4-byte v4; got len=%d", len(got))
	}
}

func TestEffectiveSourceIP_directUDP(t *testing.T) {
	ctx := ConnContext{
		SourceAddr:  &net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 53},
		SourceTrust: Direct,
	}
	got := EffectiveSourceIP(ctx)
	if !got.Equal(net.ParseIP("192.0.2.20")) {
		t.Fatalf("direct UDP: want 192.0.2.20; got %v", got)
	}
}

func TestEffectiveSourceIP_directIPv6(t *testing.T) {
	ctx := ConnContext{
		SourceAddr:  &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 5000},
		SourceTrust: Direct,
	}
	got := EffectiveSourceIP(ctx)
	if !got.Equal(net.ParseIP("2001:db8::1")) {
		t.Fatalf("direct IPv6: want 2001:db8::1; got %v", got)
	}
	if got.To4() != nil {
		t.Fatalf("pure IPv6 should not collapse to v4; got %v", got)
	}
}

func TestEffectiveSourceIP_ipv4MappedIPv6Canonicalized(t *testing.T) {
	// ::ffff:192.0.2.30 is an IPv4-mapped IPv6 address. EffectiveSourceIP must
	// reduce it to 4-byte v4 form so a CIDR like 192.0.2.0/24 matches.
	mapped := net.ParseIP("::ffff:192.0.2.30")
	ctx := ConnContext{
		SourceAddr:  &net.TCPAddr{IP: mapped, Port: 5000},
		SourceTrust: Direct,
	}
	got := EffectiveSourceIP(ctx)
	if len(got) != 4 {
		t.Fatalf("IPv4-mapped IPv6 should canonicalize to 4-byte form; got len=%d (%v)", len(got), got)
	}
	if !got.Equal(net.ParseIP("192.0.2.30")) {
		t.Fatalf("canonicalization mismatch: want 192.0.2.30; got %v", got)
	}
	// Verify a v4 CIDR matches it.
	_, cidr, _ := net.ParseCIDR("192.0.2.0/24")
	if !cidr.Contains(got) {
		t.Fatalf("v4 CIDR should match canonicalized v4 form; got %v not in %v", got, cidr)
	}
}

func TestEffectiveSourceIP_trustedLoopbackWithHint(t *testing.T) {
	// TrustedLoopback + valid hint → return hint.ClientIP, not loopback SourceAddr.
	ctx := ConnContext{
		SourceAddr:  &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5000},
		SourceTrust: TrustedLoopback,
		Hint: func() (Hint, bool) {
			return Hint{ClientIP: "203.0.113.5"}, true
		},
	}
	got := EffectiveSourceIP(ctx)
	if !got.Equal(net.ParseIP("203.0.113.5")) {
		t.Fatalf("hint should win over loopback SourceAddr; got %v", got)
	}
}

func TestEffectiveSourceIP_trustedLoopbackWithoutHint(t *testing.T) {
	// TrustedLoopback + no hint → fall through to SourceAddr (loopback).
	// Documented fail-closed semantics: deny rules trip on loopback.
	ctx := ConnContext{
		SourceAddr:  &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5000},
		SourceTrust: TrustedLoopback,
		Hint: func() (Hint, bool) {
			return Hint{}, false
		},
	}
	got := EffectiveSourceIP(ctx)
	if !got.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("no-hint should fall through to SourceAddr; got %v", got)
	}
}

func TestEffectiveSourceIP_trustedLoopbackEmptyClientIP(t *testing.T) {
	// TrustedLoopback + hint with empty ClientIP → fall through to SourceAddr per D14.
	// Intentional fail-closed: an empty hint provides no authoritative real-client IP.
	ctx := ConnContext{
		SourceAddr:  &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5000},
		SourceTrust: TrustedLoopback,
		Hint: func() (Hint, bool) {
			return Hint{ClientIP: ""}, true
		},
	}
	got := EffectiveSourceIP(ctx)
	if !got.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("empty ClientIP should fall through to SourceAddr; got %v", got)
	}
}

func TestEffectiveSourceIP_trustedLoopbackMalformedClientIP(t *testing.T) {
	// TrustedLoopback + hint with non-parseable ClientIP → fall through to SourceAddr.
	// Defense-in-depth: IssueRequestHint validates today, but if anything ever bypasses
	// validation we don't want to silently treat garbage as a real IP.
	ctx := ConnContext{
		SourceAddr:  &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5000},
		SourceTrust: TrustedLoopback,
		Hint: func() (Hint, bool) {
			return Hint{ClientIP: "not-an-ip"}, true
		},
	}
	got := EffectiveSourceIP(ctx)
	if !got.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("malformed ClientIP should fall through to SourceAddr; got %v", got)
	}
}

func TestEffectiveSourceIP_trustedLoopbackNilHintFunc(t *testing.T) {
	// TrustedLoopback but Hint accessor is nil (programming error or middleware
	// composition gap) → fall through to SourceAddr without panic.
	ctx := ConnContext{
		SourceAddr:  &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5000},
		SourceTrust: TrustedLoopback,
		Hint:        nil,
	}
	got := EffectiveSourceIP(ctx)
	if !got.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("nil Hint accessor should not panic; got %v", got)
	}
}

func TestEffectiveSourceIP_unknownAddrType(t *testing.T) {
	// SourceAddr is neither *net.TCPAddr nor *net.UDPAddr → return nil; caller fail-closed.
	ctx := ConnContext{
		SourceAddr:  &net.UnixAddr{Name: "/tmp/sock", Net: "unix"},
		SourceTrust: Direct,
	}
	got := EffectiveSourceIP(ctx)
	if got != nil {
		t.Fatalf("unknown SourceAddr type should return nil; got %v", got)
	}
}

func TestEffectiveSourceIP_nilSourceAddr(t *testing.T) {
	// Nil SourceAddr → return nil; caller fail-closed.
	ctx := ConnContext{
		SourceAddr:  nil,
		SourceTrust: Direct,
	}
	got := EffectiveSourceIP(ctx)
	if got != nil {
		t.Fatalf("nil SourceAddr should return nil; got %v", got)
	}
}

func TestEffectiveSourceIP_trustedLoopbackHintWinsOverNilSourceAddr(t *testing.T) {
	// Defense-in-depth: valid hint should resolve even if SourceAddr is somehow nil.
	// Production path won't hit this (hint_consumer_l4 only populates Hint when
	// there's a real conn), but the algorithm should not depend on SourceAddr
	// presence when hint is the authoritative source.
	ctx := ConnContext{
		SourceAddr:  nil,
		SourceTrust: TrustedLoopback,
		Hint: func() (Hint, bool) {
			return Hint{ClientIP: "203.0.113.99"}, true
		},
	}
	got := EffectiveSourceIP(ctx)
	if !got.Equal(net.ParseIP("203.0.113.99")) {
		t.Fatalf("hint should win even with nil SourceAddr; got %v", got)
	}
}

func TestEffectiveSourceIP_typedNilTCPAddr(t *testing.T) {
	// SourceAddr is a typed *net.TCPAddr that's nil → return nil without panic.
	var ta *net.TCPAddr
	ctx := ConnContext{
		SourceAddr:  ta,
		SourceTrust: Direct,
	}
	got := EffectiveSourceIP(ctx)
	if got != nil {
		t.Fatalf("typed-nil *net.TCPAddr should return nil; got %v", got)
	}
}
