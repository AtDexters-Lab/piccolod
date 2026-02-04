package orchestrator

import (
	"context"
	"fmt"
)

// Client implements acme.OrchestratorClient for Piccolo orchestrator API
type Client struct {
	endpoint string
	token    string
}

// NewClient creates a new orchestrator client
func NewClient(endpoint, token string) *Client {
	return &Client{endpoint: endpoint, token: token}
}

// SetTXTRecord creates an ACME challenge TXT record via orchestrator API
func (c *Client) SetTXTRecord(ctx context.Context, fqdn, value string) error {
	// TODO: Implement API call to orchestrator
	// POST {endpoint}/dns/txt
	// Body: { "fqdn": fqdn, "value": value }
	// Auth: Bearer {token}
	return fmt.Errorf("orchestrator API not implemented")
}

// DeleteTXTRecord removes an ACME challenge TXT record via orchestrator API
func (c *Client) DeleteTXTRecord(ctx context.Context, fqdn string) error {
	// TODO: Implement API call to orchestrator
	// DELETE {endpoint}/dns/txt/{fqdn}
	// Auth: Bearer {token}
	return fmt.Errorf("orchestrator API not implemented")
}
