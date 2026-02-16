package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"piccolod/internal/state/paths"
	"piccolod/internal/update"
)

// blockingUpdateManager simulates the real update manager's behavior
// where Watch() blocks indefinitely.
type blockingUpdateManager struct{}

func (m *blockingUpdateManager) Status(ctx context.Context) (update.Status, error) {
	return update.Status{}, nil
}
func (m *blockingUpdateManager) Apply(ctx context.Context) error                     { return nil }
func (m *blockingUpdateManager) Rollback(ctx context.Context, targetID string) error { return nil }
func (m *blockingUpdateManager) Reboot(ctx context.Context) error      { return nil }
func (m *blockingUpdateManager) ForceReboot(ctx context.Context) error { return nil }
func (m *blockingUpdateManager) PowerOff(ctx context.Context) error    { return nil }

// Watch blocks until context is cancelled, simulating the real infinite loop.
func (m *blockingUpdateManager) Watch(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestServerStartup_WithBlockingWatchdog(t *testing.T) {
	// Setup temp state dir
	tmpDir := t.TempDir()
	paths.SetCoreRootForTest(t, tmpDir)

	// Set a random port to avoid conflicts
	t.Setenv("PORT", "0")

	// Initialize server with the blocking update manager
	srv, err := NewGinServer(
		WithUpdateManager(&blockingUpdateManager{}),
		WithGinVersion("test-dev"),
	)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	// Capture the dynamically allocated port from the listener?
	// Gin's Run() doesn't easily expose the listener address until it runs.
	// But we can check s.securePort or try to force a port.
	// Let's force a port for testing to simplify connectivity check.
	testPort := "18085"
	t.Setenv("PORT", testPort)

	// Start server in background
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Start(); err != nil {
			errCh <- err
		}
	}()

	// Try to reach the health endpoint.
	// If the watchdog blocks the supervisor, Start() will never reach router.Run(),
	// and this loop will timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	url := fmt.Sprintf("http://localhost:%s/api/v1/health/live", testPort)
	client := http.Client{Timeout: 500 * time.Millisecond}

	fmt.Printf("Attempting to connect to %s...\n", url)

	for {
		select {
		case <-ctx.Done():
			t.Fatalf("Timeout waiting for server to start. The 'Watch' method likely blocked the supervisor startup sequence.")
		case err := <-errCh:
			t.Fatalf("Server failed to start: %v", err)
		case <-ticker.C:
			resp, err := client.Get(url)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					// Success! The server is serving traffic.
					t.Logf("Server successfully started and serving traffic.")
					_ = srv.Stop(context.Background())
					return
				}
			}
		}
	}
}
