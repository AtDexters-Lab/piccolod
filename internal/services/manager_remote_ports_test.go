package services

import (
	"reflect"
	"sort"
	"testing"

	"piccolod/internal/api"
)

func intPtr(v int) *int { return &v }

// TestDeriveRemotePorts covers RFC 20260519 §D2: RemotePorts derivation
// unions port_claim, applies the HTTP-flavor default [80, 443] only when
// the union is empty AND the protocol is http/websocket, and adds 443 for
// tls_wrap:true raw listeners. Raw listeners with neither tls_wrap nor
// port_claim and no explicit RemotePorts end up empty (and thus
// unreachable from outside), closing the byte-leak in matchesRemotePort.
func TestDeriveRemotePorts(t *testing.T) {
	tests := []struct {
		name     string
		listener api.AppListener
		want     []int
	}{
		{
			name:     "http_no_claim_no_explicit",
			listener: api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
			want:     []int{80, 443},
		},
		{
			name:     "websocket_no_claim_no_explicit",
			listener: api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolWebsocket},
			want:     []int{80, 443},
		},
		{
			name:     "http_explicit_only",
			listener: api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, RemotePorts: []int{8080}},
			want:     []int{8080},
		},
		{
			name:     "http_with_port_claim_no_explicit",
			listener: api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, PortClaim: intPtr(8080)},
			want:     []int{8080},
		},
		{
			name:     "http_with_port_claim_80",
			listener: api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, PortClaim: intPtr(80)},
			want:     []int{80},
		},
		{
			name:     "raw_with_port_claim_no_wrap",
			listener: api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(2222)},
			want:     []int{2222},
		},
		{
			name:     "raw_with_port_claim_and_wrap",
			listener: api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(2222), TLSWrap: true},
			want:     []int{2222, 443},
		},
		{
			name:     "raw_no_port_claim_with_wrap",
			listener: api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw, TLSWrap: true},
			want:     []int{443},
		},
		{
			name:     "raw_no_port_claim_no_wrap",
			listener: api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw},
			want:     []int{},
		},
		{
			name:     "explicit_plus_port_claim_unique",
			listener: api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, RemotePorts: []int{8080, 9090}, PortClaim: intPtr(2222)},
			want:     []int{8080, 9090, 2222},
		},
		{
			name:     "explicit_overlaps_port_claim_dedup",
			listener: api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, RemotePorts: []int{8080, 2222}, PortClaim: intPtr(2222)},
			want:     []int{8080, 2222},
		},
		{
			name:     "raw_explicit_overlaps_wrap_443",
			listener: api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw, RemotePorts: []int{443}, TLSWrap: true},
			want:     []int{443},
		},
		{
			name:     "tls_passthrough_explicit",
			listener: api.AppListener{Flow: api.FlowTLS, Protocol: api.ListenerProtocolRaw, RemotePorts: []int{8443}, PortClaim: intPtr(8443)},
			want:     []int{8443},
		},
		// flow:tls is host-routing-eligible regardless of protocol; an
		// omitted remote_ports must default to [80, 443] per the spec
		// ("If remote_ports is omitted, Piccolo defaults to [80, 443]").
		// Regression for the P1 found during gating review: pre-fix this
		// derived [] and broke TLS-mux routing on 443 for the canonical
		// "I do my own TLS, route by SNI" listener shape.
		{
			name:     "tls_passthrough_default_no_claim_no_explicit",
			listener: api.AppListener{Flow: api.FlowTLS, Protocol: api.ListenerProtocolRaw},
			want:     []int{80, 443},
		},
		{
			name:     "tls_with_http_protocol_default",
			listener: api.AppListener{Flow: api.FlowTLS, Protocol: api.ListenerProtocolHTTP},
			want:     []int{80, 443},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveRemotePorts(tt.listener)
			if got == nil {
				got = []int{}
			}
			want := append([]int{}, tt.want...)
			gotCopy := append([]int{}, got...)
			sort.Ints(want)
			sort.Ints(gotCopy)
			if !reflect.DeepEqual(want, gotCopy) {
				t.Errorf("deriveRemotePorts() = %v, want %v (order-insensitive)", got, tt.want)
			}
		})
	}
}

// TestMatchesRemotePortByteLeakRegression is the byte-leak regression
// for RFC 20260519. A raw listener with port_claim:2222 (e.g., gitea SSH)
// must NOT match port 80 — the old default-to-(80, 443) behavior on
// empty RemotePorts would have caused external HTTP requests to be
// forwarded into the SSH socket.
func TestMatchesRemotePortByteLeakRegression(t *testing.T) {
	// Build the endpoint exactly as buildEndpoint would for an
	// ssh-git listener — RemotePorts derived from the listener.
	sshListener := api.AppListener{
		Name:      "ssh-git",
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolRaw,
		PortClaim: intPtr(2222),
	}
	sshEp := ServiceEndpoint{
		App:         "gitea",
		Name:        sshListener.Name,
		Flow:        sshListener.Flow,
		Protocol:    sshListener.Protocol,
		RemotePorts: deriveRemotePorts(sshListener),
		PortClaim:   sshListener.PortClaim,
	}

	if got := matchesRemotePort(sshEp, 80); got {
		t.Errorf("matchesRemotePort(ssh-git, 80) = true; want false (byte-leak regression — port 80 must not route to a raw SSH listener)")
	}
	if got := matchesRemotePort(sshEp, 443); got {
		t.Errorf("matchesRemotePort(ssh-git, 443) = true; want false (raw without tls_wrap should not match 443)")
	}
	if got := matchesRemotePort(sshEp, 2222); !got {
		t.Errorf("matchesRemotePort(ssh-git, 2222) = false; want true (direct-port access regression)")
	}

	// ACMEHTTPFallbackPort normalization: 5002 → 80 in matchesRemotePort.
	// For the same ssh listener, fallback port must also not match.
	if got := matchesRemotePort(sshEp, ACMEHTTPFallbackPort); got {
		t.Errorf("matchesRemotePort(ssh-git, %d) = true; want false (ACME fallback must not match raw listener)", ACMEHTTPFallbackPort)
	}

	// Defense-in-depth assertion (RFC 20260519 §D2 + post-review tightening):
	// the empty-RemotePorts branch must return false for every non-zero
	// port. The legacy fallback (return remotePort == 80 || == 443) was
	// load-bearing only for HTTP listeners that hit the old empty-default
	// branch; deriveRemotePorts now provides those defaults at endpoint
	// construction, leaving the runtime branch dead-on-purpose. Asserting
	// it stays dead prevents a future refactor from silently restoring
	// the byte-leak surface.
	unreachableEp := ServiceEndpoint{App: "x", Name: "y", RemotePorts: nil}
	for _, p := range []int{80, 443, 22, 53, 8080, ACMEHTTPFallbackPort} {
		if got := matchesRemotePort(unreachableEp, p); got {
			t.Errorf("matchesRemotePort(empty RemotePorts, %d) = true; want false (empty list must be unreachable, not fall back to 80/443)", p)
		}
	}

	// Wrapped variant: same listener but with tls_wrap=true should match 443
	// (TLS mux path) while still matching the port_claim.
	wrappedListener := sshListener
	wrappedListener.TLSWrap = true
	wrappedEp := sshEp
	wrappedEp.RemotePorts = deriveRemotePorts(wrappedListener)
	if got := matchesRemotePort(wrappedEp, 443); !got {
		t.Errorf("matchesRemotePort(wrapped raw, 443) = false; want true (tls_wrap=true should add 443)")
	}
	if got := matchesRemotePort(wrappedEp, 2222); !got {
		t.Errorf("matchesRemotePort(wrapped raw, 2222) = false; want true (port_claim preserved alongside tls_wrap)")
	}
	if got := matchesRemotePort(wrappedEp, 80); got {
		t.Errorf("matchesRemotePort(wrapped raw, 80) = true; want false (tls_wrap only adds 443, not 80)")
	}
}
