package middleware

import (
	"context"
	"testing"
)

func TestHintFromContext_missing(t *testing.T) {
	if h, ok := HintFromContext(context.Background()); ok {
		t.Fatalf("missing hint should return false; got hint=%+v ok=%v", h, ok)
	}
}

func TestHintFromContext_nilContext(t *testing.T) {
	if h, ok := HintFromContext(nil); ok {
		t.Fatalf("nil context should return false; got hint=%+v ok=%v", h, ok)
	}
}

func TestHintFromContext_roundTrip(t *testing.T) {
	in := Hint{ClientIP: "10.0.0.5", IsTLS: true, RemotePort: 8443}
	ctx := ContextWithHint(context.Background(), in)
	got, ok := HintFromContext(ctx)
	if !ok {
		t.Fatal("expected ok=true after ContextWithHint")
	}
	if got != in {
		t.Fatalf("round-trip mismatch: in=%+v got=%+v", in, got)
	}
}

func TestHintFromContext_overwrite(t *testing.T) {
	// L7 overwrites L4 per D13 — the LAN-host-based hop's header-token IS source of truth
	// for that case. Verify ContextWithHint replaces an earlier value rather than wrapping.
	first := Hint{ClientIP: "10.0.0.1"}
	second := Hint{ClientIP: "10.0.0.2"}
	ctx := ContextWithHint(context.Background(), first)
	ctx = ContextWithHint(ctx, second)
	got, _ := HintFromContext(ctx)
	if got.ClientIP != "10.0.0.2" {
		t.Fatalf("overwrite expected 10.0.0.2; got %q", got.ClientIP)
	}
}

func TestHintFromContext_wrongType(t *testing.T) {
	// Defense-in-depth: if some external code somehow wrote a non-Hint value at
	// our key (shouldn't be possible — key is unexported), HintFromContext must
	// return ok=false rather than panic.
	ctx := context.WithValue(context.Background(), hintContextKey{}, "not a hint")
	if h, ok := HintFromContext(ctx); ok {
		t.Fatalf("wrong-type value should return false; got hint=%+v ok=%v", h, ok)
	}
}
