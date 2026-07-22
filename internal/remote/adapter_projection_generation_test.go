package remote

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"piccolod/internal/remote/nexusclient"
)

type projectionTestAdapter struct {
	mu             sync.Mutex
	configs        []nexusclient.Config
	blockConfigure chan struct{}
	configureIn    chan struct{}
	configureOnce  sync.Once
	blockStop      atomic.Bool
	stopIn         chan struct{}
	stopOnce       sync.Once
	stopCalls      atomic.Int32
}

func (a *projectionTestAdapter) Configure(cfg nexusclient.Config) error {
	copied := cfg
	copied.Aliases = append([]nexusclient.AliasEntry(nil), cfg.Aliases...)
	a.mu.Lock()
	a.configs = append(a.configs, copied)
	a.mu.Unlock()
	if a.configureIn != nil {
		a.configureOnce.Do(func() {
			close(a.configureIn)
			<-a.blockConfigure
		})
	}
	return nil
}

func (*projectionTestAdapter) Start(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (a *projectionTestAdapter) Stop(ctx context.Context) error {
	a.stopCalls.Add(1)
	if a.stopIn != nil {
		a.stopOnce.Do(func() { close(a.stopIn) })
	}
	if a.blockStop.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (a *projectionTestAdapter) snapshots() []nexusclient.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]nexusclient.Config, len(a.configs))
	copy(out, a.configs)
	return out
}

func newProjectionTestManager(t *testing.T, adapter nexusclient.Adapter, filter func(nexusclient.AliasEntry) bool) *Manager {
	t.Helper()
	m := newTestManagerWithDeps(t, nil, t.TempDir(), &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(1, 0)))
	m.cfgMu.Lock()
	m.cfg = &Config{NexusConfig: NexusConfig{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "secret",
		PortalHostname: "portal.example.com",
		Enabled:        true,
		Aliases: []Alias{
			{Hostname: "home.example.net", Listener: "portal"},
			{Hostname: "demo.example.net", Listener: "demo"},
		},
	}}
	m.cfgMu.Unlock()
	m.adapterMu.Lock()
	m.adapter = adapter
	m.aliasPublicationFilter = filter
	m.adapterMu.Unlock()
	return m
}

func TestAdapterProjectionDiscardsStaleSnapshotBeforeConfigure(t *testing.T) {
	adapter := &projectionTestAdapter{}
	filterEntered := make(chan struct{})
	releaseFilter := make(chan struct{})
	var active atomic.Bool
	active.Store(true)
	var first atomic.Bool
	first.Store(true)
	m := newProjectionTestManager(t, adapter, func(alias nexusclient.AliasEntry) bool {
		if alias.HostLabel == nexusclient.PortalHostLabel {
			return true
		}
		if first.CompareAndSwap(true, false) {
			close(filterEntered)
			<-releaseFilter
			return true
		}
		return active.Load()
	})

	firstApplyDone := make(chan struct{})
	go func() {
		m.RefreshPortClaims()
		close(firstApplyDone)
	}()
	<-filterEntered
	active.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := m.RefreshPortClaimsContext(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("overlapping refresh error = %v, want deadline exceeded", err)
	}
	close(releaseFilter)
	select {
	case <-firstApplyDone:
	case <-time.After(time.Second):
		t.Fatal("projection owner did not repair newest generation")
	}

	configs := adapter.snapshots()
	if len(configs) != 1 {
		t.Fatalf("Configure calls = %d, want only newest projection", len(configs))
	}
	assertProjectionAliases(t, configs[0], "home.example.net")
}

func TestAdapterProjectionRepairsGenerationChangedDuringConfigure(t *testing.T) {
	adapter := &projectionTestAdapter{
		blockConfigure: make(chan struct{}),
		configureIn:    make(chan struct{}),
	}
	var active atomic.Bool
	active.Store(true)
	m := newProjectionTestManager(t, adapter, func(alias nexusclient.AliasEntry) bool {
		return alias.HostLabel == nexusclient.PortalHostLabel || active.Load()
	})

	firstApplyDone := make(chan struct{})
	go func() {
		m.RefreshPortClaims()
		close(firstApplyDone)
	}()
	<-adapter.configureIn
	active.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := m.RefreshPortClaimsContext(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded withdrawal error = %v, want deadline exceeded", err)
	}
	close(adapter.blockConfigure)
	select {
	case <-firstApplyDone:
	case <-time.After(time.Second):
		t.Fatal("blocking Configure did not converge to newest generation")
	}

	configs := adapter.snapshots()
	if len(configs) != 2 {
		t.Fatalf("Configure calls = %d, want stale call followed by repair", len(configs))
	}
	assertProjectionAliases(t, configs[0], "home.example.net", "demo.example.net")
	assertProjectionAliases(t, configs[1], "home.example.net")
	m.adapterMu.Lock()
	lastKey := m.lastAdapterKey
	applied := m.adapterAppliedGeneration
	current := m.adapterProjectionGeneration
	m.adapterMu.Unlock()
	if lastKey != adapterConfigKey(adapterStateSnapshot{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "secret",
		PortalHostname: "portal.example.com",
		Enabled:        true,
		Aliases:        configs[1].Aliases,
	}) {
		t.Fatal("stale Configure updated the adapter fingerprint")
	}
	if applied != current {
		t.Fatalf("applied generation = %d, current = %d", applied, current)
	}
}

func TestAdapterProjectionStopDeadlineAllowsNewestGeneration(t *testing.T) {
	adapter := &projectionTestAdapter{
		stopIn: make(chan struct{}),
	}
	adapter.blockStop.Store(true)
	var active atomic.Bool
	active.Store(true)
	m := newProjectionTestManager(t, adapter, func(alias nexusclient.AliasEntry) bool {
		return alias.HostLabel == nexusclient.PortalHostLabel || active.Load()
	})
	// The production projection path is intentionally bounded; the manager's
	// ordinary test cleanup uses an unbounded Close context, so restore the fake
	// to an acknowledging adapter before that cleanup runs.
	t.Cleanup(func() { adapter.blockStop.Store(false) })
	m.adapterStopTimeout = 25 * time.Millisecond

	// Establish one running projection so the withdrawal path must stop the
	// current adapter before committing a replacement registration.
	m.RefreshPortClaims()
	active.Store(false)
	firstApplyDone := make(chan struct{})
	go func() {
		m.RefreshPortClaims()
		close(firstApplyDone)
	}()
	select {
	case <-adapter.stopIn:
	case <-time.After(time.Second):
		t.Fatal("projection did not enter adapter stop")
	}

	// Advance the projection while Stop is still waiting. This caller is
	// bounded independently, while the coalescing owner must repair the newest
	// generation once the internal stop deadline fires.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	err := m.RefreshPortClaimsContext(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("overlapping refresh error = %v, want deadline exceeded", err)
	}
	select {
	case <-firstApplyDone:
	case <-time.After(time.Second):
		t.Fatal("bounded adapter stop stranded the projection owner")
	}

	configs := adapter.snapshots()
	if len(configs) < 2 {
		t.Fatalf("Configure calls = %d, want initial plus repaired projection", len(configs))
	}
	assertProjectionAliases(t, configs[len(configs)-1], "home.example.net")
	m.adapterMu.Lock()
	applied := m.adapterAppliedGeneration
	current := m.adapterProjectionGeneration
	m.adapterMu.Unlock()
	if applied != current {
		t.Fatalf("applied generation = %d, current = %d", applied, current)
	}
	if adapter.stopCalls.Load() != 1 {
		t.Fatalf("Stop calls = %d, want one bounded stop", adapter.stopCalls.Load())
	}
}

func assertProjectionAliases(t *testing.T, cfg nexusclient.Config, want ...string) {
	t.Helper()
	if len(cfg.Aliases) != len(want) {
		t.Fatalf("aliases = %+v, want %v", cfg.Aliases, want)
	}
	for i, hostname := range want {
		if cfg.Aliases[i].Hostname != hostname {
			t.Fatalf("aliases = %+v, want %v", cfg.Aliases, want)
		}
	}
}
