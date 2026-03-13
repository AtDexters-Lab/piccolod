package identity

import (
	"context"
	"fmt"
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
// The fqdn parameter is not forwarded — namekclient.CreateACMEChallenge infers the
// target hostname from the device's enrolled identity, creating the TXT record at
// _acme-challenge.<canonical-hostname>. This works for both portal and wildcard certs
// since both use the same canonical hostname for ACME validation.
// Multiple calls for the same fqdn (e.g., wildcard + base domain) append challenge IDs
// so all can be cleaned up.
func (c *NamekACMEClient) SetTXTRecord(ctx context.Context, fqdn, value string) error {
	nc := c.clientFn()
	if nc == nil {
		return fmt.Errorf("namek client not enrolled")
	}
	result, err := nc.CreateACMEChallenge(ctx, value)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.challenges[fqdn] = append(c.challenges[fqdn], result.ID)
	c.mu.Unlock()
	return nil
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
