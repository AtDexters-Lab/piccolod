package workspacedisk

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkspaceMeta_Validate(t *testing.T) {
	tests := []struct {
		name    string
		meta    WorkspaceMeta
		wantErr bool
	}{
		{
			name: "valid meta",
			meta: WorkspaceMeta{
				FormatVersion:   MetaFormatVersion,
				BaseImageDigest: "docker.io/library/ubuntu@sha256:abc123",
				BaseImageRef:    "ubuntu:22.04",
				ImageConfig: ImageConfig{
					Cmd: []string{"/bin/bash"},
				},
				CreatedAt: time.Now(),
			},
			wantErr: false,
		},
		{
			name: "missing format version",
			meta: WorkspaceMeta{
				BaseImageDigest: "docker.io/library/ubuntu@sha256:abc123",
				CreatedAt:       time.Now(),
			},
			wantErr: true,
		},
		{
			name: "invalid format version",
			meta: WorkspaceMeta{
				FormatVersion:   "99",
				BaseImageDigest: "docker.io/library/ubuntu@sha256:abc123",
				CreatedAt:       time.Now(),
			},
			wantErr: true,
		},
		{
			name: "missing base image digest",
			meta: WorkspaceMeta{
				FormatVersion: MetaFormatVersion,
				CreatedAt:     time.Now(),
			},
			wantErr: true,
		},
		{
			name: "missing created_at",
			meta: WorkspaceMeta{
				FormatVersion:   MetaFormatVersion,
				BaseImageDigest: "docker.io/library/ubuntu@sha256:abc123",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.meta.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestImageConfig_BuildOriginalCommand(t *testing.T) {
	tests := []struct {
		name   string
		config ImageConfig
		want   []string
	}{
		{
			name: "entrypoint and cmd",
			config: ImageConfig{
				Entrypoint: []string{"/usr/bin/python"},
				Cmd:        []string{"app.py"},
			},
			want: []string{"/usr/bin/python", "app.py"},
		},
		{
			name: "only entrypoint",
			config: ImageConfig{
				Entrypoint: []string{"/bin/sh", "-c", "echo hello"},
			},
			want: []string{"/bin/sh", "-c", "echo hello"},
		},
		{
			name: "only cmd",
			config: ImageConfig{
				Cmd: []string{"/bin/bash"},
			},
			want: []string{"/bin/bash"},
		},
		{
			name:   "empty config - fallback",
			config: ImageConfig{},
			want:   []string{"/bin/sh"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.config.BuildOriginalCommand()
			if len(got) != len(tt.want) {
				t.Errorf("BuildOriginalCommand() = %v, want %v", got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("BuildOriginalCommand()[%d] = %v, want %v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestSaveAndLoadMeta(t *testing.T) {
	tmpDir := t.TempDir()

	original := &WorkspaceMeta{
		FormatVersion:   MetaFormatVersion,
		BaseImageDigest: "docker.io/library/alpine@sha256:test123",
		BaseImageRef:    "alpine:latest",
		ImageConfig: ImageConfig{
			Entrypoint: []string{"/bin/sh"},
			Cmd:        []string{"-c", "echo hello"},
			Env:        []string{"PATH=/bin", "HOME=/root"},
			WorkingDir: "/app",
			User:       "nobody",
		},
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}

	// Save
	if err := SaveMeta(tmpDir, original); err != nil {
		t.Fatalf("SaveMeta() error = %v", err)
	}

	// Verify file exists
	metaPath := filepath.Join(tmpDir, MetaFilename)
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("meta.json not created: %v", err)
	}

	// Load
	loaded, err := LoadMeta(tmpDir)
	if err != nil {
		t.Fatalf("LoadMeta() error = %v", err)
	}

	// Compare
	if loaded.FormatVersion != original.FormatVersion {
		t.Errorf("FormatVersion = %v, want %v", loaded.FormatVersion, original.FormatVersion)
	}
	if loaded.BaseImageDigest != original.BaseImageDigest {
		t.Errorf("BaseImageDigest = %v, want %v", loaded.BaseImageDigest, original.BaseImageDigest)
	}
	if loaded.BaseImageRef != original.BaseImageRef {
		t.Errorf("BaseImageRef = %v, want %v", loaded.BaseImageRef, original.BaseImageRef)
	}
	if len(loaded.ImageConfig.Entrypoint) != len(original.ImageConfig.Entrypoint) {
		t.Errorf("ImageConfig.Entrypoint = %v, want %v", loaded.ImageConfig.Entrypoint, original.ImageConfig.Entrypoint)
	}
	if loaded.ImageConfig.WorkingDir != original.ImageConfig.WorkingDir {
		t.Errorf("ImageConfig.WorkingDir = %v, want %v", loaded.ImageConfig.WorkingDir, original.ImageConfig.WorkingDir)
	}
	if !loaded.CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", loaded.CreatedAt, original.CreatedAt)
	}
}

func TestMetaExists(t *testing.T) {
	tmpDir := t.TempDir()

	// Should not exist initially
	if MetaExists(tmpDir) {
		t.Error("MetaExists() = true, want false")
	}

	// Create meta file
	meta := &WorkspaceMeta{
		FormatVersion:   MetaFormatVersion,
		BaseImageDigest: "test:latest",
		CreatedAt:       time.Now(),
	}
	if err := SaveMeta(tmpDir, meta); err != nil {
		t.Fatalf("SaveMeta() error = %v", err)
	}

	// Should exist now
	if !MetaExists(tmpDir) {
		t.Error("MetaExists() = false, want true")
	}
}

func TestLoadMeta_NotFound(t *testing.T) {
	tmpDir := t.TempDir()

	_, err := LoadMeta(tmpDir)
	if err == nil {
		t.Error("LoadMeta() error = nil, want error")
	}
}

func TestSaveMeta_InvalidMeta(t *testing.T) {
	tmpDir := t.TempDir()

	// Invalid meta (missing required fields)
	meta := &WorkspaceMeta{}

	err := SaveMeta(tmpDir, meta)
	if err == nil {
		t.Error("SaveMeta() error = nil, want error")
	}
}
