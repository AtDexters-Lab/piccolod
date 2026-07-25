package container

import (
	"slices"
	"testing"
)

func TestPodmanClassifiedNonImageArtifact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "podman explicit artifact classification",
			output: "unsupported image-specific operation on artifact with type application/example",
			want:   true,
		},
		{
			name:   "empty OCI config media type",
			output: "unsupported media type application/vnd.oci.empty.v1+json",
			want:   true,
		},
		{
			name:   "authentication failure does not fall back",
			output: "unable to retrieve auth token: invalid username/password",
		},
		{
			name:   "generic manifest failure does not fall back",
			output: "manifest unknown",
		},
		{
			name:   "capacity failure does not fall back",
			output: "no space left on device",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := podmanClassifiedNonImageArtifact(tt.output); got != tt.want {
				t.Fatalf("podmanClassifiedNonImageArtifact(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

func TestBuildCreateArgsMapsAcceleratorDevices(t *testing.T) {
	t.Parallel()
	args := buildCreateArgs(ContainerCreateSpec{
		Image:   "provider:latest",
		Devices: []string{"/dev/dri/renderD128", "/dev/accel/accel0"},
	})
	for _, sequence := range [][]string{
		{"--device", "/dev/accel/accel0"},
		{"--device", "/dev/dri/renderD128"},
	} {
		found := false
		for index := 0; index+len(sequence) <= len(args); index++ {
			if slices.Equal(args[index:index+len(sequence)], sequence) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("create args %v do not contain %v", args, sequence)
		}
	}
}

func TestPodmanArtifactExtractArgsUsesNativeWholeArtifactForm(t *testing.T) {
	t.Parallel()
	got := podmanArtifactExtractArgs("registry.example/model:latest", "/staging/model")
	want := []string{"artifact", "extract", "registry.example/model:latest", "/staging/model"}
	if !slices.Equal(got, want) {
		t.Fatalf("extract args = %v, want %v", got, want)
	}
}
