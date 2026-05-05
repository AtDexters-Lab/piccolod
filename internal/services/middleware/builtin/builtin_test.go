package builtin

import (
	"net/http"
	"testing"

	"piccolod/internal/services/middleware"
	"piccolod/internal/services/middleware/l4"
	"piccolod/internal/services/middleware/l7"
	l7oidc "piccolod/internal/services/middleware/l7/oidc"
)

// stubDeps returns a MapDeps populated with no-op getters for every dep key
// the canonical L4 + L7 + L7Response factories read. Any factory missing a
// dep here would surface as an error from RegisterDefaults + Build, so the
// table doubles as a coverage check.
func stubDeps() middleware.MapDeps {
	metricsReg := l4.NewMetricsRegistry()
	return middleware.MapDeps{
		// L4 deps.
		DepHintLookupL4: func() any {
			return l4.HintLookupFn(func(int, int) (middleware.Hint, bool) { return middleware.Hint{}, false })
		},
		DepMetricsRegistry: func() any { return metricsReg },

		// L7 deps.
		DepHintConsumerL7:        func() any { return func(r *http.Request) *http.Request { return r } },
		DepReservedPathCallback:  func() any { return func() l7oidc.CallbackHandlerFn { return nil } },
		DepACMEHandler:           func() any { return l7.ACMEHandlerFn(func() http.Handler { return nil }) },
		DepArrivedViaTLS:         func() any { return l7.ArrivedViaTLSFn(func(*http.Request) bool { return false }) },
		DepPathAuthSnapshot:      func() any { return func() l7.PathAuthSnapshot { return l7.PathAuthSnapshot{} } },
		DepCookieContext:         func() any { return func(r *http.Request) *http.Request { return r } },
		DepOIDCAuthorizeSnapshot: func() any { return func() l7oidc.AuthorizeSnapshotState { return l7oidc.AuthorizeSnapshotState{} } },
		DepForwardHeaders:        func() any { return func(*http.Request) {} },
		DepNeedsMarker:           func() any { return l7.NeedsMarkerFn(func(*http.Request) bool { return false }) },
		DepMarkerCookie:          func() any { return l7.MarkerCookieFn(func() string { return "" }) },
	}
}

// TestRegisterDefaults_buildsFullChain verifies the registry yields the full
// canonical L7 + L7Response chains in the documented order, and that every
// factory binds successfully against the documented dep keys. This is the
// equivalence anchor for plan §H step 3 — order regressions or missing deps
// fail here before they can degrade the proxy's request-handling behavior.
func TestRegisterDefaults_buildsFullChain(t *testing.T) {
	reg := middleware.NewRegistry()
	RegisterDefaults(reg)

	spec := middleware.BuildSpec{
		Endpoint: middleware.EndpointInfo{App: "test", Listener: "test"},
		HasAuth:  true, // exercise the conditionally-canonical path_auth entry
		Deps:     stubDeps(),
	}

	got, err := reg.Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// L4: conn_metrics + hint_consumer_l4 (canonical, always-on). 2 entries.
	if len(got.L4) != 2 {
		t.Fatalf("L4 chain length: got %d, want 2", len(got.L4))
	}
	// L4UDP: conn_metrics_udp only. 1 entry.
	if len(got.L4UDP) != 1 {
		t.Fatalf("L4UDP chain length: got %d, want 1", len(got.L4UDP))
	}
	// All 10 canonical L7 entries when HasAuth=true.
	if len(got.L7) != 10 {
		t.Fatalf("L7 chain length: got %d, want 10", len(got.L7))
	}
	// All 4 response-side entries.
	if len(got.L7Response) != 4 {
		t.Fatalf("L7Response chain length: got %d, want 4", len(got.L7Response))
	}
}

// TestRegisterDefaults_pathAuthGated verifies that when HasAuth=false, the
// path_auth canonical entry is skipped. Composition-blindness check: path_auth
// is the only conditionally-canonical L7 entry today, so chain length drops by
// exactly one.
func TestRegisterDefaults_pathAuthGated(t *testing.T) {
	reg := middleware.NewRegistry()
	RegisterDefaults(reg)

	spec := middleware.BuildSpec{
		Endpoint: middleware.EndpointInfo{App: "test", Listener: "test"},
		HasAuth:  false,
		Deps:     stubDeps(),
	}

	got, err := reg.Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.L7) != 9 {
		t.Fatalf("L7 chain length without HasAuth: got %d, want 9", len(got.L7))
	}
}

// TestRegisterDefaults_missingDepFailsClosed verifies that a factory whose
// required dep isn't bound returns an error from Build (which the proxy
// treats as a config_error per plan D7 §S5).
func TestRegisterDefaults_missingDepFailsClosed(t *testing.T) {
	reg := middleware.NewRegistry()
	RegisterDefaults(reg)

	deps := stubDeps()
	delete(deps, DepACMEHandler) // simulate misconfiguration

	spec := middleware.BuildSpec{
		Endpoint: middleware.EndpointInfo{App: "test", Listener: "test"},
		Deps:     deps,
	}

	if _, err := reg.Build(spec); err == nil {
		t.Fatal("Build with missing dep: expected error, got nil")
	}
}
