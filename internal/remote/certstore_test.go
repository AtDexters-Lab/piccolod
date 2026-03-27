package remote

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"piccolod/internal/services"
)

func TestFileCertProviderMissingHandlerCalled(t *testing.T) {
	dir := t.TempDir()
	p := NewFileCertProvider(dir)

	ch := make(chan string, 1)
	p.SetMissingHandler(func(host string) {
		select {
		case ch <- host:
		default:
		}
	})

	_, err := p.GetCertificate("foo.example.com")
	if !errors.Is(err, services.ErrNoCert) {
		t.Fatalf("expected ErrNoCert, got %v", err)
	}

	select {
	case h := <-ch:
		if h != "foo.example.com" {
			t.Fatalf("expected handler host foo.example.com, got %s", h)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("expected missing handler to be called")
	}
}

func TestMissingHandlerDebounce(t *testing.T) {
	dir := t.TempDir()
	p := NewFileCertProvider(dir)

	var count atomic.Int32
	p.SetMissingHandler(func(host string) {
		count.Add(1)
	})

	// Call GetCertificate 20 times rapidly for the same host.
	for i := 0; i < 20; i++ {
		_, _ = p.GetCertificate("debounce.example.com")
	}

	// Give goroutines time to run.
	time.Sleep(100 * time.Millisecond)

	got := count.Load()
	if got != 1 {
		t.Fatalf("expected missing handler to fire exactly 1 time (debounced), got %d", got)
	}

	// Different host should fire independently.
	_, _ = p.GetCertificate("other.example.com")
	time.Sleep(50 * time.Millisecond)

	got = count.Load()
	if got != 2 {
		t.Fatalf("expected 2 total fires (one per host), got %d", got)
	}
}
