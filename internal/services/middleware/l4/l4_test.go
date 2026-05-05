package l4

import (
	"net"
	"testing"
	"time"

	"piccolod/internal/services/middleware"
)

// stubConn implements net.Conn just enough for middleware tests — the L4
// chain treats conn opaquely; only RemoteAddr is read.
type stubConn struct {
	remote net.Addr
	closed bool
}

func (s *stubConn) Read(_ []byte) (int, error)         { return 0, nil }
func (s *stubConn) Write(_ []byte) (int, error)        { return 0, nil }
func (s *stubConn) Close() error                       { s.closed = true; return nil }
func (s *stubConn) LocalAddr() net.Addr                { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080} }
func (s *stubConn) RemoteAddr() net.Addr               { return s.remote }
func (s *stubConn) SetDeadline(_ time.Time) error      { return nil }
func (s *stubConn) SetReadDeadline(_ time.Time) error  { return nil }
func (s *stubConn) SetWriteDeadline(_ time.Time) error { return nil }

func mkCtx(sourceIP string, hint *middleware.Hint) middleware.ConnContext {
	addr := &net.TCPAddr{IP: net.ParseIP(sourceIP), Port: 12345}
	ctx := middleware.ConnContext{
		Endpoint:    middleware.EndpointInfo{App: "app", Listener: "lstn"},
		SourceAddr:  addr,
		LocalAddr:   &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080},
		AcceptedAt:  time.Now(),
		SourceTrust: middleware.Direct,
	}
	if hint != nil {
		h := *hint
		ctx.SourceTrust = middleware.TrustedLoopback
		ctx.Hint = func() (middleware.Hint, bool) { return h, true }
	}
	return ctx
}

// --- HintConsumer ---

func TestHintConsumer_replacesHintWithLazyLookup(t *testing.T) {
	want := middleware.Hint{ClientIP: "203.0.113.42", IsTLS: true, RemotePort: 8443}
	lookup := l4HintLookupStub(want)

	mw := HintConsumer(lookup, 8080)

	called := false
	terminal := middleware.ConnHandler(func(ctx middleware.ConnContext, _ net.Conn) {
		called = true
		got, ok := ctx.Hint()
		if !ok {
			t.Fatal("expected Hint to resolve, got false")
		}
		if got != want {
			t.Errorf("Hint = %+v, want %+v", got, want)
		}
	})
	mw(terminal)(middleware.ConnContext{}, &stubConn{remote: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}})

	if !called {
		t.Fatal("terminal was not invoked")
	}
}

func l4HintLookupStub(want middleware.Hint) HintLookupFn {
	return func(_, _ int) (middleware.Hint, bool) { return want, true }
}

// --- ConnMetrics ---

func TestConnMetrics_recordsReceivedForEveryConn(t *testing.T) {
	reg := NewMetricsRegistry()
	mw := ConnMetrics(reg)

	terminal := middleware.ConnHandler(func(_ middleware.ConnContext, _ net.Conn) {})
	mw(terminal)(mkCtx("192.0.2.1", nil), &stubConn{})
	mw(terminal)(mkCtx("192.0.2.1", nil), &stubConn{})
	mw(terminal)(mkCtx("192.0.2.2", nil), &stubConn{})

	snap := reg.Snapshot()
	if snap.Received[MetricsSample{Listener: "lstn", SourceIP: "192.0.2.1"}] != 2 {
		t.Errorf("received[192.0.2.1] = %d, want 2", snap.Received[MetricsSample{Listener: "lstn", SourceIP: "192.0.2.1"}])
	}
	if snap.Received[MetricsSample{Listener: "lstn", SourceIP: "192.0.2.2"}] != 1 {
		t.Errorf("received[192.0.2.2] = %d, want 1", snap.Received[MetricsSample{Listener: "lstn", SourceIP: "192.0.2.2"}])
	}
	if len(snap.Denied) != 0 {
		t.Errorf("denied should be empty, got %v", snap.Denied)
	}
}

func TestConnMetrics_recordsReceivedEvenWhenDownstreamDenies(t *testing.T) {
	// Effective accept = Received − sum(Denied per IP). Downstream rule deny
	// adds to Denied; ConnMetrics still records Received because the conn
	// reached the L4 chain.
	reg := NewMetricsRegistry()
	cm := ConnMetrics(reg)

	denyMW := middleware.L4Middleware(func(_ middleware.ConnHandler) middleware.ConnHandler {
		return middleware.ConnHandler(func(ctx middleware.ConnContext, _ net.Conn) {
			ip := middleware.EffectiveSourceIP(ctx)
			reg.RecordDenied(ctx.Endpoint.Listener, ip.String(), "test_rule")
		})
	})
	terminal := middleware.ConnHandler(func(_ middleware.ConnContext, _ net.Conn) {})
	chain := middleware.ComposeL4Chain([]middleware.L4Middleware{cm, denyMW}, terminal)
	chain(mkCtx("192.0.2.2", nil), &stubConn{})

	snap := reg.Snapshot()
	if snap.Received[MetricsSample{Listener: "lstn", SourceIP: "192.0.2.2"}] != 1 {
		t.Errorf("received = %v, want 1", snap.Received)
	}
	if snap.Denied[MetricsSample{Listener: "lstn", SourceIP: "192.0.2.2", DenyReason: "test_rule"}] != 1 {
		t.Errorf("denied = %v, want 1", snap.Denied)
	}
}

// --- IPAllowlist ---

func TestIPAllowlist_allowDenyDefault(t *testing.T) {
	cases := []struct {
		name    string
		params  map[string]any
		ip      string
		wantOK  bool
		wantDen bool
	}{
		{
			name:    "default deny — no rules — IP not matched",
			params:  map[string]any{},
			ip:      "192.0.2.1",
			wantOK:  false,
			wantDen: true,
		},
		{
			name:    "default allow — no rules — IP not matched but allowed",
			params:  map[string]any{"default": "allow"},
			ip:      "192.0.2.1",
			wantOK:  true,
			wantDen: false,
		},
		{
			name:    "allow CIDR matches",
			params:  map[string]any{"allow": []any{"192.0.2.0/24"}},
			ip:      "192.0.2.42",
			wantOK:  true,
			wantDen: false,
		},
		{
			name:    "deny CIDR overrides allow CIDR",
			params:  map[string]any{"allow": []any{"192.0.2.0/24"}, "deny": []any{"192.0.2.42/32"}},
			ip:      "192.0.2.42",
			wantOK:  false,
			wantDen: true,
		},
		{
			name:    "IPv4-mapped IPv6 source matches v4 CIDR",
			params:  map[string]any{"allow": []any{"192.0.2.0/24"}},
			ip:      "::ffff:192.0.2.42",
			wantOK:  true,
			wantDen: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewMetricsRegistry()
			mw, err := IPAllowlist(tc.params, reg)
			if err != nil {
				t.Fatalf("IPAllowlist build: %v", err)
			}
			called := false
			terminal := middleware.ConnHandler(func(_ middleware.ConnContext, _ net.Conn) { called = true })
			mw(terminal)(mkCtx(tc.ip, nil), &stubConn{})

			if called != tc.wantOK {
				t.Errorf("terminal called = %v, want %v", called, tc.wantOK)
			}
			snap := reg.Snapshot()
			denied := snap.Denied[MetricsSample{Listener: "lstn", SourceIP: net.ParseIP(tc.ip).String(), DenyReason: "ip_allowlist"}]
			gotDen := denied > 0
			if gotDen != tc.wantDen {
				t.Errorf("metrics denied count = %d, want recorded=%v", denied, tc.wantDen)
			}
		})
	}
}

func TestIPAllowlist_paramValidation(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
	}{
		{"bad CIDR", map[string]any{"allow": []any{"not-a-cidr"}}},
		{"non-string allow entry", map[string]any{"allow": []any{42}}},
		{"unknown default", map[string]any{"default": "maybe"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := IPAllowlist(tc.params, nil); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// --- IPRateLimit ---

func TestIPRateLimit_burstThenDeny(t *testing.T) {
	reg := NewMetricsRegistry()
	mw, err := IPRateLimit(map[string]any{"per_second": 1, "burst": 3}, reg)
	if err != nil {
		t.Fatalf("IPRateLimit build: %v", err)
	}
	calls := 0
	terminal := middleware.ConnHandler(func(_ middleware.ConnContext, _ net.Conn) { calls++ })

	for i := 0; i < 5; i++ {
		mw(terminal)(mkCtx("192.0.2.7", nil), &stubConn{})
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (burst)", calls)
	}
	snap := reg.Snapshot()
	denied := snap.Denied[MetricsSample{Listener: "lstn", SourceIP: "192.0.2.7", DenyReason: "ip_rate_limit"}]
	if denied != 2 {
		t.Errorf("denied count = %d, want 2", denied)
	}
}

func TestIPRateLimit_perIPIsolation(t *testing.T) {
	mw, err := IPRateLimit(map[string]any{"per_second": 1, "burst": 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	hits := map[string]int{}
	terminal := middleware.ConnHandler(func(ctx middleware.ConnContext, _ net.Conn) {
		ip := middleware.EffectiveSourceIP(ctx)
		hits[ip.String()]++
	})

	// Two IPs each get exactly one burst token.
	mw(terminal)(mkCtx("192.0.2.10", nil), &stubConn{})
	mw(terminal)(mkCtx("192.0.2.10", nil), &stubConn{}) // denied — bucket empty
	mw(terminal)(mkCtx("192.0.2.11", nil), &stubConn{}) // separate bucket — allowed

	if hits["192.0.2.10"] != 1 || hits["192.0.2.11"] != 1 {
		t.Errorf("per-IP isolation broken: %v", hits)
	}
}

func TestIPRateLimit_paramValidation(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
	}{
		{"missing per_second", map[string]any{"burst": 1}},
		{"missing burst", map[string]any{"per_second": 1}},
		{"zero per_second", map[string]any{"per_second": 0, "burst": 1}},
		{"zero burst", map[string]any{"per_second": 1, "burst": 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := IPRateLimit(tc.params, nil); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}
