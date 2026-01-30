package app

import (
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
			Status:          "running",
			NetworkAnchorID: "anchor123",
			Containers:      map[string]string{"main": "container456"},
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
			Definition: &api.AppDefinition{
				Name:  "test-app",
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
			Status:          "stopped",
			NetworkAnchorID: "anchor789",
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
			Definition: &api.AppDefinition{
				Name:  "test-app-2",
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
		if cached.Status != "stopped" {
			t.Errorf("cached status mismatch: got %q, want %q", cached.Status, "stopped")
		}
		if cached.NetworkAnchorID != "anchor789" {
			t.Errorf("cached NetworkAnchorID mismatch: got %q, want %q", cached.NetworkAnchorID, "anchor789")
		}
	})
}
