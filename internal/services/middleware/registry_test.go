package middleware

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
)

// noopConnHandler is a sink for chain-invocation tests — accepts the ConnHandler
// signature without doing anything with the conn.
func noopConnHandler(_ ConnContext, _ net.Conn) {}

func TestRegistry_Build_empty(t *testing.T) {
	r := NewRegistry()
	out, err := r.Build(BuildSpec{})
	if err != nil {
		t.Fatalf("empty Build: unexpected error: %v", err)
	}
	if out.L4 != nil || out.L4UDP != nil || out.L7 != nil || out.L7Response != nil {
		t.Fatalf("empty Build: expected all-nil chains, got %+v", out)
	}
}

func TestRegistry_Build_canonicalOrder(t *testing.T) {
	r := NewRegistry()
	var calls []string

	mkL4 := func(name string) Factory {
		return func(_ map[string]any, _ EndpointInfo, _ RegistryDeps) (any, error) {
			return L4Middleware(func(next ConnHandler) ConnHandler {
				calls = append(calls, name)
				return next
			}), nil
		}
	}
	r.RegisterCanonical("first", LayerL4, mkL4("first"))
	r.RegisterCanonical("second", LayerL4, mkL4("second"))
	r.RegisterCanonical("third", LayerL4, mkL4("third"))

	out, err := r.Build(BuildSpec{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(out.L4) != 3 {
		t.Fatalf("expected 3 L4 entries, got %d", len(out.L4))
	}
	// Invoke each to capture the registration order.
	for _, mw := range out.L4 {
		mw(noopConnHandler)
	}
	got := strings.Join(calls, ",")
	if got != "first,second,third" {
		t.Fatalf("expected canonical order first,second,third; got %s", got)
	}
}

func TestRegistry_Build_operatorAppendedAfterCanonical(t *testing.T) {
	r := NewRegistry()
	var calls []string

	mkL4 := func(name string) Factory {
		return func(_ map[string]any, _ EndpointInfo, _ RegistryDeps) (any, error) {
			return L4Middleware(func(next ConnHandler) ConnHandler {
				calls = append(calls, name)
				return next
			}), nil
		}
	}
	r.RegisterCanonical("canon_a", LayerL4, mkL4("canon_a"))
	r.RegisterCanonical("canon_b", LayerL4, mkL4("canon_b"))
	r.Register("op_x", []Layer{LayerL4}, mkL4("op_x"))
	r.Register("op_y", []Layer{LayerL4}, mkL4("op_y"))

	spec := BuildSpec{
		OperatorEntries: []OperatorEntry{
			{Name: "op_x"},
			{Name: "op_y"},
		},
	}
	out, err := r.Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(out.L4) != 4 {
		t.Fatalf("expected 4 L4 entries, got %d", len(out.L4))
	}
	for _, mw := range out.L4 {
		mw(noopConnHandler)
	}
	got := strings.Join(calls, ",")
	if got != "canon_a,canon_b,op_x,op_y" {
		t.Fatalf("expected canon then op order; got %s", got)
	}
}

func TestRegistry_Build_unknownOperatorEntryFails(t *testing.T) {
	r := NewRegistry()
	spec := BuildSpec{
		OperatorEntries: []OperatorEntry{{Name: "does_not_exist"}},
	}
	_, err := r.Build(spec)
	if err == nil {
		t.Fatal("expected error for unknown operator entry")
	}
	if !strings.Contains(err.Error(), "does_not_exist") {
		t.Fatalf("error should name missing entry; got %v", err)
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("error should say 'not registered'; got %v", err)
	}
}

func TestRegistry_Build_factoryError(t *testing.T) {
	r := NewRegistry()
	r.Register("boom", []Layer{LayerL4}, func(_ map[string]any, _ EndpointInfo, _ RegistryDeps) (any, error) {
		return nil, errors.New("synthetic failure")
	})
	_, err := r.Build(BuildSpec{
		OperatorEntries: []OperatorEntry{{Name: "boom"}},
	})
	if err == nil {
		t.Fatal("expected error from factory")
	}
	if !strings.Contains(err.Error(), "synthetic failure") {
		t.Fatalf("error should wrap factory error; got %v", err)
	}
}

func TestRegistry_Build_typeAssertionMismatch(t *testing.T) {
	r := NewRegistry()
	// Factory claims L4 but returns an L7Middleware — Build should reject.
	r.Register("liar", []Layer{LayerL4}, func(_ map[string]any, _ EndpointInfo, _ RegistryDeps) (any, error) {
		return L7Middleware(func(next http.Handler) http.Handler { return next }), nil
	})
	_, err := r.Build(BuildSpec{
		OperatorEntries: []OperatorEntry{{Name: "liar"}},
	})
	if err == nil {
		t.Fatal("expected type-assertion error")
	}
	if !strings.Contains(err.Error(), "expected L4") {
		t.Fatalf("error should name expected layer; got %v", err)
	}
}

func TestRegistry_Build_conditionalCanonicalGate(t *testing.T) {
	r := NewRegistry()
	calls := 0
	r.RegisterCanonical("connection_auth", LayerL4, func(_ map[string]any, _ EndpointInfo, _ RegistryDeps) (any, error) {
		calls++
		return L4Middleware(func(next ConnHandler) ConnHandler { return next }), nil
	})

	// Without HasConnectionAuth, factory should NOT be invoked.
	out, err := r.Build(BuildSpec{HasConnectionAuth: false})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(out.L4) != 0 {
		t.Fatalf("expected connection_auth skipped; got %d entries", len(out.L4))
	}
	if calls != 0 {
		t.Fatalf("factory invoked %d times; expected 0", calls)
	}

	// With HasConnectionAuth, factory IS invoked.
	out, err = r.Build(BuildSpec{HasConnectionAuth: true})
	if err != nil {
		t.Fatalf("Build with gate: %v", err)
	}
	if len(out.L4) != 1 {
		t.Fatalf("expected 1 entry; got %d", len(out.L4))
	}
	if calls != 1 {
		t.Fatalf("factory invoked %d times; expected 1", calls)
	}
}

func TestRegistry_Build_operatorEntrySkippedForWrongLayer(t *testing.T) {
	r := NewRegistry()
	// Middleware registered for L4 only; listener composes L7 chain — should silently skip.
	r.Register("l4only", []Layer{LayerL4}, func(_ map[string]any, _ EndpointInfo, _ RegistryDeps) (any, error) {
		return L4Middleware(func(next ConnHandler) ConnHandler { return next }), nil
	})
	out, err := r.Build(BuildSpec{
		OperatorEntries: []OperatorEntry{{Name: "l4only"}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(out.L4) != 1 {
		t.Fatalf("expected 1 L4 entry; got %d", len(out.L4))
	}
	if len(out.L7) != 0 {
		t.Fatalf("expected 0 L7 entries (l4only skipped at L7 layer); got %d", len(out.L7))
	}
}

func TestRegistry_Register_panicsOnInvalid(t *testing.T) {
	cases := []struct {
		name    string
		fn      func()
		wantSub string
	}{
		{"empty name", func() { NewRegistry().Register("", []Layer{LayerL4}, dummyFactory) }, "empty name"},
		{"empty layers", func() { NewRegistry().Register("x", nil, dummyFactory) }, "empty layers"},
		{"nil factory", func() { NewRegistry().Register("x", []Layer{LayerL4}, nil) }, "nil factory"},
		{"duplicate layers", func() {
			NewRegistry().Register("x", []Layer{LayerL4, LayerL4}, dummyFactory)
		}, "duplicate layer"},
		{"re-register same name (Register then Register)", func() {
			r := NewRegistry()
			r.Register("x", []Layer{LayerL4}, dummyFactory)
			r.Register("x", []Layer{LayerL7}, dummyFactory)
		}, "already registered"},
		{"re-register same name (Register then RegisterCanonical)", func() {
			r := NewRegistry()
			r.Register("x", []Layer{LayerL4}, dummyFactory)
			r.RegisterCanonical("x", LayerL4, dummyFactory)
		}, "already registered"},
		{"re-register same name (RegisterCanonical twice)", func() {
			r := NewRegistry()
			r.RegisterCanonical("x", LayerL4, dummyFactory)
			r.RegisterCanonical("x", LayerL4, dummyFactory)
		}, "already registered"},
		{"RegisterCanonical empty name", func() {
			NewRegistry().RegisterCanonical("", LayerL4, dummyFactory)
		}, "empty name"},
		{"RegisterCanonical nil factory", func() {
			NewRegistry().RegisterCanonical("x", LayerL4, nil)
		}, "nil factory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("expected panic")
				}
				msg, ok := r.(string)
				if !ok {
					t.Fatalf("expected string panic; got %T", r)
				}
				if !strings.Contains(msg, tc.wantSub) {
					t.Fatalf("panic message %q should contain %q", msg, tc.wantSub)
				}
			}()
			tc.fn()
		})
	}
}

func TestRegistry_Build_rejectsCanonicalAsOperator(t *testing.T) {
	// Canonical entries are composed automatically (always-on, or via typed fields
	// like ConnectionAuth/Auth). Listing them in Middleware[] is a config error and
	// must be rejected at Build time.
	r := NewRegistry()
	r.RegisterCanonical("connection_auth", LayerL4, dummyFactory)

	_, err := r.Build(BuildSpec{
		HasConnectionAuth: true, // canonical pass would include it
		OperatorEntries:   []OperatorEntry{{Name: "connection_auth"}},
	})
	if err == nil {
		t.Fatal("expected error rejecting canonical name in operator entries")
	}
	if !strings.Contains(err.Error(), "canonical entry not operator-listable") {
		t.Fatalf("error should explain canonical-vs-operator rule; got %v", err)
	}
}

func dummyFactory(_ map[string]any, _ EndpointInfo, _ RegistryDeps) (any, error) {
	return L4Middleware(func(next ConnHandler) ConnHandler { return next }), nil
}

func TestMapDeps(t *testing.T) {
	calls := 0
	deps := MapDeps{
		"counter": func() any {
			calls++
			return calls
		},
	}
	if got := deps.Get("counter"); got != 1 {
		t.Fatalf("first Get: want 1; got %v", got)
	}
	if got := deps.Get("counter"); got != 2 {
		t.Fatalf("second Get should re-invoke getter (hot-swap support); got %v", got)
	}
	if got := deps.Get("missing"); got != nil {
		t.Fatalf("missing key: want nil; got %v", got)
	}
}
