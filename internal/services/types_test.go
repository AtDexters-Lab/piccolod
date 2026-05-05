package services

import (
	"testing"

	"piccolod/internal/api"
)

func TestIsEligibleForHostRouting(t *testing.T) {
	tests := []struct {
		name     string
		protocol api.ListenerProtocol
		flow     api.ListenerFlow
		want     bool
	}{
		{"http tcp", api.ListenerProtocolHTTP, api.FlowTCP, true},
		{"websocket tcp", api.ListenerProtocolWebsocket, api.FlowTCP, true},
		{"http tls", api.ListenerProtocolHTTP, api.FlowTLS, true},
		{"websocket tls", api.ListenerProtocolWebsocket, api.FlowTLS, true},
		// Plan §D8: tcp+raw is now host-routable via TLS mux SNI.
		{"raw tcp", api.ListenerProtocolRaw, api.FlowTCP, true},
		{"raw tls", api.ListenerProtocolRaw, api.FlowTLS, true},
		// Only flow:udp remains port-routed-only.
		{"raw udp", api.ListenerProtocolRaw, api.FlowUDP, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsEligibleForHostRouting(tt.protocol, tt.flow)
			if got != tt.want {
				t.Errorf("IsEligibleForHostRouting(%v, %v) = %v, want %v", tt.protocol, tt.flow, got, tt.want)
			}
		})
	}
}
