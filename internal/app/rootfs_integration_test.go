package app

import (
	"bytes"
	"context"
	"reflect"
	"slices"
	"syscall"
	"testing"

	"piccolod/internal/container"
	"piccolod/internal/persistence"
)

func TestReadImageConfigForGoldenRootfsFallsBackToLegacyRepoQualifiedGolden(t *testing.T) {
	rawDigest := "docker.io/example/piclu@sha256:legacy"
	canonicalDigest := "sha256:legacy"
	canonicalGolden := "golden-" + persistence.ShortDigest(canonicalDigest)
	legacyGolden := "golden-" + persistence.ShortDigest(rawDigest)
	rootfs := newStubRootfsManager(t.TempDir())
	rootfs.goldenConfigs = map[string]persistence.GoldenImageConfig{
		legacyGolden: {Cmd: []string{"/legacy"}},
	}

	cfg, err := (&AppManager{}).readImageConfigForGoldenRootfs(context.Background(), rootfs, "", rawDigest)
	if err != nil {
		t.Fatalf("read image config: %v", err)
	}
	if got := cfg.Cmd; !slices.Equal(got, []string{"/legacy"}) {
		t.Fatalf("cmd = %v, want legacy config", got)
	}
	if want := []string{canonicalGolden, legacyGolden}; !slices.Equal(rootfs.goldenReads, want) {
		t.Fatalf("golden reads = %v, want %v", rootfs.goldenReads, want)
	}
}

func TestReadImageConfigForGoldenRootfsPrefersRecordedGoldenIdentity(t *testing.T) {
	digest := "sha256:collision"
	derivedGolden := "golden-" + persistence.ShortDigest(digest)
	recordedGolden := derivedGolden + "-abcdef"
	rootfs := newStubRootfsManager(t.TempDir())
	rootfs.goldenConfigs = map[string]persistence.GoldenImageConfig{
		recordedGolden: {Cmd: []string{"/recorded"}},
		derivedGolden:  {Cmd: []string{"/wrong-collision"}},
	}

	cfg, err := (&AppManager{}).readImageConfigForGoldenRootfs(
		context.Background(),
		rootfs,
		recordedGolden,
		digest,
	)
	if err != nil {
		t.Fatalf("read image config: %v", err)
	}
	if got := cfg.Cmd; !slices.Equal(got, []string{"/recorded"}) {
		t.Fatalf("cmd = %v, want recorded golden config", got)
	}
	if want := []string{recordedGolden}; !slices.Equal(rootfs.goldenReads, want) {
		t.Fatalf("golden reads = %v, want %v", rootfs.goldenReads, want)
	}
}

func TestNewRootfsExportCommandAppliesRootlessWrapperAndPreservesPipe(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rt := container.PodmanRuntime{
		Credential: &syscall.Credential{Uid: 475, Gid: 475},
		HomeDir:    "/var/lib/piccolo/apps/namek/home",
	}

	cmd := newRootfsExportCommand(
		context.Background(),
		rt,
		[]string{"--root", "/apps/namek/podman", "export", "container-id"},
		stdout,
		stderr,
	)

	wantArgs := []string{
		"/usr/bin/choom", "-n", "0", "--", "/usr/bin/podman",
		"--root", "/apps/namek/podman", "export", "container-id",
	}
	if cmd.Path != "/usr/bin/choom" || !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("rootfs export command = path %q args %v, want %q %v", cmd.Path, cmd.Args, "/usr/bin/choom", wantArgs)
	}
	if cmd.Stdout != stdout || cmd.Stderr != stderr {
		t.Fatal("rootfs export pipe or stderr sink was not preserved")
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential != rt.Credential {
		t.Fatal("rootfs export command did not receive the runtime credential")
	}
}
