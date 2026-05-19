package services

import (
	"testing"

	"piccolod/internal/api"
)

// TestIsEligibleForHostRouting covers RFC 20260316 (per-flow eligibility),
// plan §D8 (tcp+raw via TLS mux SNI), and RFC 20260519 (raw listener
// tls_wrap opt-in: tcp+raw is host-routable only when tls_wrap=true).
func TestIsEligibleForHostRouting(t *testing.T) {
	tests := []struct {
		name string
		l    api.AppListener
		want bool
	}{
		// HTTP / Websocket on tcp — always eligible (L7 chain TLS-terminates on :443).
		{"http tcp", api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP}, true},
		{"http tcp tls_wrap ignored", api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, TLSWrap: true}, true},
		{"websocket tcp", api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolWebsocket}, true},
		{"websocket tcp tls_wrap ignored", api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolWebsocket, TLSWrap: true}, true},

		// flow:tls — always eligible regardless of protocol or tls_wrap.
		{"http tls", api.AppListener{Flow: api.FlowTLS, Protocol: api.ListenerProtocolHTTP}, true},
		{"websocket tls", api.AppListener{Flow: api.FlowTLS, Protocol: api.ListenerProtocolWebsocket}, true},
		{"raw tls", api.AppListener{Flow: api.FlowTLS, Protocol: api.ListenerProtocolRaw}, true},
		{"raw tls tls_wrap ignored", api.AppListener{Flow: api.FlowTLS, Protocol: api.ListenerProtocolRaw, TLSWrap: true}, true},

		// flow:tcp + protocol:raw — eligible only when tls_wrap=true (RFC 20260519).
		{"raw tcp no wrap", api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw}, false},
		{"raw tcp with wrap", api.AppListener{Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw, TLSWrap: true}, true},

		// flow:udp — never eligible (port-based routing only).
		{"raw udp no wrap", api.AppListener{Flow: api.FlowUDP, Protocol: api.ListenerProtocolRaw}, false},
		{"raw udp tls_wrap ignored", api.AppListener{Flow: api.FlowUDP, Protocol: api.ListenerProtocolRaw, TLSWrap: true}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsEligibleForHostRouting(tt.l); got != tt.want {
				t.Errorf("IsEligibleForHostRouting(flow=%v protocol=%v tlsWrap=%v) = %v, want %v", tt.l.Flow, tt.l.Protocol, tt.l.TLSWrap, got, tt.want)
			}
		})
	}
}
