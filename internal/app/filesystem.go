package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/fsutil"
	"piccolod/internal/state/paths"
)

const (
	AppsDir    = "apps"
	EnabledDir = "enabled"
	CacheDir   = "cache"
)

// FilesystemStateManager manages app state using filesystem as source of truth
type FilesystemStateManager struct {
	stateDir   string
	appsDir    string
	enabledDir string
	cacheDir   string

	// In-memory cache for performance
	cache   map[string]*AppInstance
	cacheMu sync.RWMutex

	// File system mutex for atomic operations
	fsMu sync.Mutex
}

// AppMetadata represents runtime metadata stored separately from app.yaml.
// Note: The actual enabled state is tracked via symlinks in the enabled/ directory,
// not in this struct. See EnableApp/DisableApp/IsAppEnabled methods.
type AppMetadata struct {
	InstanceID  string `json:"instance_id"`
	DisplayName string `json:"display_name,omitempty"`
	AppName     string `json:"app_name"`
	Status      string `json:"status"` // "created", "running", "stopped", "error"
	// Container runtime metadata.
	PrimaryService  string            `json:"primary_service,omitempty"`
	NetworkAnchorID string            `json:"network_anchor_id,omitempty"`
	Containers      map[string]string `json:"containers,omitempty"` // service name -> container ID
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// NewFilesystemStateManager creates a new filesystem state manager
func NewFilesystemStateManager(stateDir string) (*FilesystemStateManager, error) {
	if stateDir == "" {
		stateDir = paths.Root()
	}

	info, err := os.Stat(stateDir)
	if err != nil {
		return nil, fmt.Errorf("state directory unavailable: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("state directory %s is not a directory", stateDir)
	}

	fsm := &FilesystemStateManager{
		stateDir:   stateDir,
		appsDir:    filepath.Join(stateDir, AppsDir),
		enabledDir: filepath.Join(stateDir, EnabledDir),
		cacheDir:   filepath.Join(stateDir, CacheDir),
		cache:      make(map[string]*AppInstance),
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
	dirs := []string{fsm.appsDir, fsm.enabledDir, fsm.cacheDir}

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

	appDef, err := ParseAppDefinition(appDefData)
	if err != nil {
		return nil, fmt.Errorf("failed to parse app.yaml: %w", err)
	}

	// Load metadata.json
	metadataPath := filepath.Join(appDir, "metadata.json")
	metadataData, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata.json: %w", err)
	}

	var metadata AppMetadata
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		return nil, fmt.Errorf("failed to parse metadata.json: %w", err)
	}

	// Create AppInstance with embedded definition
	app := &AppInstance{
		InstanceID:      metadata.InstanceID,
		DisplayName:     metadata.DisplayName,
		Status:          metadata.Status,
		PrimaryService:  metadata.PrimaryService,
		NetworkAnchorID: metadata.NetworkAnchorID,
		Containers:      metadata.Containers,
		CreatedAt:       metadata.CreatedAt,
		UpdatedAt:       metadata.UpdatedAt,
		Definition:      appDef,
	}

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
	appDef, err := ParseAppDefinition(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse app.yaml: %w", err)
	}
	return appDef, nil
}

// StoreApp saves app definition and metadata to filesystem.
// The app instance is stored under apps/{InstanceID}/.
// Uses app.Definition for the app.yaml; the separate appDef parameter is kept for
// backward compatibility but is ignored if app.Definition is set.
func (fsm *FilesystemStateManager) StoreApp(app *AppInstance, appDef *api.AppDefinition) error {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()

	// Use embedded definition if available, fall back to parameter
	def := app.Definition
	if def == nil {
		def = appDef
	}
	if def == nil {
		return fmt.Errorf("no app definition provided")
	}

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
	// AppName is stored for backward compatibility with existing metadata.json files
	metadata := AppMetadata{
		InstanceID:      app.InstanceID,
		DisplayName:     app.DisplayName,
		AppName:         def.Name,
		Status:          app.Status,
		PrimaryService:  app.PrimaryService,
		NetworkAnchorID: app.NetworkAnchorID,
		Containers:      app.Containers,
		CreatedAt:       app.CreatedAt,
		UpdatedAt:       app.UpdatedAt,
	}

	metadataData, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize metadata: %w", err)
	}

	metadataPath := filepath.Join(appDir, "metadata.json")
	if err := fsutil.AtomicWriteFile(metadataPath, metadataData, 0644); err != nil {
		return fmt.Errorf("failed to write metadata.json: %w", err)
	}

	// Update cache keyed by InstanceID
	fsm.cacheMu.Lock()
	fsm.cache[app.InstanceID] = app
	fsm.cacheMu.Unlock()

	return nil
}

// UpdateAppRuntime updates app runtime metadata (status and container ID) and persists metadata.json.
// The instanceID parameter is the unique instance identifier.
// The containerID parameter updates the primary service container ID via SetPrimaryContainerID().
func (fsm *FilesystemStateManager) UpdateAppRuntime(instanceID, status, containerID string) error {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()

	// Update cache first
	fsm.cacheMu.Lock()
	app, exists := fsm.cache[instanceID]
	if !exists {
		fsm.cacheMu.Unlock()
		return fmt.Errorf("app instance not found: %s", instanceID)
	}
	if status != "" {
		app.Status = status
	}
	if containerID != "" {
		app.SetPrimaryContainerID(containerID)
	}
	app.UpdatedAt = time.Now()
	createdAt := app.CreatedAt
	displayName := app.DisplayName
	appName := app.AppName() // Use method to get name from Definition
	primaryService := app.PrimaryService
	networkAnchorID := app.NetworkAnchorID
	containers := app.Containers
	fsm.cacheMu.Unlock()

	// Update filesystem
	appDir := filepath.Join(fsm.appsDir, instanceID)
	metadataPath := filepath.Join(appDir, "metadata.json")

	metadata := AppMetadata{
		InstanceID:      instanceID,
		DisplayName:     displayName,
		AppName:         appName,
		Status:          app.Status,
		PrimaryService:  primaryService,
		NetworkAnchorID: networkAnchorID,
		Containers:      containers,
		CreatedAt:       createdAt,
		UpdatedAt:       app.UpdatedAt,
	}

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
// Use this for runtime state changes like container IDs, status updates, etc.
// This is more efficient than StoreApp when the app definition hasn't changed.
func (fsm *FilesystemStateManager) StoreAppMetadata(app *AppInstance) error {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()

	appDir := filepath.Join(fsm.appsDir, app.InstanceID)

	// Ensure directory exists (should already exist, but be safe)
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return fmt.Errorf("failed to create app directory: %w", err)
	}

	// Get app name from definition if available
	appName := ""
	if app.Definition != nil {
		appName = app.Definition.Name
	}

	metadata := AppMetadata{
		InstanceID:      app.InstanceID,
		DisplayName:     app.DisplayName,
		AppName:         appName,
		Status:          app.Status,
		PrimaryService:  app.PrimaryService,
		NetworkAnchorID: app.NetworkAnchorID,
		Containers:      app.Containers,
		CreatedAt:       app.CreatedAt,
		UpdatedAt:       app.UpdatedAt,
	}

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

// UpdateAppStatus updates just the app status and updated timestamp.
// The instanceID parameter is the unique instance identifier.
func (fsm *FilesystemStateManager) UpdateAppStatus(instanceID, status string) error {
	return fsm.UpdateAppRuntime(instanceID, status, "")
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

	// Remove enabled symlink if it exists
	enabledPath := filepath.Join(fsm.enabledDir, instanceID)
	_ = os.Remove(enabledPath) // Ignore error if symlink doesn't exist

	// Remove from cache
	fsm.cacheMu.Lock()
	delete(fsm.cache, instanceID)
	fsm.cacheMu.Unlock()

	return nil
}

// EnableApp creates a symlink to enable app instance (systemctl-style).
// The instanceID parameter is the unique instance identifier.
func (fsm *FilesystemStateManager) EnableApp(instanceID string) error {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()

	appDir := filepath.Join(fsm.appsDir, instanceID)
	enabledPath := filepath.Join(fsm.enabledDir, instanceID)

	// Check if app instance exists
	if _, err := os.Stat(appDir); err != nil {
		return fmt.Errorf("app instance not found: %s", instanceID)
	}

	// Create symlink (relative path for portability)
	relativePath := filepath.Join("..", AppsDir, instanceID)
	if err := os.Symlink(relativePath, enabledPath); err != nil {
		if os.IsExist(err) {
			return nil // Already enabled
		}
		return fmt.Errorf("failed to create symlink: %w", err)
	}

	return nil
}

// DisableApp removes the symlink to disable app instance.
// The instanceID parameter is the unique instance identifier.
func (fsm *FilesystemStateManager) DisableApp(instanceID string) error {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()

	enabledPath := filepath.Join(fsm.enabledDir, instanceID)
	if err := os.Remove(enabledPath); err != nil {
		if os.IsNotExist(err) {
			return nil // Already disabled
		}
		return fmt.Errorf("failed to remove symlink: %w", err)
	}

	return nil
}

// IsAppEnabled checks if app instance is enabled (symlink exists).
// The instanceID parameter is the unique instance identifier.
func (fsm *FilesystemStateManager) IsAppEnabled(instanceID string) bool {
	enabledPath := filepath.Join(fsm.enabledDir, instanceID)
	_, err := os.Lstat(enabledPath)
	return err == nil
}

// ListEnabledApps returns instance IDs of all enabled app instances.
func (fsm *FilesystemStateManager) ListEnabledApps() ([]string, error) {
	entries, err := os.ReadDir(fsm.enabledDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read enabled directory: %w", err)
	}

	var enabled []string
	for _, entry := range entries {
		// Only count symlinks
		if entry.Type()&os.ModeSymlink != 0 {
			enabled = append(enabled, entry.Name())
		}
	}

	return enabled, nil
}
