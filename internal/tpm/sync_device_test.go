package tpm

import (
	"sync"
	"testing"
	"time"
)

// mockDevice records calls and supports configurable blocking for testing.
type mockDevice struct {
	mu           sync.Mutex
	quoteCalls   int
	closeCalls   int
	blockQuote   chan struct{} // if non-nil, Quote blocks until closed
	quoteEntered chan struct{} // if non-nil, closed when Quote begins executing
	quoteResult  string
	quoteErr     error
}

func (m *mockDevice) EKCertDER() ([]byte, error) { return []byte("ek"), nil }
func (m *mockDevice) AKPublic() ([]byte, error)   { return []byte("ak"), nil }
func (m *mockDevice) ActivateCredential(enc []byte) ([]byte, error) {
	return []byte("secret"), nil
}

func (m *mockDevice) Quote(nonce string) (string, error) {
	if m.quoteEntered != nil {
		close(m.quoteEntered)
	}
	if m.blockQuote != nil {
		<-m.blockQuote
	}
	m.mu.Lock()
	m.quoteCalls++
	m.mu.Unlock()
	return m.quoteResult, m.quoteErr
}

func (m *mockDevice) QuoteOverData(data []byte) (string, error) {
	m.mu.Lock()
	m.quoteCalls++
	m.mu.Unlock()
	return m.quoteResult, m.quoteErr
}

func (m *mockDevice) Close() error {
	m.mu.Lock()
	m.closeCalls++
	m.mu.Unlock()
	return nil
}

func (m *mockDevice) getQuoteCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.quoteCalls
}

func (m *mockDevice) getCloseCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeCalls
}

func TestSyncDevice_SerializesConcurrentCalls(t *testing.T) {
	inner := &mockDevice{quoteResult: "q"}
	dev := newSyncDevice(inner)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			result, err := dev.Quote("nonce")
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if result != "q" {
				t.Errorf("unexpected result: %s", result)
			}
		}()
	}
	wg.Wait()

	if got := inner.getQuoteCalls(); got != n {
		t.Errorf("expected %d quote calls, got %d", n, got)
	}
}

func TestSyncDevice_UseAfterClose(t *testing.T) {
	inner := &mockDevice{quoteResult: "q"}
	dev := newSyncDevice(inner)

	if err := dev.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	if _, err := dev.EKCertDER(); err != errDeviceClosed {
		t.Errorf("EKCertDER after close: got %v, want errDeviceClosed", err)
	}
	if _, err := dev.AKPublic(); err != errDeviceClosed {
		t.Errorf("AKPublic after close: got %v, want errDeviceClosed", err)
	}
	if _, err := dev.ActivateCredential(nil); err != errDeviceClosed {
		t.Errorf("ActivateCredential after close: got %v, want errDeviceClosed", err)
	}
	if _, err := dev.Quote("n"); err != errDeviceClosed {
		t.Errorf("Quote after close: got %v, want errDeviceClosed", err)
	}
	if _, err := dev.QuoteOverData([]byte("data")); err != errDeviceClosed {
		t.Errorf("QuoteOverData after close: got %v, want errDeviceClosed", err)
	}
}

func TestSyncDevice_DoubleClose(t *testing.T) {
	inner := &mockDevice{}
	dev := newSyncDevice(inner)

	if err := dev.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := dev.Close(); err != errDeviceClosed {
		t.Errorf("second Close: got %v, want errDeviceClosed", err)
	}
	if got := inner.getCloseCalls(); got != 1 {
		t.Errorf("inner Close called %d times, want 1", got)
	}
}

func TestSyncDevice_CloseWaitsForInFlight(t *testing.T) {
	blockCh := make(chan struct{})
	quoteEntered := make(chan struct{})
	inner := &mockDevice{blockQuote: blockCh, quoteResult: "q", quoteEntered: quoteEntered}
	dev := newSyncDevice(inner)

	// Start a blocking Quote — wait for it to acquire the syncDevice mutex
	// and enter the inner Quote (which signals quoteEntered then blocks).
	quoteDone := make(chan struct{})
	go func() {
		dev.Quote("n")
		close(quoteDone)
	}()
	<-quoteEntered // deterministic: inner.Quote is executing, holding syncDevice.mu

	// Close should block on the mutex until Quote finishes.
	closeDone := make(chan struct{})
	go func() {
		dev.Close()
		close(closeDone)
	}()

	// Verify Close is still blocked.
	select {
	case <-closeDone:
		t.Fatal("Close returned before Quote finished")
	case <-time.After(50 * time.Millisecond):
	}

	// Unblock Quote.
	close(blockCh)
	<-quoteDone

	// Close should now complete.
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not return after Quote finished")
	}
}
