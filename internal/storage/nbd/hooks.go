package nbd

import "context"

// NoopColdRecallHook is the default ColdRecallHook that passes through all reads.
type NoopColdRecallHook struct{}

func (NoopColdRecallHook) OnRead(_ context.Context, _, _ uint64) (bool, error) {
	return true, nil // always proceed with local read
}

// NoopTierEvictionHook is the default TierEvictionHook that never evicts.
type NoopTierEvictionHook struct{}

func (NoopTierEvictionHook) ShouldEvict(_ context.Context) ([]EvictRange, error) {
	return nil, nil // no eviction
}

// NoopDirtyBitmapHook is the default DirtyBitmapHook that performs no tracking.
type NoopDirtyBitmapHook struct{}

func (NoopDirtyBitmapHook) MarkDirty(_, _ uint64)  {}
func (NoopDirtyBitmapHook) DirtyRanges() []DirtyRange { return nil }
func (NoopDirtyBitmapHook) Clear()                    {}

// NoopCoalescingHook is the default CoalescingHook that performs no aggregation.
type NoopCoalescingHook struct{}

func (NoopCoalescingHook) OnWrite(_ context.Context, _, _ uint64) error {
	return nil
}

// Hooks bundles all PSFN hook implementations for an NBD session.
// All fields default to no-op implementations.
type Hooks struct {
	ColdRecall  ColdRecallHook
	Eviction    TierEvictionHook
	DirtyBitmap DirtyBitmapHook
	Coalescing  CoalescingHook
}

// DefaultHooks returns a Hooks bundle with all no-op implementations.
func DefaultHooks() Hooks {
	return Hooks{
		ColdRecall:  NoopColdRecallHook{},
		Eviction:    NoopTierEvictionHook{},
		DirtyBitmap: NoopDirtyBitmapHook{},
		Coalescing:  NoopCoalescingHook{},
	}
}
