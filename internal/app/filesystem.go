package app

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/fsutil"
	"piccolod/internal/state/paths"
)

const (
	AppsDir  = "apps"
	CacheDir = "cache"
)

// FilesystemStateManager manages app state using filesystem as source of truth
type FilesystemStateManager struct {
	stateDir string
	appsDir  string
	cacheDir string

	// In-memory cache for performance
	cache   map[string]*AppInstance
	cacheMu sync.RWMutex

	// File system mutex for atomic operations
	fsMu sync.Mutex

	// Test hook for fault-injecting install_state.json writes.
	storeInstallStateHook func(instanceID string, st *InstallState) error

	// Test hook for fault-injecting manifest update transaction writes.
	storeManifestUpdateTransactionHook func(instanceID string, txn *ManifestUpdateTransaction) error

	// Test hook for fault-injecting StoreApp after app.yaml is written and
	// before metadata.json is written.
	storeAppMetadataHook func(instanceID string, app *AppInstance) error
}

// AppMetadata represents runtime metadata stored separately from app.yaml.
type AppMetadata struct {
	InstanceID string `json:"instance_id"`
	Enabled    *bool  `json:"enabled,omitempty"` // pointer for migration detection from legacy Status field
	// Container runtime metadata.
	PrimaryService  string            `json:"primary_service,omitempty"`
	NetworkAnchorID string            `json:"network_anchor_id,omitempty"`
	Containers      map[string]string `json:"containers,omitempty"` // service name -> container ID
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	// CatalogSource tracks the catalog item name this app was installed from.
	// Used for icon lookup and update tracking.
	CatalogSource string `json:"catalog_source,omitempty"`
	// ActiveRootfs tracks the active rootfs volume ID per service (RFC 20260302).
	// nil = legacy install (use ServiceRootfsVolumeID without digest).
	ActiveRootfs map[string]string `json:"active_rootfs,omitempty"`
	// ClonedFrom tracks the origin instance ID this app was cloned from.
	ClonedFrom string `json:"cloned_from,omitempty"`
	// Init tracks per-service init script execution state.
	Init *InitState `json:"init,omitempty"`

	// Catalog manifest sync fields.

	// CatalogManifestHash is the SHA-256 of the raw catalog app.yaml bytes
	// captured at install (or after a successful sync). Used to detect drift.
	CatalogManifestHash string `json:"catalog_manifest_hash,omitempty"`
	// InitScriptHashes maps service name to SHA-256 of the init script file
	// content captured at install. Sync skips an app whose init scripts have
	// changed (manual reinstall required) since init scripts are never re-run
	// automatically.
	InitScriptHashes map[string]string `json:"init_script_hashes,omitempty"`
	// SyncDisabled, when true, opts this app out of automatic catalog sync.
	SyncDisabled bool `json:"sync_disabled,omitempty"`
	// LastSyncAttemptHash records the catalog hash of the most recent sync
	// attempt. Used to throttle retries: a hash that already failed will not
	// be re-attempted until the catalog publishes a different hash or the
	// admin clears the throttle via /sync/trigger or /sync/enable.
	LastSyncAttemptHash string `json:"last_sync_attempt_hash,omitempty"`
	// LastSyncError records a human-readable reason the most recent sync
	// attempt failed. Empty on success or when no attempt has been made.
	LastSyncError string `json:"last_sync_error,omitempty"`
}

// instanceToMetadata projects an AppInstance into the on-disk AppMetadata
// shape. The single source of truth for the field set, used by all three
// persistence write paths.
func instanceToMetadata(app *AppInstance) AppMetadata {
	return AppMetadata{
		InstanceID:          app.InstanceID,
		Enabled:             boolPtr(app.Enabled),
		PrimaryService:      app.PrimaryService,
		NetworkAnchorID:     app.NetworkAnchorID,
		Containers:          app.Containers,
		CreatedAt:           app.CreatedAt,
		UpdatedAt:           app.UpdatedAt,
		CatalogSource:       app.CatalogSource,
		ActiveRootfs:        app.ActiveRootfs,
		ClonedFrom:          app.ClonedFrom,
		Init:                app.Init,
		CatalogManifestHash: app.CatalogManifestHash,
		InitScriptHashes:    app.InitScriptHashes,
		SyncDisabled:        app.SyncDisabled,
		LastSyncAttemptHash: app.LastSyncAttemptHash,
		LastSyncError:       app.LastSyncError,
	}
}

// metadataIntoInstance applies an on-disk AppMetadata onto an in-memory
// AppInstance. Mirrors instanceToMetadata; shared between LoadApp and any
// other read path.
func metadataIntoInstance(meta AppMetadata, app *AppInstance) {
	app.InstanceID = meta.InstanceID
	app.PrimaryService = meta.PrimaryService
	app.NetworkAnchorID = meta.NetworkAnchorID
	app.Containers = meta.Containers
	app.CreatedAt = meta.CreatedAt
	app.UpdatedAt = meta.UpdatedAt
	app.CatalogSource = meta.CatalogSource
	app.ActiveRootfs = meta.ActiveRootfs
	app.ClonedFrom = meta.ClonedFrom
	app.Init = meta.Init
	app.CatalogManifestHash = meta.CatalogManifestHash
	app.InitScriptHashes = meta.InitScriptHashes
	app.SyncDisabled = meta.SyncDisabled
	app.LastSyncAttemptHash = meta.LastSyncAttemptHash
	app.LastSyncError = meta.LastSyncError
}

// InitState tracks init script execution across services.
type InitState struct {
	Services map[string]ServiceInitState `json:"services"`
}

// InitStatus constants for ServiceInitState.Status.
const (
	InitStatusPending   = "pending"
	InitStatusRunning   = "running"
	InitStatusCompleted = "completed"
	InitStatusFailed    = "failed"
)

// ServiceInitState tracks the execution state of a single service's init script.
type ServiceInitState struct {
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	ExitCode    int       `json:"exit_code,omitempty"`
	Error       string    `json:"error,omitempty"`
}

func boolPtr(b bool) *bool { return &b }

// AppMetaDir returns the control-plane metadata directory for an instance.
// Unlike layout.DataDir (which lives on the encrypted per-app volume and is
// destroyed by cleanupInstallResources on failure), this directory survives
// install-failure cleanup, making it suitable for diagnostic artifacts like
// init script logs.
func (fsm *FilesystemStateManager) AppMetaDir(instanceID string) string {
	return filepath.Join(fsm.appsDir, instanceID)
}

// NewFilesystemStateManager creates a new filesystem state manager
func NewFilesystemStateManager(stateDir string) (*FilesystemStateManager, error) {
	if stateDir == "" {
		stateDir = paths.CoreRoot()
	}

	info, err := os.Stat(stateDir)
	if err != nil {
		return nil, fmt.Errorf("state directory unavailable: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("state directory %s is not a directory", stateDir)
	}

	fsm := &FilesystemStateManager{
		stateDir: stateDir,
		appsDir:  filepath.Join(stateDir, AppsDir),
		cacheDir: filepath.Join(stateDir, CacheDir),
		cache:    make(map[string]*AppInstance),
	}

	// Create directory structure
	if err := fsm.initDirectories(); err != nil {
		return nil, fmt.Errorf("failed to initialize directories: %w", err)
	}

	// Load apps from filesystem into cache
	if err := fsm.loadCache(); err != nil {
		return nil, fmt.Errorf("failed to load cache: %w", err)
	}

	return fsm, nil
}

// initDirectories creates the required directory structure
func (fsm *FilesystemStateManager) initDirectories() error {
	dirs := []string{fsm.appsDir, fsm.cacheDir}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	return nil
}

// loadCache loads all apps from filesystem into memory cache
func (fsm *FilesystemStateManager) loadCache() error {
	entries, err := os.ReadDir(fsm.appsDir)
	if err != nil {
		return fmt.Errorf("failed to read apps directory: %w", err)
	}

	fsm.cacheMu.Lock()
	defer fsm.cacheMu.Unlock()

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Directory name is the instance ID
		instanceID := entry.Name()
		app, err := fsm.loadAppFromDisk(instanceID)
		if err != nil {
			// Log error but continue loading other apps
			fmt.Printf("Warning: failed to load app instance %s: %v\n", instanceID, err)
			continue
		}

		fsm.cache[instanceID] = app
	}

	return nil
}

// loadAppFromDisk loads a single app instance from filesystem.
// The instanceID parameter is the directory name under apps/.
func (fsm *FilesystemStateManager) loadAppFromDisk(instanceID string) (*AppInstance, error) {
	appDir := filepath.Join(fsm.appsDir, instanceID)

	// Load app.yaml
	appDefPath := filepath.Join(appDir, "app.yaml")
	appDefData, err := os.ReadFile(appDefPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read app.yaml: %w", err)
	}

	appDef, err := parseAppDefinitionWithLegacyMigration(appDefData, appDefPath, instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to parse app.yaml: %w", err)
	}

	// Load metadata.json
	metadataPath := filepath.Join(appDir, "metadata.json")
	metadataData, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata.json: %w", err)
	}

	// Use a migration struct that can read both old (Status) and new (Enabled) formats.
	var raw struct {
		AppMetadata
		Status string `json:"status"` // legacy field for migration
	}
	if err := json.Unmarshal(metadataData, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse metadata.json: %w", err)
	}

	// Determine Enabled value with migration from legacy Status field.
	enabled := true // default
	if raw.Enabled != nil {
		enabled = *raw.Enabled
	} else if raw.Status != "" {
		// Legacy migration: derive Enabled from old Status field
		enabled = raw.Status != "stopped"
	}

	// Create AppInstance with embedded definition
	app := &AppInstance{
		Enabled:    enabled,
		Definition: appDef,
	}
	metadataIntoInstance(raw.AppMetadata, app)

	// Fallback: if InstanceID is empty in metadata, use directory name
	if app.InstanceID == "" {
		app.InstanceID = instanceID
	}

	return app, nil
}

// BackupCurrentAppDefinition writes current app.yaml to app.prev.yaml for rollback.
// The instanceID parameter is the unique instance identifier.
func (fsm *FilesystemStateManager) BackupCurrentAppDefinition(instanceID string) error {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()
	appDir := filepath.Join(fsm.appsDir, instanceID)
	cur := filepath.Join(appDir, "app.yaml")
	prev := filepath.Join(appDir, "app.prev.yaml")
	data, err := os.ReadFile(cur)
	if err != nil {
		return fmt.Errorf("read current app.yaml: %w", err)
	}
	if err := fsutil.AtomicWriteFile(prev, data, 0644); err != nil {
		return fmt.Errorf("write app.prev.yaml: %w", err)
	}
	return nil
}

// GetPreviousAppDefinition reads app.prev.yaml if present.
// The instanceID parameter is the unique instance identifier.
func (fsm *FilesystemStateManager) GetPreviousAppDefinition(instanceID string) (*api.AppDefinition, error) {
	appDir := filepath.Join(fsm.appsDir, instanceID)
	prev := filepath.Join(appDir, "app.prev.yaml")
	data, err := os.ReadFile(prev)
	if err != nil {
		return nil, fmt.Errorf("previous definition not found: %w", err)
	}
	def, err := ParseAppDefinition(data)
	if err != nil {
		return nil, fmt.Errorf("parse previous app.yaml: %w", err)
	}
	return def, nil
}

// GetAppDefinition reads and parses app.yaml for a given app instance.
// The instanceID parameter is the unique instance identifier.
func (fsm *FilesystemStateManager) GetAppDefinition(instanceID string) (*api.AppDefinition, error) {
	appDir := filepath.Join(fsm.appsDir, instanceID)
	appDefPath := filepath.Join(appDir, "app.yaml")
	data, err := os.ReadFile(appDefPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read app.yaml: %w", err)
	}
	appDef, err := parseAppDefinitionWithLegacyMigration(data, appDefPath, instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to parse app.yaml: %w", err)
	}
	return appDef, nil
}

// parseAppDefinitionWithLegacyMigration wraps ParseAppDefinition with the
// one-shot pre-v2 resource-stewardship migration for persisted app.yaml
// files carried forward from older piccolod installs. On parse-time
// rejection with IsLegacyResourcesSchemaError, strips the legacy
// resources declarations and persists the rewritten file so subsequent
// reads short-circuit the fast path. See plan D-6 and
// docs/runtime/resource-stewardship-rollout.md.
//
// This block can be removed once all deployed installs have been
// migrated past the rollout window (tracked alongside the LegacyLimits
// catch-all in api/types.go).
func parseAppDefinitionWithLegacyMigration(data []byte, appDefPath, instanceID string) (*api.AppDefinition, error) {
	def, err := ParseAppDefinition(data)
	if err == nil {
		return def, nil
	}
	if !IsLegacyResourcesSchemaError(err) {
		return nil, err
	}
	migrated, changed, mErr := MigrateLegacyResourcesYAML(data)
	if mErr != nil || !changed {
		return nil, err
	}
	def2, pErr := ParseAppDefinition(migrated)
	if pErr != nil {
		return nil, fmt.Errorf("migration attempted but still invalid: %w", pErr)
	}
	if wErr := os.WriteFile(appDefPath, migrated, 0o644); wErr != nil {
		log.Printf("WARN: load %s: legacy app.yaml migrated in-memory but persist failed: %v", instanceID, wErr)
	} else {
		log.Printf("INFO: load %s: migrated legacy app.yaml to new-shape (run 'piccolod catalog sync' to pick up current resource declarations)", instanceID)
	}
	return def2, nil
}

// StoreApp saves app definition and metadata to filesystem.
// The app instance is stored under apps/{InstanceID}/.
//
// TODO: Callers mutate the cached AppInstance in-place before calling StoreApp.
// If StoreApp fails (e.g. disk I/O), the in-memory cache diverges from disk.
// Consider accepting a copy and updating the cache only on successful persist.
func (fsm *FilesystemStateManager) StoreApp(app *AppInstance) error {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()

	if app.Definition == nil {
		return fmt.Errorf("no app definition provided for instance %s", app.InstanceID)
	}
	def := app.Definition

	appDir := filepath.Join(fsm.appsDir, app.InstanceID)
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return fmt.Errorf("failed to create app directory: %w", err)
	}

	// Store app.yaml (atomic write to prevent corruption during interruption)
	appDefPath := filepath.Join(appDir, "app.yaml")
	appDefData, err := SerializeAppDefinition(def)
	if err != nil {
		return fmt.Errorf("failed to serialize app definition: %w", err)
	}

	if err := fsutil.AtomicWriteFile(appDefPath, appDefData, 0644); err != nil {
		return fmt.Errorf("failed to write app.yaml: %w", err)
	}

	// Store metadata.json with runtime fields (atomic write)
	metadata := instanceToMetadata(app)

	metadataData, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize metadata: %w", err)
	}

	metadataPath := filepath.Join(appDir, "metadata.json")
	if fsm.storeAppMetadataHook != nil {
		if err := fsm.storeAppMetadataHook(app.InstanceID, app); err != nil {
			return fmt.Errorf("failed to write metadata.json: %w", err)
		}
	}
	if err := fsutil.AtomicWriteFile(metadataPath, metadataData, 0644); err != nil {
		return fmt.Errorf("failed to write metadata.json: %w", err)
	}

	// Update cache keyed by InstanceID
	fsm.cacheMu.Lock()
	fsm.cache[app.InstanceID] = app
	fsm.cacheMu.Unlock()

	return nil
}

// UpdateAppRuntime updates app runtime metadata (container ID) and persists metadata.json.
// The instanceID parameter is the unique instance identifier.
// The containerID parameter updates the primary service container ID via SetPrimaryContainerID().
func (fsm *FilesystemStateManager) UpdateAppRuntime(instanceID, containerID string) error {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()

	// Update cache first
	fsm.cacheMu.Lock()
	app, exists := fsm.cache[instanceID]
	if !exists {
		fsm.cacheMu.Unlock()
		return fmt.Errorf("app instance not found: %s", instanceID)
	}
	if containerID != "" {
		app.SetPrimaryContainerID(containerID)
	}
	app.UpdatedAt = time.Now()
	metadata := instanceToMetadata(app)
	fsm.cacheMu.Unlock()

	// Update filesystem
	appDir := filepath.Join(fsm.appsDir, instanceID)
	metadataPath := filepath.Join(appDir, "metadata.json")

	metadataData, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize metadata: %w", err)
	}

	if err := fsutil.AtomicWriteFile(metadataPath, metadataData, 0644); err != nil {
		return fmt.Errorf("failed to write metadata.json: %w", err)
	}

	return nil
}

// StoreAppMetadata persists only the runtime metadata (metadata.json) without touching app.yaml.
// Use this for runtime state changes like container IDs, enabled state, etc.
// This is more efficient than StoreApp when the app definition hasn't changed.
func (fsm *FilesystemStateManager) StoreAppMetadata(app *AppInstance) error {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()

	appDir := filepath.Join(fsm.appsDir, app.InstanceID)

	// Ensure directory exists (should already exist, but be safe)
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return fmt.Errorf("failed to create app directory: %w", err)
	}

	metadata := instanceToMetadata(app)

	metadataData, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize metadata: %w", err)
	}

	metadataPath := filepath.Join(appDir, "metadata.json")
	if err := fsutil.AtomicWriteFile(metadataPath, metadataData, 0644); err != nil {
		return fmt.Errorf("failed to write metadata.json: %w", err)
	}

	// Update cache
	fsm.cacheMu.Lock()
	fsm.cache[app.InstanceID] = app
	fsm.cacheMu.Unlock()

	return nil
}

// UpdateAppEnabled updates the app's Enabled flag and persists it.
// The instanceID parameter is the unique instance identifier.
func (fsm *FilesystemStateManager) UpdateAppEnabled(instanceID string, enabled bool) error {
	fsm.cacheMu.Lock()
	app, exists := fsm.cache[instanceID]
	if !exists {
		fsm.cacheMu.Unlock()
		return fmt.Errorf("app instance not found: %s", instanceID)
	}
	app.Enabled = enabled
	app.UpdatedAt = time.Now()
	fsm.cacheMu.Unlock()
	return fsm.StoreAppMetadata(app)
}

// GetApp retrieves an app instance from cache by instance ID (fast access).
func (fsm *FilesystemStateManager) GetApp(instanceID string) (*AppInstance, bool) {
	fsm.cacheMu.RLock()
	defer fsm.cacheMu.RUnlock()

	app, exists := fsm.cache[instanceID]
	return app, exists
}

// ListInstanceIDs returns all instance IDs currently in the cache.
// Used for conflict detection during instance ID generation.
func (fsm *FilesystemStateManager) ListInstanceIDs() []string {
	fsm.cacheMu.RLock()
	defer fsm.cacheMu.RUnlock()

	ids := make([]string, 0, len(fsm.cache))
	for id := range fsm.cache {
		ids = append(ids, id)
	}
	return ids
}

// ListApps returns all apps from cache
func (fsm *FilesystemStateManager) ListApps() []*AppInstance {
	fsm.cacheMu.RLock()
	defer fsm.cacheMu.RUnlock()

	apps := make([]*AppInstance, 0, len(fsm.cache))
	for _, app := range fsm.cache {
		apps = append(apps, app)
	}

	return apps
}

const generationsFile = "generations.json"

// LoadTupleState reads the tuple generation state for an app instance.
// Returns nil, nil if no generations.json exists (app has never been updated).
func (fsm *FilesystemStateManager) LoadTupleState(instanceID string) (*TupleState, error) {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()

	genPath := filepath.Join(fsm.appsDir, instanceID, generationsFile)
	data, err := os.ReadFile(genPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read generations.json: %w", err)
	}

	var ts TupleState
	if err := json.Unmarshal(data, &ts); err != nil {
		return nil, fmt.Errorf("parse generations.json: %w", err)
	}
	return &ts, nil
}

// StoreTupleState writes the tuple generation state for an app instance atomically.
func (fsm *FilesystemStateManager) StoreTupleState(instanceID string, state *TupleState) error {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()

	appDir := filepath.Join(fsm.appsDir, instanceID)
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return fmt.Errorf("create app directory: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize generations: %w", err)
	}

	genPath := filepath.Join(appDir, generationsFile)
	if err := fsutil.AtomicWriteFile(genPath, data, 0644); err != nil {
		return fmt.Errorf("write generations.json: %w", err)
	}
	return nil
}

// RemoveApp removes an app instance from both filesystem and cache.
// The instanceID parameter is the unique instance identifier.
func (fsm *FilesystemStateManager) RemoveApp(instanceID string) error {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()

	// Remove from filesystem
	appDir := filepath.Join(fsm.appsDir, instanceID)
	if err := os.RemoveAll(appDir); err != nil {
		return fmt.Errorf("failed to remove app directory: %w", err)
	}

	// Remove from cache
	fsm.cacheMu.Lock()
	delete(fsm.cache, instanceID)
	fsm.cacheMu.Unlock()

	return nil
}
