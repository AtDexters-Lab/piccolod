package services

import (
	"net/http"
	"testing"

	"piccolod/internal/api"
	"piccolod/internal/services/middleware/builtin"
	"piccolod/internal/services/middleware/l7"
)

// proxy_equivalence_test.go is the plan §H step-3 behavior-preservation
// harness. The "pre-refactor baseline" is encoded as the assertions in
// proxy_test.go and proxy_auth_test.go — those tests built and exercised
// startHTTPProxy before the registry took over chain composition. After
// step 3, every existing proxy-side test runs through the registry-built
// chain. Their continued passage is the equivalence proof.
//
// This file adds focused assertions that pin two registry invariants the
// prior inline composition couldn't accidentally violate:
//  1. The L7 deps map this package supplies covers every dep key the canonical
//     chain factories read. Drift in either direction (rename a dep key in
//     builtin/, drop a dep here) fails this test before it can degrade
//     production behavior.
//  2. Every dep key returns a value of the documented concrete type — guards
//     against the type-assertion failures that would otherwise surface as
//     opaque "factory: missing dep X" errors at proxy startup.

// TestProxyEquivalence_DepsCoverAllRegisteredKeys verifies that the deps
// map ProxyManager builds for each listener supplies every dep key the
// canonical chain factories read. Built by walking the registered builtin
// constants and asserting each is bound.
func TestProxyEquivalence_DepsCoverAllRegisteredKeys(t *testing.T) {
	pm := NewProxyManager()
	defer pm.StopAll()

	// Minimal endpoint shape — actual values don't matter for dep coverage.
	ep := ServiceEndpoint{
		App:        "app",
		Name:       "lstn",
		HostBind:   8080,
		PublicPort: 8080,
		Flow:       api.FlowTCP,
		Protocol:   api.ListenerProtocolHTTP,
	}
	deps := pm.buildL7Deps(ep)

	required := []string{
		builtin.DepHintConsumerL7,
		builtin.DepReservedPathCallback,
		builtin.DepACMEHandler,
		builtin.DepArrivedViaTLS,
		builtin.DepPathAuthSnapshot,
		builtin.DepCookieContext,
		builtin.DepOIDCAuthorizeSnapshot,
		builtin.DepForwardHeaders,
		builtin.DepNeedsMarker,
		builtin.DepMarkerCookie,
	}

	for _, key := range required {
		if v := deps.Get(key); v == nil {
			t.Errorf("dep %q: not bound by ProxyManager.buildL7Deps", key)
		}
	}
}

// TestProxyEquivalence_DepTypesMatchFactoryExpectations verifies each dep
// resolves to the concrete type its factory type-asserts. A drift between
// what services/ supplies and what builtin/ expects surfaces here, not in a
// production proxy startup that fails after the listener accepts traffic.
func TestProxyEquivalence_DepTypesMatchFactoryExpectations(t *testing.T) {
	pm := NewProxyManager()
	defer pm.StopAll()

	ep := ServiceEndpoint{App: "app", Name: "lstn", Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP}
	deps := pm.buildL7Deps(ep)

	type assertion struct {
		key   string
		check func(any) bool
	}
	checks := []assertion{
		{builtin.DepHintConsumerL7, func(v any) bool { _, ok := v.(func(*http.Request) *http.Request); return ok }},
		{builtin.DepACMEHandler, func(v any) bool { _, ok := v.(l7.ACMEHandlerFn); return ok }},
		{builtin.DepArrivedViaTLS, func(v any) bool { _, ok := v.(l7.ArrivedViaTLSFn); return ok }},
		{builtin.DepPathAuthSnapshot, func(v any) bool { _, ok := v.(func() l7.PathAuthSnapshot); return ok }},
		{builtin.DepCookieContext, func(v any) bool { _, ok := v.(func(*http.Request) *http.Request); return ok }},
		{builtin.DepForwardHeaders, func(v any) bool { _, ok := v.(func(*http.Request)); return ok }},
		{builtin.DepNeedsMarker, func(v any) bool { _, ok := v.(l7.NeedsMarkerFn); return ok }},
		{builtin.DepMarkerCookie, func(v any) bool { _, ok := v.(l7.MarkerCookieFn); return ok }},
	}

	for _, c := range checks {
		v := deps.Get(c.key)
		if v == nil {
			t.Errorf("dep %q: nil", c.key)
			continue
		}
		if !c.check(v) {
			t.Errorf("dep %q: type %T does not match factory expectation", c.key, v)
		}
	}
}
