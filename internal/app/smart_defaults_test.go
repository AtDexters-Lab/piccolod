package app

import (
	"context"
	"os"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/state/paths"
)

// setupTestAppManager creates an AppManager backed by a temp dir with optional pre-existing apps.
// Each app entry is {instanceID: definition}. Pass nil definition for apps that only need
// instance-level presence (no listeners to check).
func setupTestAppManager(t *testing.T, existingApps map[string]*api.AppDefinition) *AppManager {
	t.Helper()
	tempDir := t.TempDir()

	if os.Getenv("PICCOLO_ALLOW_UNMOUNTED_TESTS") != "1" {
		t.Skip("set PICCOLO_ALLOW_UNMOUNTED_TESTS=1 to run without mounted volumes")
	}
	t.Setenv("PICCOLO_STATE_DIR", tempDir)
	paths.SetRootForTest(tempDir)
	t.Cleanup(func() { paths.SetRootForTest("") })

	mock := NewMockContainerManager()
	mgr, err := NewAppManager(mock, tempDir)
	if err != nil {
		t.Fatalf("NewAppManager: %v", err)
	}
	mgr.ForceLockState(false)
	mgr.SetMountVerifier(func(string) error { return nil })

	// Initialize state by calling List once.
	if _, err := mgr.List(context.Background()); err != nil {
		t.Fatalf("initial List: %v", err)
	}

	// Pre-populate apps via the filesystem state manager.
	if len(existingApps) > 0 {
		state, err := mgr.ensureStateManager()
		if err != nil {
			t.Fatalf("ensureStateManager: %v", err)
		}
		for id, def := range existingApps {
			if def == nil {
				def = &api.AppDefinition{
					Image:         "docker.io/library/test:latest",
					WorkspaceName: id,
					Extensions:    map[string]interface{}{"mode": "workspace"},
				}
			}
			inst := &AppInstance{
				InstanceID: id,
				Enabled:    true,
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
				Definition: def,
			}
			if err := state.StoreApp(inst); err != nil {
				t.Fatalf("StoreApp(%s): %v", id, err)
			}
		}
	}

	return mgr
}

// --- IsInstanceIDTaken ---

func TestIsInstanceIDTaken(t *testing.T) {
	mgr := setupTestAppManager(t, map[string]*api.AppDefinition{
		"myworkspace": nil,
	})
	ctx := context.Background()

	taken, err := IsInstanceIDTaken(ctx, mgr, "myworkspace")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !taken {
		t.Fatal("expected myworkspace to be taken")
	}

	taken, err = IsInstanceIDTaken(ctx, mgr, "other")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if taken {
		t.Fatal("expected 'other' to be free")
	}
}

// --- FindFreeName ---

func TestFindFreeName(t *testing.T) {
	mgr := setupTestAppManager(t, nil)
	ctx := context.Background()

	// No conflicts — base name returned as-is.
	name, err := FindFreeName(ctx, mgr, "blog")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "blog" {
		t.Fatalf("expected 'blog', got %q", name)
	}
}

func TestFindFreeName_SubdomainFreeButIDTaken(t *testing.T) {
	// The instance ID "code" is taken (workspace app, no listeners) but the subdomain
	// "code" is free (no listener with that name). FindFreeName must still skip it.
	mgr := setupTestAppManager(t, map[string]*api.AppDefinition{
		"code": {
			Image:         "docker.io/library/code:latest",
			WorkspaceName: "code",
			Extensions:    map[string]interface{}{"mode": "workspace"},
			// No listeners — so subdomain "code" is technically free.
		},
	})
	ctx := context.Background()

	name, err := FindFreeName(ctx, mgr, "code")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name == "code" {
		t.Fatal("expected FindFreeName to skip 'code' because instance ID is taken")
	}
	if name != "code1" {
		t.Fatalf("expected 'code1', got %q", name)
	}
}

func TestFindFreeName_IDFreeButSubdomainTaken(t *testing.T) {
	// A listener app with listener name "blog" exists under instance "blogapp".
	// Instance ID "blog" is free, but subdomain "blog" is taken.
	mgr := setupTestAppManager(t, map[string]*api.AppDefinition{
		"blogapp": {
			Extensions: map[string]interface{}{"mode": "service"},
			Services: map[string]api.AppService{
				"main": {Image: "docker.io/library/blog:latest"},
			},
			Listeners: []api.AppListener{
				{Name: "blog", GuestPort: 8080, Primary: true},
			},
		},
	})
	ctx := context.Background()

	name, err := FindFreeName(ctx, mgr, "blog")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name == "blog" {
		t.Fatal("expected FindFreeName to skip 'blog' because subdomain is taken")
	}
	if name != "blog1" {
		t.Fatalf("expected 'blog1', got %q", name)
	}
}

// --- PrepareSmartDefaults workspace injection ---

func TestPrepareSmartDefaults_WorkspaceApp(t *testing.T) {
	mgr := setupTestAppManager(t, nil)
	ctx := context.Background()

	schema := &api.AppDefinition{
		Image:         "docker.io/library/code:latest",
		WorkspaceName: "code",
		Extensions:    map[string]interface{}{"mode": "workspace"},
		// No listeners, no inputs.
	}

	if err := PrepareSmartDefaults(ctx, mgr, schema, "Code Editor"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	input, ok := schema.Inputs["__app_address__"]
	if !ok {
		t.Fatal("expected __app_address__ to be injected for workspace app")
	}
	if input.Label != "Workspace Name" {
		t.Fatalf("expected label 'Workspace Name', got %q", input.Label)
	}
	if input.Default != "codeeditor" {
		t.Fatalf("expected default 'codeeditor' (sanitized from 'Code Editor'), got %q", input.Default)
	}
	if !input.Required {
		t.Fatal("expected __app_address__ to be required")
	}
	if input.Validation == nil {
		t.Fatal("expected validation to be set")
	}
}

func TestPrepareSmartDefaults_WorkspaceApp_Collision(t *testing.T) {
	mgr := setupTestAppManager(t, map[string]*api.AppDefinition{
		"code": nil, // existing workspace app named "code"
	})
	ctx := context.Background()

	schema := &api.AppDefinition{
		Image:         "docker.io/library/code:latest",
		WorkspaceName: "code",
		Extensions:    map[string]interface{}{"mode": "workspace"},
	}

	if err := PrepareSmartDefaults(ctx, mgr, schema, "code"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	input, ok := schema.Inputs["__app_address__"]
	if !ok {
		t.Fatal("expected __app_address__ to be injected")
	}
	// "code" is taken → default should be "code1"
	if input.Default != "code1" {
		t.Fatalf("expected default 'code1' (collision avoidance), got %q", input.Default)
	}
}

func TestPrepareSmartDefaults_WorkspaceWithListeners_NoInjection(t *testing.T) {
	mgr := setupTestAppManager(t, nil)
	ctx := context.Background()

	// Evolved workspace: has listeners → should NOT get workspace injection.
	schema := &api.AppDefinition{
		Image:      "docker.io/library/code:latest",
		Extensions: map[string]interface{}{"mode": "workspace"},
		Listeners: []api.AppListener{
			{Name: "__primary", GuestPort: 8080},
		},
	}

	if err := PrepareSmartDefaults(ctx, mgr, schema, "code"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	input, ok := schema.Inputs["__app_address__"]
	if !ok {
		t.Fatal("expected __app_address__ to exist (from __primary detection)")
	}
	// Should be "App Address" (listener path), NOT "Workspace Name"
	if input.Label != "App Address" {
		t.Fatalf("expected label 'App Address' for evolved workspace, got %q", input.Label)
	}
}

func TestPrepareSmartDefaults_ServiceModeNoListeners_NoInjection(t *testing.T) {
	mgr := setupTestAppManager(t, nil)
	ctx := context.Background()

	// Service-mode app with no listeners and no workspace mode → no injection.
	schema := &api.AppDefinition{
		Image:      "docker.io/library/test:latest",
		Extensions: map[string]interface{}{"mode": "service"},
	}

	if err := PrepareSmartDefaults(ctx, mgr, schema, "test"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if schema.Inputs != nil {
		if _, ok := schema.Inputs["__app_address__"]; ok {
			t.Fatal("expected no __app_address__ injection for service-mode app without listeners")
		}
	}
}

func TestPrepareSmartDefaults_WorkspaceApp_FallbackDefault(t *testing.T) {
	mgr := setupTestAppManager(t, nil)
	ctx := context.Background()

	schema := &api.AppDefinition{
		Image:         "docker.io/library/test:latest",
		WorkspaceName: "test",
		Extensions:    map[string]interface{}{"mode": "workspace"},
	}

	// Empty catalog name → fallback to "workspace"
	if err := PrepareSmartDefaults(ctx, mgr, schema, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	input := schema.Inputs["__app_address__"]
	if input.Default != "workspace" {
		t.Fatalf("expected fallback default 'workspace', got %q", input.Default)
	}
}

func TestNameVariants_TruncatesLongBase(t *testing.T) {
	// A 16-char base must not produce >16 char suffixed names.
	base := "abcdefghijklmnop" // exactly 16 chars
	variants := nameVariants(base)

	if variants[0] != base {
		t.Fatalf("expected first variant to be base %q, got %q", base, variants[0])
	}
	for i, v := range variants {
		if len(v) > 16 {
			t.Fatalf("variant[%d] = %q (%d chars) exceeds 16-char limit", i, v, len(v))
		}
	}
	// Verify suffix is preserved: variant[1] should end with "1"
	if v := variants[1]; v != "abcdefghijklmno1" {
		t.Fatalf("expected 'abcdefghijklmno1', got %q", v)
	}
	// Double-digit suffix: variant[10] should end with "10"
	if v := variants[10]; v != "abcdefghijklmn10" {
		t.Fatalf("expected 'abcdefghijklmn10', got %q", v)
	}
}
