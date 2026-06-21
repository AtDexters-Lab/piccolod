package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"piccolod/internal/api"
)

// ErrSyncHostUnavailable is returned when the catalog manifest sync host is
// not wired (e.g. tests, or pre-StartBackground initialization).
var ErrSyncHostUnavailable = errors.New("catalog sync host not wired")

// ErrAppNotFound is returned when an app instance is not present in the
// state store. Callers should map this to HTTP 404.
var ErrAppNotFound = errors.New("app instance not found")

// ErrSyncInProgress is returned when a sync attempt is rejected because
// another sync is already running for the same app instance. Callers should
// map this to HTTP 409.
var ErrSyncInProgress = errors.New("sync already in progress for this app")

func errAppNotFound(instanceID string) error {
	return fmt.Errorf("%w: %s", ErrAppNotFound, instanceID)
}

const (
	// catalogSyncInterval is the cadence of the catalog manifest drift check.
	catalogSyncInterval = 15 * time.Minute

	// catalogSyncStartupDelay defers the first sync pass after StartBackground
	// to avoid colliding with the unlock warmup and the reconcile ticker's
	// initial pass.
	catalogSyncStartupDelay = 2 * time.Minute

	// catalogSyncEnvDisable, when set to "1", disables the entire sync subsystem
	// at the global level (kill switch).
	catalogSyncEnvDisable = "PICCOLO_CATALOG_SYNC_DISABLE"
)

// SyncHost is the injection interface that catalog manifest sync uses to reach
// host-level concerns that live on the server (OIDC client manager, proxy
// register/delete, system context builder, catalog template fetch).
//
// AppManager has no direct dependency on GinServer; SyncHost is the seam.
// Tests that don't exercise sync may pass nil and the ticker will skip cleanly.
type SyncHost interface {
	// FetchCatalogTemplate returns the raw catalog manifest bytes for the given
	// catalog item name.
	FetchCatalogTemplate(ctx context.Context, catalogSource string) ([]byte, error)

	// FetchInitScript returns the raw init script bytes for a given catalog
	// item and file path. Used to compare init script hashes.
	FetchInitScript(ctx context.Context, catalogSource, filePath string) ([]byte, error)

	// CurrentInstallSystemContext returns a freshly-snapshotted
	// InstallSystemContext built from live host state. Called at install time
	// and on the admin refresh-context endpoint.
	CurrentInstallSystemContext() InstallSystemContext

	// OIDCClientGenerator returns a generator that produces fresh credentials.
	// Sync only uses the generator on legacy patcher paths that need to seed
	// new credentials; modern paths reuse persisted credentials. Returns nil
	// if the OIDC subsystem is unavailable (e.g. control store still locked).
	OIDCClientGenerator() OIDCClientGenerator

	// PersistOIDCClient registers a freshly-generated OIDC client with the
	// client manager. Called by sync when the catalog adds oidc_client to a
	// service that didn't previously declare one.
	PersistOIDCClient(ctx context.Context, clientID, clientSecret, appID string) error
	DeleteOIDCClient(ctx context.Context, clientID string) error

	// RequiresProxyOIDCClient mirrors the GinServer helper of the same name.
	// Sync uses this to compute proxy-client register/delete deltas.
	RequiresProxyOIDCClient(def *api.AppDefinition) bool

	// RegisterProxyOIDCClient idempotently registers a proxy OIDC client for
	// the named app. Sync calls this when a structural change makes the proxy
	// client newly required.
	RegisterProxyOIDCClient(ctx context.Context, instanceID string) error

	// DeleteProxyOIDCClient removes a proxy OIDC client. Sync calls this when
	// a structural change removes the requirement.
	DeleteProxyOIDCClient(ctx context.Context, instanceID string)
}

// SetSyncHost wires the catalog manifest sync host. Safe to call multiple
// times (last write wins). Must be called before StartBackground for the
// sync ticker to do useful work.
func (m *AppManager) SetSyncHost(h SyncHost) {
	m.stateMu.Lock()
	m.syncHost = h
	m.stateMu.Unlock()
}

// currentSyncHost returns the wired sync host or nil. Read under stateMu.
func (m *AppManager) currentSyncHost() SyncHost {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.syncHost
}

// startCatalogSyncLoop is invoked from StartBackground to start the dedicated
// 15-minute catalog manifest sync ticker. The first pass is deferred by
// catalogSyncStartupDelay so it does not collide with the reconcile ticker's
// initial pass and the post-unlock warmup work.
func (m *AppManager) startCatalogSyncLoop(ctx context.Context) {
	m.reconcileWG.Add(1)
	go func() {
		defer m.reconcileWG.Done()

		select {
		case <-time.After(catalogSyncStartupDelay):
		case <-ctx.Done():
			return
		}

		m.runCatalogSyncPass(ctx)

		ticker := time.NewTicker(catalogSyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.runCatalogSyncPass(ctx)
			}
		}
	}()
}

// runCatalogSyncPass enumerates installed apps and applies catalog manifest
// drift to each one in series. Pass-by-pass serialization (rather than
// fan-out) avoids head-of-line blocking on reconcileMu and gives concurrent
// operations (UpdateImage, UpdateListeners, reconcile) a chance to interleave
// between apps.
func (m *AppManager) runCatalogSyncPass(ctx context.Context) {
	if os.Getenv(catalogSyncEnvDisable) == "1" {
		return
	}
	host := m.currentSyncHost()
	if host == nil {
		return
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return
	}
	apps := state.ListApps()
	for _, inst := range apps {
		if ctx.Err() != nil {
			return
		}
		if inst == nil {
			continue
		}
		if inst.Mode() != ModeService {
			continue
		}
		if inst.SyncDisabled {
			continue
		}
		if inst.CatalogSource == "" {
			continue
		}
		if err := m.syncManifestIfDrifted(ctx, inst.InstanceID, false, nil); err != nil {
			log.Printf("WARN: catalog sync %s: %v", inst.InstanceID, err)
		}
	}
}

// RecordInstallStateInput captures the post-install metadata that the install
// handler hands back to the app manager so that catalog manifest sync can
// later replay the install. The handler owns the parsed pipeline result and
// the fetched init scripts; the manager owns the on-disk persistence.
type RecordInstallStateInput struct {
	CatalogManifestHash string
	InitScriptHashes    map[string]string
	InstallState        *InstallState
}

// RecordInstallState persists the catalog manifest hash, init script hashes,
// and install state for an installed app instance. Called by the install
// handler after Install + OIDC client registration succeed.
//
// Does NOT acquire reconcileMu — that would block the install HTTP handler
// behind any in-flight sync ticker pass (observed: 3-minute install hang
// when the first sync tick was processing a stale legacy app for 1m21s).
// The race window where sync could observe the just-installed app without
// install_state.json is closed by syncManifestIfDrifted's grace-period
// check on appInst.CreatedAt below.
//
// Also clears LastSyncAttemptHash and LastSyncError. If sync DID race
// during the install handler's window and recorded a transient legacy-path
// failure, this clear lets the next sync tick re-evaluate cleanly.
func (m *AppManager) RecordInstallState(ctx context.Context, instanceID string, in RecordInstallStateInput) error {
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	inst, exists := state.GetApp(instanceID)
	if !exists {
		return errAppNotFound(instanceID)
	}
	inst.CatalogManifestHash = in.CatalogManifestHash
	inst.InitScriptHashes = in.InitScriptHashes
	inst.LastSyncAttemptHash = ""
	inst.LastSyncError = ""
	if err := state.StoreAppMetadata(inst); err != nil {
		return fmt.Errorf("persist install metadata: %w", err)
	}
	if in.InstallState != nil {
		if in.InstallState.InstanceID == "" {
			in.InstallState.InstanceID = instanceID
		}
		if err := state.StoreInstallState(instanceID, in.InstallState); err != nil {
			return fmt.Errorf("persist install state: %w", err)
		}
	}
	return nil
}

// installGracePeriod is the window after install during which sync skips an
// app entirely. Gives the install handler time to call RecordInstallState
// without racing the auto-sync ticker. The handler completes the call within
// milliseconds in the normal case; this grace period is generous to cover
// any transient I/O delays.
const installGracePeriod = 5 * time.Minute

// SyncManifest is the public entrypoint for /sync/trigger. Returns an error
// if no sync host is wired or this node is not the cluster leader (writes
// must always go through the leader). The throttle clear happens inside
// syncManifestIfDrifted under reconcileMu so it cannot race with a
// concurrent auto-sync pass writing the same fields on the cached
// AppInstance pointer.
func (m *AppManager) SyncManifest(ctx context.Context, instanceID string) error {
	if m.currentSyncHost() == nil {
		return ErrSyncHostUnavailable
	}
	if err := m.ensureKernelLeader(); err != nil {
		return err
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	if err := m.rejectIfTransitionInProgress(state, instanceID, TransitionFenceSyncTrigger); err != nil {
		return err
	}
	return m.syncManifestIfDrifted(ctx, instanceID, true, nil)
}

// SetSyncDisabled persists the per-app sync opt-out flag. When clearing
// (disabled=false), throttle state is also cleared so the next sync pass
// re-evaluates the catalog hash. Must run on the cluster leader so the
// metadata write is the canonical truth across the cluster.
func (m *AppManager) SetSyncDisabled(ctx context.Context, instanceID string, disabled bool) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	if err := m.ensureKernelLeader(); err != nil {
		return err
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	inst, exists := state.GetApp(instanceID)
	if !exists {
		return errAppNotFound(instanceID)
	}
	if err := m.rejectIfTransitionInProgress(state, instanceID, transitionFenceForSyncDisabled(disabled)); err != nil {
		return err
	}
	inst.SyncDisabled = disabled
	if !disabled {
		inst.LastSyncAttemptHash = ""
		inst.LastSyncError = ""
	}
	return state.StoreAppMetadata(inst)
}

// SyncStatus is a read-only projection of an app's sync state, returned by
// the /sync/status endpoint.
type SyncStatus struct {
	InstanceID          string `json:"instance_id"`
	CatalogManifestHash string `json:"catalog_manifest_hash"`
	LastSyncAttemptHash string `json:"last_sync_attempt_hash"`
	LastSyncError       string `json:"last_sync_error"`
	SyncDisabled        bool   `json:"sync_disabled"`
	IsLegacyBackfill    bool   `json:"is_legacy_backfill"`
	SyncBlocked         bool   `json:"sync_blocked,omitempty"`
	SyncBlockedReason   string `json:"sync_blocked_reason,omitempty"`
}

// GetSyncStatus returns the current sync state for an app.
func (m *AppManager) GetSyncStatus(ctx context.Context, instanceID string) (*SyncStatus, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	inst, exists := state.GetApp(instanceID)
	if !exists {
		return nil, errAppNotFound(instanceID)
	}
	st := &SyncStatus{
		InstanceID:          inst.InstanceID,
		CatalogManifestHash: inst.CatalogManifestHash,
		LastSyncAttemptHash: inst.LastSyncAttemptHash,
		LastSyncError:       inst.LastSyncError,
		SyncDisabled:        inst.SyncDisabled,
	}
	if installSt, err := state.LoadInstallState(instanceID); err == nil && installSt != nil {
		st.IsLegacyBackfill = installSt.IsLegacyBackfill
		st.SyncBlocked = installSt.SyncBlocked
		st.SyncBlockedReason = installSt.SyncBlockedReason
	}
	return st, nil
}

// RefreshInstallSystemContext re-snapshots the live host system context and
// stages it through catalog sync. AR-1 mitigation: after admin enrolls a new
// portal or changes timezone, this endpoint repairs the frozen context.
// Leader-only.
func (m *AppManager) RefreshInstallSystemContext(ctx context.Context, instanceID string) error {
	host := m.currentSyncHost()
	if host == nil {
		return ErrSyncHostUnavailable
	}
	if err := m.ensureKernelLeader(); err != nil {
		return err
	}
	finishSync, err := m.beginSyncAttempt(instanceID)
	if err != nil {
		return err
	}
	defer finishSync()

	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	if _, exists := state.GetApp(instanceID); !exists {
		return errAppNotFound(instanceID)
	}
	if err := m.rejectIfTransitionInProgress(state, instanceID, TransitionFenceSyncRefreshContext); err != nil {
		return err
	}
	st, err := state.LoadInstallState(instanceID)
	if err != nil && !errors.Is(err, ErrInstallStateNotFound) {
		return err
	}
	if st == nil {
		st = &InstallState{InstanceID: instanceID, IsLegacyBackfill: true}
	}
	fresh := host.CurrentInstallSystemContext()

	if st.IsLegacyBackfill || st.SchemaVersion < installStateSchemaVersionConfig {
		hadInstallState := !errors.Is(err, ErrInstallStateNotFound)
		previous, err := st.Clone()
		if err != nil {
			return err
		}
		st.InstallSystemCtx = &fresh
		if err := state.StoreInstallState(instanceID, st); err != nil {
			return err
		}
		if err := m.syncManifestIfDriftedLocked(ctx, host, instanceID, true, nil); err != nil {
			var restoreErr error
			if hadInstallState && previous != nil {
				restoreErr = state.StoreInstallState(instanceID, previous)
			} else {
				restoreErr = state.DeleteInstallState(instanceID)
			}
			if restoreErr != nil {
				return errors.Join(err, fmt.Errorf("restore install context after sync failure: %w", restoreErr))
			}
			return err
		}
		return nil
	}

	return m.syncManifestIfDriftedLocked(ctx, host, instanceID, true, &fresh)
}
