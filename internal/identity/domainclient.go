package identity

import (
	"context"
	"fmt"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

// NamekDomainClient wraps namekclient domain management methods.
// Follows the NamekACMEClient pattern: uses clientFn to get the current client at call time.
type NamekDomainClient struct {
	clientFn func() *namekclient.Client
}

// NewNamekDomainClient creates a domain client backed by namekclient.
func NewNamekDomainClient(clientFn func() *namekclient.Client) *NamekDomainClient {
	return &NamekDomainClient{clientFn: clientFn}
}

func (c *NamekDomainClient) client() (*namekclient.Client, error) {
	nc := c.clientFn()
	if nc == nil {
		return nil, fmt.Errorf("namek client not available")
	}
	if nc.DeviceID() == "" {
		return nil, fmt.Errorf("namek enrollment incomplete")
	}
	return nc, nil
}

// Register registers a new alias domain on namek.
func (c *NamekDomainClient) Register(ctx context.Context, domain string) (*namekclient.DomainInfo, error) {
	nc, err := c.client()
	if err != nil {
		return nil, err
	}
	return nc.RegisterDomain(ctx, domain)
}

// Verify triggers CNAME verification for a domain on namek.
func (c *NamekDomainClient) Verify(ctx context.Context, domainID string) (*namekclient.DomainInfo, error) {
	nc, err := c.client()
	if err != nil {
		return nil, err
	}
	return nc.VerifyDomain(ctx, domainID)
}

// Assign assigns the domain to this device (1:1 model — device assigns to itself).
func (c *NamekDomainClient) Assign(ctx context.Context, domainID string) ([]namekclient.DomainAssignment, error) {
	nc, err := c.client()
	if err != nil {
		return nil, err
	}
	return nc.AssignDomain(ctx, domainID, []string{nc.DeviceID()})
}

// Unassign removes this device's assignment from the domain.
func (c *NamekDomainClient) Unassign(ctx context.Context, domainID string) error {
	nc, err := c.client()
	if err != nil {
		return err
	}
	return nc.UnassignDomain(ctx, domainID, nc.DeviceID())
}

// Delete removes a domain registration from namek.
func (c *NamekDomainClient) Delete(ctx context.Context, domainID string) error {
	nc, err := c.client()
	if err != nil {
		return err
	}
	return nc.DeleteDomain(ctx, domainID)
}

// List returns all domains registered by this account on namek.
func (c *NamekDomainClient) List(ctx context.Context) ([]namekclient.DomainInfo, error) {
	nc, err := c.client()
	if err != nil {
		return nil, err
	}
	return nc.ListDomains(ctx)
}
