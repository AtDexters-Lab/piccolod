package autounlock

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"time"

	"piccolod/internal/cryptoutil"
)

// RecoveryFactorProvider is the provider-neutral boundary used by restart
// unlock continuity. Providers receive only a random per-handoff factor. They
// do not own SDEK wrapping, local persistence, expiry enforcement, cleanup, or
// the post-unlock chain.
//
// V1 intentionally has no revoke operation: Namek exposes one unkeyed slot, so
// a delayed revoke could erase a newer handoff. Unused factors expire by TTL.
type RecoveryFactorProvider interface {
	Deposit(ctx context.Context, randomFactor []byte, ttl time.Duration) (effectiveExpiry time.Time, err error)
	Pickup(ctx context.Context) (randomFactor []byte, err error)
}

const namekV1ProviderID = "namek-v1"

// namekV1Provider adapts the released Namek singleton escrow API to the small
// provider contract. The adapter deliberately does not expose legacy revoke.
type namekV1Provider struct {
	client NamekEscrowClient
}

func (p namekV1Provider) Deposit(ctx context.Context, randomFactor []byte, ttl time.Duration) (time.Time, error) {
	if ttl <= 0 {
		return time.Time{}, errors.New("autounlock: provider deposit TTL must be positive")
	}
	seconds := int(math.Ceil(ttl.Seconds()))
	resp, err := p.client.DepositUnlockEscrow(ctx, randomFactor, seconds)
	if err != nil {
		return time.Time{}, err
	}
	if resp == nil {
		return time.Time{}, errors.New("autounlock: namek deposit returned no response")
	}
	expiry, err := time.Parse(time.RFC3339Nano, resp.ExpiresAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("autounlock: invalid namek expiry: %w", err)
	}
	return expiry, nil
}

func (p namekV1Provider) Pickup(ctx context.Context) ([]byte, error) {
	resp, err := p.client.PickupUnlockEscrow(ctx)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("autounlock: namek pickup returned no response")
	}
	factor, err := base64.RawURLEncoding.DecodeString(resp.Secret)
	if err != nil {
		cryptoutil.SecureZero(factor)
		return nil, fmt.Errorf("%w: namek factor encoding: %v", ErrRecoveryFactorInvalid, err)
	}
	if len(factor) != fSize {
		gotLen := len(factor)
		cryptoutil.SecureZero(factor)
		return nil, fmt.Errorf("%w: namek factor length %d", ErrRecoveryFactorInvalid, gotLen)
	}
	return factor, nil
}

type providerBinding struct {
	id       string
	provider RecoveryFactorProvider
}

func (o *Orchestrator) configuredProvider() (providerBinding, bool) {
	if o.deps.RecoveryProvider != nil {
		provider := o.deps.RecoveryProvider()
		if provider == nil || o.deps.RecoveryProviderID == "" {
			return providerBinding{}, false
		}
		return providerBinding{id: o.deps.RecoveryProviderID, provider: provider}, true
	}
	return o.namekProvider()
}

func (o *Orchestrator) namekProvider() (providerBinding, bool) {
	if o.deps.NamekClient == nil {
		return providerBinding{}, false
	}
	client := o.deps.NamekClient()
	if client == nil {
		return providerBinding{}, false
	}
	return providerBinding{id: namekV1ProviderID, provider: namekV1Provider{client: client}}, true
}

// providerForID is intentionally a one-provider dispatch table in v1. Future
// providers must add an explicit migration/dispatch contract; unknown matching
// metadata is never interpreted as legacy Namek.
func (o *Orchestrator) providerForID(id string) (RecoveryFactorProvider, bool) {
	if id == namekV1ProviderID {
		binding, ok := o.namekProvider()
		if !ok {
			return nil, false
		}
		return binding.provider, true
	}
	if o.deps.RecoveryProvider != nil && id == o.deps.RecoveryProviderID {
		provider := o.deps.RecoveryProvider()
		return provider, provider != nil
	}
	return nil, false
}

func (o *Orchestrator) isRecognizedProviderID(id string) bool {
	if id == namekV1ProviderID {
		return true
	}
	return o.deps.RecoveryProvider != nil && id != "" && id == o.deps.RecoveryProviderID
}

func (o *Orchestrator) providerReady() bool {
	if o.deps.IsRecoveryProviderReady != nil {
		return o.deps.IsRecoveryProviderReady()
	}
	return o.deps.IsIdentityReady != nil && o.deps.IsIdentityReady()
}

func (o *Orchestrator) waitForProvider(ctx context.Context, timeout time.Duration) bool {
	if o.providerReady() {
		return true
	}
	if o.deps.WaitForRecoveryProviderReady != nil {
		return o.deps.WaitForRecoveryProviderReady(ctx, timeout)
	}
	return o.waitForIdentity(ctx, timeout)
}
