package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"piccolod/internal/api"
)

func TestStoreAppMetadata(t *testing.T) {
	tmpDir := t.TempDir()

	// Create state manager
	fsm, err := NewFilesystemStateManager(tmpDir)
	if err != nil {
		t.Fatalf("NewFilesystemStateManager failed: %v", err)
	}

	t.Run("stores metadata without touching app.yaml", func(t *testing.T) {
		instanceID := "test-app-1"
		appDir := filepath.Join(tmpDir, "apps", instanceID)
		if err := os.MkdirAll(appDir, 0755); err != nil {
			t.Fatalf("setup failed: %v", err)
		}

		// Write initial app.yaml
		appYamlContent := "name: test-app\nimage: test:latest\n"
		appYamlPath := filepath.Join(appDir, "app.yaml")
		if err := os.WriteFile(appYamlPath, []byte(appYamlContent), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}

		// Store metadata
		appInst := &AppInstance{
			InstanceID:      instanceID,
			Enabled:         true,
			NetworkAnchorID: "anchor123",
			Containers:      map[string]string{"main": "container456"},
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
			Definition: &api.AppDefinition{
				Image: "test:latest",
			},
		}

		if err := fsm.StoreAppMetadata(appInst); err != nil {
			t.Fatalf("StoreAppMetadata failed: %v", err)
		}

		// Verify app.yaml is unchanged
		data, err := os.ReadFile(appYamlPath)
		if err != nil {
			t.Fatalf("ReadFile failed: %v", err)
		}
		if string(data) != appYamlContent {
			t.Errorf("app.yaml was modified: got %q, want %q", data, appYamlContent)
		}

		// Verify metadata.json was written
		metadataPath := filepath.Join(appDir, "metadata.json")
		if _, err := os.Stat(metadataPath); os.IsNotExist(err) {
			t.Error("metadata.json was not created")
		}
	})

	t.Run("updates cache on store", func(t *testing.T) {
		instanceID := "test-app-2"
		appDir := filepath.Join(tmpDir, "apps", instanceID)
		if err := os.MkdirAll(appDir, 0755); err != nil {
			t.Fatalf("setup failed: %v", err)
		}

		appInst := &AppInstance{
			InstanceID:      instanceID,
			Enabled:         false,
			NetworkAnchorID: "anchor789",
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
			Definition: &api.AppDefinition{
				Image: "test:latest",
			},
		}

		if err := fsm.StoreAppMetadata(appInst); err != nil {
			t.Fatalf("StoreAppMetadata failed: %v", err)
		}

		// Check cache
		cached, exists := fsm.GetApp(instanceID)
		if !exists {
			t.Error("app not found in cache after StoreAppMetadata")
		}
		if cached.Enabled != false {
			t.Errorf("cached Enabled mismatch: got %v, want false", cached.Enabled)
		}
		if cached.NetworkAnchorID != "anchor789" {
			t.Errorf("cached NetworkAnchorID mismatch: got %q, want %q", cached.NetworkAnchorID, "anchor789")
		}
	})
}

func TestRemoveIncompleteAppCleansOneFileStoreAppPublication(t *testing.T) {
	state, err := NewFilesystemStateManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystemStateManager: %v", err)
	}
	app := &AppInstance{
		InstanceID: "partial",
		Definition: &api.AppDefinition{
			Services: map[string]api.AppService{
				"main": {Image: "example.invalid/app:latest"},
			},
		},
	}
	injected := errors.New("injected metadata publication failure")
	state.storeAppMetadataHook = func(string, *AppInstance) error { return injected }
	if err := state.StoreApp(app); !errors.Is(err, injected) {
		t.Fatalf("StoreApp error = %v, want injected metadata failure", err)
	}
	appDir := filepath.Join(state.appsDir, app.InstanceID)
	publication, err := inspectAppPublication(appDir)
	if err != nil {
		t.Fatalf("inspectAppPublication: %v", err)
	}
	if publication != appPublicationIncomplete {
		t.Fatalf("publication = %v, want incomplete", publication)
	}
	if err := state.removeIncompleteApp(app.InstanceID); err != nil {
		t.Fatalf("removeIncompleteApp: %v", err)
	}
	if _, err := os.Stat(appDir); !os.IsNotExist(err) {
		t.Fatalf("one-file StoreApp debris survived: %v", err)
	}
}

func TestRemoveIncompleteAppRefusesCompletePublication(t *testing.T) {
	state, err := NewFilesystemStateManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystemStateManager: %v", err)
	}
	app := &AppInstance{
		InstanceID: "installed",
		Definition: &api.AppDefinition{
			Services: map[string]api.AppService{
				"main": {Image: "example.invalid/app:latest"},
			},
		},
	}
	if err := state.StoreApp(app); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}
	if err := state.removeIncompleteApp(app.InstanceID); err == nil {
		t.Fatal("complete publication was removed as debris")
	}
	for _, name := range []string{"app.yaml", "metadata.json"} {
		if _, err := os.Stat(filepath.Join(state.appsDir, app.InstanceID, name)); err != nil {
			t.Fatalf("complete publication file %s was damaged: %v", name, err)
		}
	}
}

func TestUpdateAppEnabledCommitsDiskBeforeCache(t *testing.T) {
	tmpDir := t.TempDir()
	fsm, err := NewFilesystemStateManager(tmpDir)
	if err != nil {
		t.Fatalf("NewFilesystemStateManager: %v", err)
	}
	instanceID := "enabled-commit"
	appDir := filepath.Join(tmpDir, "apps", instanceID)
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("mkdir app: %v", err)
	}
	app := &AppInstance{
		InstanceID: instanceID,
		Enabled:    false,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if err := fsm.StoreAppMetadata(app); err != nil {
		t.Fatalf("StoreAppMetadata: %v", err)
	}

	fsm.storeAppMetadataHook = func(string, *AppInstance) error {
		return errors.New("injected write failure")
	}
	if err := fsm.UpdateAppEnabled(instanceID, true); err == nil {
		t.Fatal("expected enabled-state write failure")
	}
	if cached, ok := fsm.GetApp(instanceID); !ok || cached.Enabled {
		t.Fatalf("cache changed after failed commit: ok=%v app=%+v", ok, cached)
	}
	data, err := os.ReadFile(filepath.Join(appDir, "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var metadata AppMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if metadata.Enabled == nil || *metadata.Enabled {
		t.Fatalf("disk changed after failed commit: enabled=%v", metadata.Enabled)
	}

	fsm.storeAppMetadataHook = nil
	if err := fsm.UpdateAppEnabled(instanceID, true); err != nil {
		t.Fatalf("UpdateAppEnabled success: %v", err)
	}
	if cached, ok := fsm.GetApp(instanceID); !ok || !cached.Enabled {
		t.Fatalf("cache did not publish successful commit: ok=%v app=%+v", ok, cached)
	}
	data, err = os.ReadFile(filepath.Join(appDir, "metadata.json"))
	if err != nil {
		t.Fatalf("read committed metadata: %v", err)
	}
	metadata = AppMetadata{}
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatalf("unmarshal committed metadata: %v", err)
	}
	if metadata.Enabled == nil || !*metadata.Enabled {
		t.Fatalf("disk did not commit enabled=true: enabled=%v", metadata.Enabled)
	}
}
