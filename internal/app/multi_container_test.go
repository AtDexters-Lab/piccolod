package app

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"gopkg.in/yaml.v3"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/persistence"
	"piccolod/internal/state/paths"
)

func TestWriteAppConfig(t *testing.T) {
	t.Run("writes_yaml_when_config_present", func(t *testing.T) {
		dir := t.TempDir()
		config := map[string]interface{}{
			"feature_flags": map[string]interface{}{
				"demo_mode": true,
			},
		}

		if err := writeAppConfig(dir, config, os.Getuid(), os.Getgid()); err != nil {
			t.Fatalf("writeAppConfig: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(dir, "app.yaml"))
		if err != nil {
			t.Fatalf("read app.yaml: %v", err)
		}

		var got map[string]interface{}
		if err := yaml.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal app.yaml: %v", err)
		}
		flags, ok := got["feature_flags"].(map[string]interface{})
		if !ok || flags["demo_mode"] != true {
			t.Errorf("unexpected structure: %v", got)
		}
	})

	t.Run("removes_file_when_config_nil", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "app.yaml")

		if err := os.WriteFile(path, []byte("old: config"), 0o644); err != nil {
			t.Fatalf("setup: %v", err)
		}

		if err := writeAppConfig(dir, nil, os.Getuid(), os.Getgid()); err != nil {
			t.Fatalf("writeAppConfig: %v", err)
		}

		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("expected app.yaml to be removed")
		}
	})

	t.Run("nil_config_noop_when_no_file", func(t *testing.T) {
		dir := t.TempDir()

		if err := writeAppConfig(dir, nil, os.Getuid(), os.Getgid()); err != nil {
			t.Fatalf("writeAppConfig: %v", err)
		}
	})

}

func TestArtifactMountConflictUsesFinalGeneratedMountPlan(t *testing.T) {
	spec := container.ContainerCreateSpec{
		Volumes: []container.VolumeMapping{
			{Container: "/piccolo/config"},
			{Container: "/piccolo/boot.sh"},
			{Container: "/usr/local/bin/piccolo-startup"},
		},
		Tmpfs: []container.TmpfsMount{{Container: "/run"}},
		CAMounts: []container.CAMount{{
			ContainerPath: "/etc/ssl/certs/piccolo-internal-ca.crt",
		}},
		Devices: []string{
			"/dev/dri/renderD128",
			"/dev/accel/accel0",
		},
		UseInit: true,
	}
	for _, target := range []string{
		"/piccolo/config",
		"/piccolo/config/models",
		"/piccolo",
		"/piccolo/boot.sh",
		"/usr/local/bin",
		"/run/models",
		"/etc/ssl/certs",
		"/dev/dri",
		"/dev/dri/renderD128",
		"/dev/dri/renderD128/models",
		"/dev/accel",
		"/run/podman-init",
	} {
		if owner, overlaps := artifactMountConflict(spec, target); !overlaps {
			t.Fatalf("artifact target %s did not conflict; owner=%q", target, owner)
		}
	}
	if owner, overlaps := artifactMountConflict(spec, "/models/model"); overlaps {
		t.Fatalf("independent artifact target conflicts with %s", owner)
	}

	readOnly := container.ContainerCreateSpec{ReadOnly: true}
	for _, target := range []string{"/tmp/model", "/run/model", "/var/tmp/model"} {
		if _, overlaps := artifactMountConflict(readOnly, target); !overlaps {
			t.Fatalf("read-only implicit tmpfs target %s did not conflict", target)
		}
	}
	if _, overlaps := artifactMountConflict(
		container.ContainerCreateSpec{UseInit: true},
		"/run/podman-init",
	); !overlaps {
		t.Fatal("Podman init target did not independently conflict")
	}
}

func TestBuildServiceContainerSpec(t *testing.T) {
	// Common setup: point asset paths to a temp dir with dummy boot.sh/piccolo-startup.
	stateDir := t.TempDir()
	paths.SetCoreRootForTest(t, stateDir)
	assetsDir := filepath.Join(stateDir, "assets")
	_ = os.MkdirAll(assetsDir, 0o755)
	_ = os.WriteFile(filepath.Join(assetsDir, "boot.sh"), []byte("#!/bin/sh\n"), 0o755)
	_ = os.WriteFile(filepath.Join(assetsDir, "piccolo-startup"), []byte("#!/bin/sh\n"), 0o755)

	mgr := &AppManager{}
	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())
	cred := &syscall.Credential{Uid: uid, Gid: gid}

	makeOpts := func(mode PiccoloMode, init string) serviceContainerOptions {
		extensions := map[string]interface{}{
			"mode": string(mode),
		}
		if mode == ModeUnknown {
			extensions = nil
		}
		dataDir := t.TempDir()
		return serviceContainerOptions{
			layout:     appVolumeLayout{DataDir: dataDir},
			instanceID: "test-app",
			primary:    "main",
			svcName:    "main",
			anchorID:   "anchor-123",
			credential: cred,
			appDef: &api.AppDefinition{
				Services: map[string]api.AppService{
					"main": {Image: "postgres:16", Init: init},
				},
				Extensions: extensions,
			},
			rootfsHandle:    &persistence.RootfsHandle{MountPath: dataDir, ReadOnly: true},
			goldenImgConfig: &persistence.GoldenImageConfig{Entrypoint: []string{"/entrypoint.sh"}, Cmd: []string{"postgres"}},
		}
	}

	hasVolume := func(vols []container.VolumeMapping, containerPath string) bool {
		for _, v := range vols {
			if v.Container == containerPath {
				return true
			}
		}
		return false
	}

	t.Run("service_mode_uses_native_entrypoint", func(t *testing.T) {
		spec, err := mgr.buildServiceContainerSpec(makeOpts(ModeService, ""))
		if err != nil {
			t.Fatalf("buildServiceContainerSpec: %v", err)
		}

		if len(spec.Entrypoint) != 1 || spec.Entrypoint[0] != "/entrypoint.sh" {
			t.Errorf("expected native entrypoint [/entrypoint.sh], got %v", spec.Entrypoint)
		}
		if len(spec.Command) != 1 || spec.Command[0] != "postgres" {
			t.Errorf("expected native cmd [postgres], got %v", spec.Command)
		}
		if !spec.UseInit {
			t.Error("expected UseInit=true for service mode")
		}
		if hasVolume(spec.Volumes, "/piccolo/boot.sh") {
			t.Error("service mode should not mount boot.sh")
		}
		if !hasVolume(spec.Volumes, "/piccolo/config") {
			t.Error("service mode should mount /piccolo/config")
		}
	})

	t.Run("workspace_mode_uses_boot_sh_wrapper", func(t *testing.T) {
		spec, err := mgr.buildServiceContainerSpec(makeOpts(ModeWorkspace, ""))
		if err != nil {
			t.Fatalf("buildServiceContainerSpec: %v", err)
		}

		if len(spec.Entrypoint) != 2 || spec.Entrypoint[0] != "/bin/sh" || spec.Entrypoint[1] != "/piccolo/boot.sh" {
			t.Errorf("expected boot.sh entrypoint, got %v", spec.Entrypoint)
		}
		// Command should be the combined original entrypoint+cmd.
		if len(spec.Command) != 2 || spec.Command[0] != "/entrypoint.sh" || spec.Command[1] != "postgres" {
			t.Errorf("expected original cmd as command, got %v", spec.Command)
		}
		if !spec.UseInit {
			t.Error("expected UseInit=true for workspace mode")
		}
		if !hasVolume(spec.Volumes, "/piccolo/boot.sh") {
			t.Error("workspace mode should mount boot.sh")
		}
		if !hasVolume(spec.Volumes, "/usr/local/bin/piccolo-startup") {
			t.Error("workspace mode should mount piccolo-startup")
		}
	})

	t.Run("workspace_init_image_uses_native_entrypoint", func(t *testing.T) {
		spec, err := mgr.buildServiceContainerSpec(makeOpts(ModeWorkspace, "image"))
		if err != nil {
			t.Fatalf("buildServiceContainerSpec: %v", err)
		}

		if len(spec.Entrypoint) != 1 || spec.Entrypoint[0] != "/entrypoint.sh" {
			t.Errorf("expected native entrypoint, got %v", spec.Entrypoint)
		}
		if spec.UseInit {
			t.Error("expected UseInit=false for init:image")
		}
		if hasVolume(spec.Volumes, "/piccolo/boot.sh") {
			t.Error("init:image should not mount boot.sh")
		}
	})

	t.Run("unknown_mode_falls_through_to_service_behavior", func(t *testing.T) {
		spec, err := mgr.buildServiceContainerSpec(makeOpts(ModeUnknown, ""))
		if err != nil {
			t.Fatalf("buildServiceContainerSpec: %v", err)
		}

		if len(spec.Entrypoint) != 1 || spec.Entrypoint[0] != "/entrypoint.sh" {
			t.Errorf("expected native entrypoint, got %v", spec.Entrypoint)
		}
		if !spec.UseInit {
			t.Error("expected UseInit=true for unknown mode")
		}
		if hasVolume(spec.Volumes, "/piccolo/boot.sh") {
			t.Error("unknown mode should not mount boot.sh")
		}
	})
}
