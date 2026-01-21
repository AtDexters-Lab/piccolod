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
		{"http tls", api.ListenerProtocolHTTP, api.FlowTLS, false},
		{"websocket tls", api.ListenerProtocolWebsocket, api.FlowTLS, false},
		{"raw tcp", api.ListenerProtocolRaw, api.FlowTCP, false},
		{"raw tls", api.ListenerProtocolRaw, api.FlowTLS, false},
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
