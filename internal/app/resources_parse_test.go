package app

import (
	"strings"
	"testing"

	"piccolod/internal/api"
)

// TestParseResources_NewShapeAccepted verifies the new-shape resources
// block is accepted and round-trips through parse + validate.
func TestParseResources_NewShapeAccepted(t *testing.T) {
	yamlSrc := `type: user
listeners:
  - name: __primary
    guest_port: 80
services:
  main:
    image: nginx:alpine
    bind_ports: [80]
resources:
  priority: normal
  memory:
    min_required: 4GB
    profile: elastic
  storage:
    max: 500GiB
x-piccolo:
  mode: service
`
	app, err := ParseAppDefinition([]byte(yamlSrc))
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if app.Resources == nil {
		t.Fatal("Resources is nil")
	}
	if app.Resources.Priority != api.PriorityNormal {
		t.Errorf("Priority = %q, want normal", app.Resources.Priority)
	}
	if app.Resources.Memory == nil || app.Resources.Memory.MinRequired != "4GB" {
		t.Errorf("Memory.MinRequired = %+v, want 4GB", app.Resources.Memory)
	}
	if app.Resources.Memory.Profile != api.ProfileElastic {
		t.Errorf("Memory.Profile = %q, want elastic", app.Resources.Memory.Profile)
	}
	if app.Resources.Storage == nil || app.Resources.Storage.Max != "500GiB" {
		t.Errorf("Storage.Max = %+v, want 500GiB", app.Resources.Storage)
	}
}

// TestParseResources_LegacyLimitsRejected verifies the pre-v2 shape at
// the app top level (resources.limits) is rejected with the catalog-sync
// hint.
func TestParseResources_LegacyLimitsRejected(t *testing.T) {
	yamlSrc := `type: user
listeners:
  - name: __primary
    guest_port: 80
services:
  main:
    image: nginx:alpine
    bind_ports: [80]
resources:
  limits:
    memory: 2GB
    cpu: 2.0
x-piccolo:
  mode: service
`
	_, err := ParseAppDefinition([]byte(yamlSrc))
	if err == nil {
		t.Fatal("expected parse error for legacy resources.limits shape")
	}
	if !strings.Contains(err.Error(), "pre-v2") || !strings.Contains(err.Error(), "catalog sync") {
		t.Errorf("error message should mention pre-v2 schema and catalog sync, got: %v", err)
	}
}

// TestParseResources_LegacyPerServiceRejected verifies per-service
// resources blocks are rejected with the catalog-sync hint.
func TestParseResources_LegacyPerServiceRejected(t *testing.T) {
	yamlSrc := `type: user
listeners:
  - name: __primary
    guest_port: 80
services:
  main:
    image: nginx:alpine
    bind_ports: [80]
    resources:
      limits:
        memory: 2GB
x-piccolo:
  mode: service
`
	_, err := ParseAppDefinition([]byte(yamlSrc))
	if err == nil {
		t.Fatal("expected parse error for legacy per-service resources shape")
	}
	if !strings.Contains(err.Error(), "pre-v2") || !strings.Contains(err.Error(), "catalog sync") {
		t.Errorf("error message should mention pre-v2 schema and catalog sync, got: %v", err)
	}
}

// TestParseResources_MemoryRequiresMinRequired verifies that when a
// memory block is declared, min_required is required.
func TestParseResources_MemoryRequiresMinRequired(t *testing.T) {
	yamlSrc := `type: user
listeners:
  - name: __primary
    guest_port: 80
services:
  main:
    image: nginx:alpine
    bind_ports: [80]
resources:
  memory:
    profile: elastic
x-piccolo:
  mode: service
`
	_, err := ParseAppDefinition([]byte(yamlSrc))
	if err == nil {
		t.Fatal("expected parse error for memory block without min_required")
	}
	if !strings.Contains(err.Error(), "min_required") {
		t.Errorf("error should mention min_required; got: %v", err)
	}
}

// TestParseResources_PriorityValidation checks accepted and rejected
// priority values.
func TestParseResources_PriorityValidation(t *testing.T) {
	cases := []struct {
		priority  string
		wantErr   bool
		errSubstr string
	}{
		{"high", false, ""},
		{"normal", false, ""},
		{"background", false, ""},
		{"medium", true, "priority must be one of"},
		{"HIGH", true, "priority must be one of"}, // case-sensitive
	}
	for _, c := range cases {
		yamlSrc := `type: user
listeners:
  - name: __primary
    guest_port: 80
services:
  main:
    image: nginx:alpine
    bind_ports: [80]
resources:
  priority: ` + c.priority + `
x-piccolo:
  mode: service
`
		_, err := ParseAppDefinition([]byte(yamlSrc))
		if c.wantErr {
			if err == nil {
				t.Errorf("priority=%q: expected error, got nil", c.priority)
				continue
			}
			if !strings.Contains(err.Error(), c.errSubstr) {
				t.Errorf("priority=%q: error should contain %q, got: %v", c.priority, c.errSubstr, err)
			}
		} else {
			if err != nil {
				t.Errorf("priority=%q: unexpected error: %v", c.priority, err)
			}
		}
	}
}

// TestParseResources_ProfileValidation checks accepted and rejected
// memory profile values.
func TestParseResources_ProfileValidation(t *testing.T) {
	cases := []struct {
		profile string
		wantErr bool
	}{
		{"bounded", false},
		{"elastic", false},
		{"flexible", true},
	}
	for _, c := range cases {
		yamlSrc := `type: user
listeners:
  - name: __primary
    guest_port: 80
services:
  main:
    image: nginx:alpine
    bind_ports: [80]
resources:
  memory:
    min_required: 1GB
    profile: ` + c.profile + `
x-piccolo:
  mode: service
`
		_, err := ParseAppDefinition([]byte(yamlSrc))
		if c.wantErr && err == nil {
			t.Errorf("profile=%q: expected error", c.profile)
		}
		if !c.wantErr && err != nil {
			t.Errorf("profile=%q: unexpected error: %v", c.profile, err)
		}
	}
}

// TestParseAppSchema_LegacyRejection mirrors the ParseAppDefinition
// coverage on the loose-parse path used by the install pipeline.
// Regressions here would let legacy-shape catalog manifests silently
// slip past the install-pipeline pre-render parse.
func TestParseAppSchema_LegacyRejection(t *testing.T) {
	legacyTopLevel := `type: user
listeners:
  - name: __primary
    guest_port: 80
services:
  main:
    image: nginx:alpine
    bind_ports: [80]
resources:
  limits:
    memory: 2GB
x-piccolo:
  mode: service
`
	if _, err := ParseAppSchema([]byte(legacyTopLevel)); err == nil {
		t.Error("ParseAppSchema should reject legacy resources.limits")
	} else if !strings.Contains(err.Error(), "catalog sync") {
		t.Errorf("ParseAppSchema error should mention catalog sync; got: %v", err)
	}

	legacyPerService := `type: user
listeners:
  - name: __primary
    guest_port: 80
services:
  main:
    image: nginx:alpine
    bind_ports: [80]
    resources:
      limits:
        memory: 2GB
x-piccolo:
  mode: service
`
	if _, err := ParseAppSchema([]byte(legacyPerService)); err == nil {
		t.Error("ParseAppSchema should reject legacy per-service resources")
	} else if !strings.Contains(err.Error(), "catalog sync") {
		t.Errorf("ParseAppSchema error should mention catalog sync; got: %v", err)
	}

	// New shape passes loose parse cleanly.
	newShape := `type: user
listeners:
  - name: __primary
    guest_port: 80
services:
  main:
    image: nginx:alpine
    bind_ports: [80]
resources:
  priority: normal
  memory:
    min_required: 1GB
    profile: bounded
x-piccolo:
  mode: service
`
	if _, err := ParseAppSchema([]byte(newShape)); err != nil {
		t.Errorf("ParseAppSchema should accept new shape: %v", err)
	}
}

// TestParseAppDefinition_YAMLAliasSmuggling exercises the catch-all
// LegacyLimits field that guards against pre-v2 shapes smuggled via
// YAML alias/merge-key constructs that the raw-YAML MappingNode walk
// might not catch. The struct-decode path is the belt-and-suspenders
// for validateRawResourcesShape.
func TestParseAppDefinition_YAMLAliasSmuggling(t *testing.T) {
	aliased := `type: user
listeners:
  - name: __primary
    guest_port: 80
services:
  main:
    image: nginx:alpine
    bind_ports: [80]
x-shared-limits: &l
  memory: 2GB
  cpu: 2.0
resources:
  limits: *l
x-piccolo:
  mode: service
`
	if _, err := ParseAppDefinition([]byte(aliased)); err == nil {
		t.Error("aliased legacy limits must be rejected")
	} else if !strings.Contains(err.Error(), "catalog sync") {
		t.Errorf("error should mention catalog sync; got: %v", err)
	}
}

// TestParseResources_LifecycleReservedNamespace verifies the lifecycle
// block is accepted (reserved for future use) without fields being
// consumed.
func TestParseResources_LifecycleReservedNamespace(t *testing.T) {
	yamlSrc := `type: user
listeners:
  - name: __primary
    guest_port: 80
services:
  main:
    image: nginx:alpine
    bind_ports: [80]
resources:
  priority: normal
lifecycle: {}
x-piccolo:
  mode: service
`
	app, err := ParseAppDefinition([]byte(yamlSrc))
	if err != nil {
		t.Fatalf("lifecycle block should be accepted: %v", err)
	}
	if app.Lifecycle == nil {
		t.Error("Lifecycle should be non-nil (declared in manifest)")
	}
}
