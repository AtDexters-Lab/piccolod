package remote

import (
	"errors"
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
