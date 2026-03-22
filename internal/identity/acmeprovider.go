package identity

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

// NamekACMEClient implements acme.OrchestratorClient using namekclient ACME endpoints.
type NamekACMEClient struct {
	clientFn   func() *namekclient.Client
	mu         sync.Mutex
	challenges map[string][]string // fqdn → challenge IDs (multiple for wildcard + base)
}

// NewNamekACMEClient creates an ACME orchestrator client backed by namekclient.
func NewNamekACMEClient(clientFn func() *namekclient.Client) *NamekACMEClient {
	return &NamekACMEClient{
		clientFn:   clientFn,
		challenges: make(map[string][]string),
	}
}

// SetTXTRecord creates a DNS-01 ACME challenge via namekclient.
// The fqdn from lego (e.g. "_acme-challenge.mydevice.example.com.") is stripped to
// extract the target hostname and forwarded to the server, so the TXT record is created
// at the correct FQDN for both canonical and custom hostnames.
// Multiple calls for the same fqdn (e.g., wildcard + base domain) append challenge IDs
// so all can be cleaned up.
func (c *NamekACMEClient) SetTXTRecord(ctx context.Context, fqdn, value string) error {
	nc := c.clientFn()
	if nc == nil {
		return fmt.Errorf("namek client not enrolled")
	}
	hostname := extractHostname(fqdn)
	result, err := nc.CreateACMEChallenge(ctx, value, hostname)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.challenges[fqdn] = append(c.challenges[fqdn], result.ID)
	c.mu.Unlock()
	return nil
}

// extractHostname strips the "_acme-challenge." prefix and trailing dot from a
// lego-provided FQDN, returning the bare hostname for the server. Returns empty
// string if the prefix is missing (server will default to canonical hostname).
func extractHostname(fqdn string) string {
	h, found := strings.CutPrefix(fqdn, "_acme-challenge.")
	if !found {
		return ""
	}
	return strings.TrimSuffix(h, ".")
}

func (c *NamekACMEClient) DeleteTXTRecord(ctx context.Context, fqdn string) error {
	nc := c.clientFn()
	if nc == nil {
		return nil // best-effort
	}
	c.mu.Lock()
	ids := c.challenges[fqdn]
	delete(c.challenges, fqdn)
	c.mu.Unlock()

	var firstErr error
	for _, id := range ids {
		if err := nc.DeleteACMEChallenge(ctx, id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
