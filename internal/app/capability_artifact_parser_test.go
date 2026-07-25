package app

import (
	"strings"
	"testing"

	"piccolod/internal/api"
)

func fullCapabilityArtifactManifest() string {
	return `
artifacts:
  model:
    source:
      type: huggingface
      repository: OpenVINO/Qwen3-0.6B-int4-ov
      revision: main
      path: .
services:
  main:
    image: provider:latest
    bind_ports: [8000]
    environment:
      EXISTING: value
    storage:
      persistent:
        data:
          container: /var/lib/data
      artifacts:
        model:
          container: /models/model
    consumes:
      - capability: ai.inference.openai.v1
        env:
          OPENAI_BASE_URL: base_url
listeners:
  - name: __primary
    guest_port: 8000
    flow: tcp
    protocol: http
    provides:
      - capability: ai.inference.openai.v1
        base_path: /v3
x-piccolo:
  mode: service
  requires_features:
    - capability_bindings_v1
    - artifact_bindings_v1
`
}

func TestParseCapabilityAndArtifactManifest(t *testing.T) {
	def, err := ParseAppDefinition([]byte(fullCapabilityArtifactManifest()))
	if err != nil {
		t.Fatalf("ParseAppDefinition() error = %v", err)
	}

	if len(def.Listeners) != 1 || len(def.Listeners[0].Provides) != 1 {
		t.Fatalf("provider declaration missing: %+v", def.Listeners)
	}
	if got := def.Listeners[0].Provides[0].BasePath; got != "/v3" {
		t.Fatalf("provider base_path = %q, want /v3", got)
	}
	service := def.Services["main"]
	if len(service.Consumes) != 1 {
		t.Fatalf("consumer declaration missing: %+v", service.Consumes)
	}
	if got := service.Consumes[0].Env["OPENAI_BASE_URL"]; got != api.CapabilityBindingBaseURL {
		t.Fatalf("consumer property = %q, want %q", got, api.CapabilityBindingBaseURL)
	}
	if got := def.Artifacts["model"].Source.Repository; got != "OpenVINO/Qwen3-0.6B-int4-ov" {
		t.Fatalf("artifact repository = %q", got)
	}
	if got := service.Storage.Artifacts["model"].Container; got != "/models/model" {
		t.Fatalf("artifact mount = %q", got)
	}

	schema, err := ParseAppSchema([]byte(fullCapabilityArtifactManifest()))
	if err != nil {
		t.Fatalf("ParseAppSchema() error = %v", err)
	}
	if len(schema.Artifacts) != 1 || len(schema.Services["main"].Consumes) != 1 {
		t.Fatalf("schema parse lost accepted fields: %+v", schema)
	}
}

func TestCapabilityFeatureGateAndValidation(t *testing.T) {
	base := fullCapabilityArtifactManifest()
	tests := []struct {
		name    string
		replace string
		with    string
		wantErr string
	}{
		{
			name:    "missing capability gate",
			replace: "    - capability_bindings_v1\n",
			wantErr: "capability_bindings_v1",
		},
		{
			name:    "unknown provider field",
			replace: "        base_path: /v3",
			with:    "        base_path: /v3\n        adapter: openai",
			wantErr: "unsupported field \"adapter\"",
		},
		{
			name:    "unknown consumer field",
			replace: "        env:\n          OPENAI_BASE_URL: base_url",
			with:    "        env:\n          OPENAI_BASE_URL: base_url\n        optional: true",
			wantErr: "unsupported field \"optional\"",
		},
		{
			name:    "provider transport mismatch",
			replace: "    protocol: http",
			with:    "    protocol: websocket",
			wantErr: "flow: tcp and protocol: http",
		},
		{
			name:    "duplicate provider",
			replace: "    provides:\n      - capability: ai.inference.openai.v1\n        base_path: /v3",
			with: "    provides:\n" +
				"      - capability: ai.inference.openai.v1\n" +
				"        base_path: /v3\n" +
				"      - capability: ai.inference.openai.v1\n" +
				"        base_path: /v4",
			wantErr: "provided by both listeners",
		},
		{
			name:    "encoded separator in base path",
			replace: "base_path: /v3",
			with:    "base_path: /v3%2Fadmin",
			wantErr: "encoded separators",
		},
		{
			name:    "nested encoded separator in base path",
			replace: "base_path: /v3",
			with:    "base_path: /v3%252Fadmin",
			wantErr: "residual percent escaping",
		},
		{
			name:    "multiply encoded dot segment in base path",
			replace: "base_path: /v3",
			with:    "base_path: /v3/%25252e%25252e/admin",
			wantErr: "residual percent escaping",
		},
		{
			name:    "literal percent in base path",
			replace: "base_path: /v3",
			with:    "base_path: /v3/%25literal",
			wantErr: "residual percent escaping",
		},
		{
			name:    "noncanonical base path",
			replace: "base_path: /v3",
			with:    "base_path: /v3/",
			wantErr: "trailing slash",
		},
		{
			name:    "unknown binding property",
			replace: "OPENAI_BASE_URL: base_url",
			with:    "OPENAI_BASE_URL: endpoint",
			wantErr: "property \"endpoint\" is not registered",
		},
		{
			name:    "explicit environment collision",
			replace: "OPENAI_BASE_URL: base_url",
			with:    "EXISTING: base_url",
			wantErr: "collides with services.main.environment",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := strings.Replace(base, test.replace, test.with, 1)
			_, err := ParseAppDefinition([]byte(manifest))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ParseAppDefinition() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestArtifactFeatureGateAndValidation(t *testing.T) {
	base := fullCapabilityArtifactManifest()
	tests := []struct {
		name    string
		replace string
		with    string
		wantErr string
	}{
		{
			name:    "missing artifact gate",
			replace: "    - artifact_bindings_v1\n",
			wantErr: "artifact_bindings_v1",
		},
		{
			name:    "unknown artifact field",
			replace: "  model:\n    source:",
			with:    "  model:\n    writable: true\n    source:",
			wantErr: "unsupported field \"writable\"",
		},
		{
			name:    "unknown source field",
			replace: "      path: .",
			with:    "      path: .\n      url: https://huggingface.co/model",
			wantErr: "unsupported field \"url\"",
		},
		{
			name:    "unknown mount field",
			replace: "          container: /models/model",
			with:    "          container: /models/model\n          read_only: true",
			wantErr: "unsupported field \"read_only\"",
		},
		{
			name:    "undeclared mount",
			replace: "        model:\n          container: /models/model",
			with:    "        other:\n          container: /models/model",
			wantErr: "references undeclared artifact",
		},
		{
			name:    "unmounted declaration",
			replace: "      artifacts:\n        model:\n          container: /models/model\n",
			wantErr: "must be mounted by at least one service",
		},
		{
			name:    "artifact root target",
			replace: "container: /models/model",
			with:    "container: /",
			wantErr: "absolute path other than /",
		},
		{
			name:    "artifact overlaps persistent mount",
			replace: "container: /models/model",
			with:    "container: /var/lib",
			wantErr: "overlaps persistent.data",
		},
		{
			name:    "provider artifact overlaps DRM device family",
			replace: "container: /models/model",
			with:    "container: /dev/dri/renderD128",
			wantErr: "overlaps accelerator device family /dev/dri",
		},
		{
			name:    "provider artifact overlaps accel device family",
			replace: "container: /models/model",
			with:    "container: /dev/accel",
			wantErr: "overlaps accelerator device family /dev/accel",
		},
		{
			name:    "invalid hugging face path",
			replace: "      path: .",
			with:    "      path: ../model",
			wantErr: "dot path segments",
		},
		{
			name:    "unsafe hugging face revision",
			replace: "      revision: main",
			with:    "      revision: ../../other",
			wantErr: "revision invalid",
		},
		{
			name:    "noncanonical digest",
			replace: "      path: .",
			with:    "      path: model.gguf\n      digest: SHA256:1234",
			wantErr: "64 lowercase hexadecimal",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := strings.Replace(base, test.replace, test.with, 1)
			_, err := ParseAppDefinition([]byte(manifest))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ParseAppDefinition() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestProviderArtifactMayUseDeviceFamilySiblingPath(t *testing.T) {
	manifest := strings.Replace(
		fullCapabilityArtifactManifest(),
		"container: /models/model",
		"container: /dev/driver-model",
		1,
	)
	if _, err := ParseAppDefinition([]byte(manifest)); err != nil {
		t.Fatalf("ParseAppDefinition() rejected non-overlapping device sibling: %v", err)
	}
}

func TestParseOCIArtifactSource(t *testing.T) {
	manifest := strings.Replace(
		fullCapabilityArtifactManifest(),
		"type: huggingface\n      repository: OpenVINO/Qwen3-0.6B-int4-ov\n      revision: main\n      path: .",
		"type: oci\n      reference: localhost:5000/example/model:latest\n      digest: sha256:"+strings.Repeat("a", 64),
		1,
	)
	def, err := ParseAppDefinition([]byte(manifest))
	if err != nil {
		t.Fatalf("ParseAppDefinition() error = %v", err)
	}
	source := def.Artifacts["model"].Source
	if source.Type != "oci" || source.Reference != "localhost:5000/example/model:latest" {
		t.Fatalf("OCI source = %+v", source)
	}
}

func TestOCIArtifactReferenceGrammar(t *testing.T) {
	tests := []struct {
		name      string
		reference string
		wantErr   bool
	}{
		{
			name:      "uppercase tag is valid",
			reference: "ghcr.io/acme/model:Q4_K_M",
		},
		{
			name:      "registry port is valid",
			reference: "localhost:5000/example/model:latest",
		},
		{
			name:      "double path separator is invalid",
			reference: "ghcr.io/acme//model:latest",
			wantErr:   true,
		},
		{
			name:      "multiple digest delimiters are invalid",
			reference: "ghcr.io/acme/model@sha256:abc@sha256:def",
			wantErr:   true,
		},
		{
			name:      "URL scheme is invalid",
			reference: "https://ghcr.io/acme/model:latest",
			wantErr:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOCIReference(tc.reference)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateOCIReference(%q) error = %v, wantErr %t", tc.reference, err, tc.wantErr)
			}
		})
	}
}

func TestArtifactSourceChangeIsStructural(t *testing.T) {
	oldDef := capabilityConsumerDefinition("OPENAI_BASE_URL")
	oldDef.Artifacts = map[string]api.AppArtifact{
		"model": {
			Source: api.ArtifactSource{
				Type:       "huggingface",
				Repository: "example/model",
				Revision:   "main",
				Path:       "model.gguf",
			},
		},
	}
	newDef := *oldDef
	newDef.Artifacts = map[string]api.AppArtifact{
		"model": {
			Source: api.ArtifactSource{
				Type:       "huggingface",
				Repository: "example/model",
				Revision:   "next",
				Path:       "model.gguf",
			},
		},
	}

	if got := classifyDiff(oldDef, &newDef); got != DiffKindStructuralWithImage {
		t.Fatalf("artifact source change classified as %s, want structural_with_image", got)
	}
}
