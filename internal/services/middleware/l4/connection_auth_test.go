package l4

import (
	"net"
	"testing"

	"piccolod/internal/api"
	"piccolod/internal/services/middleware"
)

func TestConnectionAuth_decisionMatrix(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *api.ConnectionAuth
		ip      string
		wantOK  bool
		wantDen bool
	}{
		{
			name:    "nil config — implicit allow",
			cfg:     nil,
			ip:      "192.0.2.1",
			wantOK:  true,
			wantDen: false,
		},
		{
			name:    "default deny — no rule match",
			cfg:     &api.ConnectionAuth{Default: "deny"},
			ip:      "192.0.2.1",
			wantOK:  false,
			wantDen: true,
		},
		{
			name: "first rule wins (allow before deny)",
			cfg: &api.ConnectionAuth{
				Default: "deny",
				Rules: []api.ConnectionAuthRule{
					{Match: "192.0.2.0/24", Strategy: "allow"},
					{Match: "192.0.2.42/32", Strategy: "deny"}, // never reached for .42
				},
			},
			ip:      "192.0.2.42",
			wantOK:  true,
			wantDen: false,
		},
		{
			name: "deny rule blocks specific host",
			cfg: &api.ConnectionAuth{
				Default: "allow",
				Rules: []api.ConnectionAuthRule{
					{Match: "192.0.2.42/32", Strategy: "deny"},
				},
			},
			ip:      "192.0.2.42",
			wantOK:  false,
			wantDen: true,
		},
		{
			name:    "default allow — no rules — passes",
			cfg:     &api.ConnectionAuth{Default: "allow"},
			ip:      "192.0.2.1",
			wantOK:  true,
			wantDen: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewMetricsRegistry()
			mw, err := ConnectionAuth(tc.cfg, reg)
			if err != nil {
				t.Fatalf("ConnectionAuth: %v", err)
			}
			called := false
			term := middleware.ConnHandler(func(_ middleware.ConnContext, _ net.Conn) { called = true })
			mw(term)(mkCtx(tc.ip, nil), &stubConn{})

			if called != tc.wantOK {
				t.Errorf("terminal called = %v, want %v", called, tc.wantOK)
			}
			snap := reg.Snapshot()
			denied := snap.Denied[MetricsSample{Listener: "lstn", SourceIP: net.ParseIP(tc.ip).String(), DenyReason: "connection_auth"}]
			gotDen := denied > 0
			if gotDen != tc.wantDen {
				t.Errorf("metrics denied = %d, want recorded=%v", denied, tc.wantDen)
			}
		})
	}
}

func TestConnectionAuth_paramValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  *api.ConnectionAuth
	}{
		{"bad default", &api.ConnectionAuth{Default: "maybe"}},
		{"bad rule strategy", &api.ConnectionAuth{Rules: []api.ConnectionAuthRule{{Match: "10.0.0.0/8", Strategy: "skip"}}}},
		{"bad rule CIDR", &api.ConnectionAuth{Rules: []api.ConnectionAuthRule{{Match: "not-a-cidr", Strategy: "deny"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ConnectionAuth(tc.cfg, nil); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}
