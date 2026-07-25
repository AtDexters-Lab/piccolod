package app

import (
	"testing"
	"time"

	"piccolod/internal/api"
)

func TestAppMetadataRoundTripsCapabilityArtifactEffects(t *testing.T) {
	root := t.TempDir()
	state, err := NewFilesystemStateManager(root)
	if err != nil {
		t.Fatalf("NewFilesystemStateManager: %v", err)
	}
	definition, err := ParseAppDefinition([]byte(`
workspace_name: provider
services:
  main:
    image: provider:latest
    bind_ports: []
x-piccolo:
  mode: workspace
`))
	if err != nil {
		t.Fatalf("ParseAppDefinition: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	instance := &AppInstance{
		InstanceID:         "provider",
		Enabled:            true,
		PrimaryService:     "main",
		CreatedAt:          now,
		UpdatedAt:          now,
		Definition:         definition,
		ArtifactReferences: map[string]string{"model": "provider--artifact--model--abc"},
		AcceleratorDevices: []string{"/dev/accel/accel0", "/dev/dri/renderD128"},
		CapabilityBindings: map[string]string{api.CapabilityAIInferenceOpenAIV1: "provider\x00inference\x00/v3"},
	}
	if err := state.StoreApp(instance); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	reloaded, err := NewFilesystemStateManager(root)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	got, ok := reloaded.GetApp("provider")
	if !ok {
		t.Fatalf("reloaded app missing")
	}
	if got.ArtifactReferences["model"] != instance.ArtifactReferences["model"] ||
		len(got.AcceleratorDevices) != 2 ||
		got.CapabilityBindings[api.CapabilityAIInferenceOpenAIV1] != instance.CapabilityBindings[api.CapabilityAIInferenceOpenAIV1] {
		t.Fatalf("effect evidence did not round trip: %+v", got)
	}
}
