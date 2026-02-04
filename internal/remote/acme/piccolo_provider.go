package acme

import (
	"context"
	"fmt"
	"time"

	"github.com/go-acme/lego/v4/challenge/dns01"
)

// OrchestratorClient defines the interface for Piccolo orchestrator DNS API
type OrchestratorClient interface {
	// SetTXTRecord creates an ACME challenge TXT record
	SetTXTRecord(ctx context.Context, fqdn, value string) error
	// DeleteTXTRecord removes an ACME challenge TXT record
	DeleteTXTRecord(ctx context.Context, fqdn string) error
}

// piccoloProvider implements lego's challenge.Provider for Piccolo orchestrator
type piccoloProvider struct {
	client OrchestratorClient
}

// NewPiccoloProvider creates a DNS-01 provider using Piccolo orchestrator
func NewPiccoloProvider(client OrchestratorClient) *piccoloProvider {
	return &piccoloProvider{client: client}
}

func (p *piccoloProvider) Present(domain, token, keyAuth string) error {
	if p.client == nil {
		return fmt.Errorf("piccolo orchestrator client not configured")
	}
	// Compute the correct DNS-01 TXT record value (base64url-encoded SHA256 of keyAuth)
	fqdn, txtValue := dns01.GetRecord(domain, keyAuth)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return p.client.SetTXTRecord(ctx, fqdn, txtValue)
}

func (p *piccoloProvider) CleanUp(domain, token, keyAuth string) error {
	if p.client == nil {
		return nil // Best effort cleanup
	}
	// Use dns01.GetRecord to get the proper FQDN format
	fqdn, _ := dns01.GetRecord(domain, keyAuth)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return p.client.DeleteTXTRecord(ctx, fqdn)
}
