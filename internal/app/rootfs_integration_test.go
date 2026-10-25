package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/persistence"
	"piccolod/internal/state/paths"
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

func TestImagePullProgressDoesNotRegress(t *testing.T) {
	reporter := &recordingArtifactProgressReporter{}
	manager := &AppManager{}
	manager.SetProgressReporter(reporter)
	ctx := WithTaskID(context.Background(), "install-provider")
	manager.emitProgress(
		ctx,
		taskTypeInstallApp,
		"provider",
		taskPhaseAllocatingPorts,
		20,
		"Allocated ports",
		false,
		nil,
	)

	callback := manager.makeImagePullProgressCallback(
		ctx,
		"provider",
		"inference",
		"registry.example/provider:latest",
		transferProgressRange{Min: 15, Max: 55},
	)
	for _, report := range []container.ImagePullReport{
		{OverallPercent: -1, Phase: "pulling"},
		{OverallPercent: 60, Phase: "pulling", DownloadedBytes: 60, TotalBytes: 100},
		{OverallPercent: 20, Phase: "pulling", DownloadedBytes: 20, TotalBytes: 100},
		{OverallPercent: 100, Phase: "complete", DownloadedBytes: 100, TotalBytes: 100},
	} {
		callback(report)
	}

	var progress []int
	var downloaded []int64
	for _, event := range reporter.snapshot() {
		if event.Phase == taskPhasePullingImage {
			progress = append(progress, event.Progress)
			if current, ok := event.Metadata["downloaded_bytes"].(int64); ok {
				downloaded = append(downloaded, current)
			}
		}
	}
	want := []int{20, 41, 41, 55}
	if !reflect.DeepEqual(progress, want) {
		t.Fatalf("image lifecycle progress = %v, want monotonic %v", progress, want)
	}
	if wantBytes := []int64{0, 60, 60, 100}; !reflect.DeepEqual(downloaded, wantBytes) {
		t.Fatalf("image byte detail = %v, want monotonic %v", downloaded, wantBytes)
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

func TestDestroyAllServiceRootfsDestroysAnchorWithoutServices(t *testing.T) {
	const instanceID = "anchorleak"
	anchor := persistence.ServiceRootfsVolumeID(instanceID, networkAnchorServiceName)
	versionedAnchor := persistence.VersionedServiceRootfsVolumeID(instanceID, networkAnchorServiceName, "0123456789ab")
	service := persistence.ServiceRootfsVolumeID(instanceID, "main")
	versionedService := persistence.VersionedServiceRootfsVolumeID(instanceID, "main", "0123456789ab")
	otherAnchor := persistence.ServiceRootfsVolumeID("otherapp", networkAnchorServiceName)

	tests := []struct {
		name   string
		mode   PiccoloMode
		appDef *api.AppDefinition
		want   []string
	}{
		{name: "definition without services", mode: ModeService, appDef: &api.AppDefinition{}, want: []string{anchor, versionedAnchor}},
		// cleanupInstallResources' workspace pass has no definition.
		{name: "nil definition", mode: ModeWorkspace, want: []string{anchor, versionedAnchor}},
		{
			name:   "definition with services",
			mode:   ModeService,
			appDef: &api.AppDefinition{Services: map[string]api.AppService{"main": {Image: "example/app:1"}}},
			want:   []string{anchor, versionedAnchor, service, versionedService},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			core := t.TempDir()
			paths.SetCoreRootForTest(t, core)
			for _, volumeID := range []string{anchor, versionedAnchor, service, versionedService, otherAnchor} {
				if err := os.MkdirAll(paths.VolumeMetaDir(volumeID), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			rootfs := newStubRootfsManager(core)
			mgr := &AppManager{}
			mgr.SetRootfsManager(rootfs)

			mgr.destroyAllServiceRootfs(context.Background(), instanceID, tc.mode, tc.appDef)

			for _, volumeID := range tc.want {
				if !slices.Contains(rootfs.destroyed, volumeID) {
					t.Errorf("destroyed rootfs = %v, want %s", rootfs.destroyed, volumeID)
				}
			}
			if slices.Contains(rootfs.destroyed, otherAnchor) {
				t.Errorf("destroyed rootfs = %v, includes another app's %s", rootfs.destroyed, otherAnchor)
			}
		})
	}
}

// A failed call detaches only the volumes it attached itself. A volume it
// found attached may back running containers: the anchor and siblings of a
// service that fails to attach while they run.
func TestEnsureAllServiceRootfsAttachedRollsBackOnlyItsOwnAttaches(t *testing.T) {
	const instanceID = "site"
	anchor := persistence.ServiceRootfsVolumeID(instanceID, networkAnchorServiceName)
	web := persistence.ServiceRootfsVolumeID(instanceID, "web")
	worker := persistence.ServiceRootfsVolumeID(instanceID, "worker")
	def := &api.AppDefinition{Services: map[string]api.AppService{
		"web":    {Image: "registry.example/web:1"},
		"worker": {Image: "registry.example/worker:1"},
	}}

	tests := []struct {
		name            string
		alreadyAttached []string
		configFails     bool // the failing service attaches, but its golden image config is unreadable
		wantErr         string
	}{
		{name: "attach fails beside running siblings", alreadyAttached: []string{anchor, web, worker}, wantErr: "attach rootfs for service"},
		{name: "image config unreadable beside running siblings", alreadyAttached: []string{anchor, web, worker}, configFails: true, wantErr: "read image config for service"},
		{name: "image config unreadable after this call attached the services", alreadyAttached: []string{anchor}, configFails: true, wantErr: "read image config for service"},
		{name: "attach fails on a cold start", wantErr: "attach rootfs for service"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rootfs := newStubRootfsManager(t.TempDir())
			rootfs.alreadyAttached = map[string]bool{}
			for _, volumeID := range tc.alreadyAttached {
				rootfs.alreadyAttached[volumeID] = true
			}
			if tc.configFails {
				rootfs.attachGoldenLVs = map[string]string{web: "golden-web", worker: "golden-worker"}
			}
			// The anchor attaches first, then the services in map order. Fail
			// whichever service comes second, so the rollback always covers the
			// anchor and a sibling.
			attaches := 0
			rootfs.attachHook = func() {
				attaches++
				switch {
				case !tc.configFails && attaches == 2:
					rootfs.attachErrs = map[string]error{web: persistence.ErrIDMapStaleMount, worker: persistence.ErrIDMapStaleMount}
				case tc.configFails && attaches == 3:
					rootfs.goldenConfigs = map[string]persistence.GoldenImageConfig{} // the reads from here on fail
				}
			}
			mgr := &AppManager{subIDRangeResolver: liveTestSubIDRanges}
			mgr.SetRootfsManager(rootfs)

			attached, err := mgr.ensureAllServiceRootfsAttached(context.Background(), instanceID, ModeService, def, nil, liveTestRuntime())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %s", err, tc.wantErr)
			}
			if !tc.configFails && !errors.Is(err, persistence.ErrIDMapStaleMount) {
				t.Fatalf("error = %v, want it to wrap the attach failure", err)
			}
			if attached != nil {
				t.Fatalf("attached = %v, want nil on failure", attached)
			}

			wantAttaches := 2 // the anchor and the first service
			if tc.configFails {
				wantAttaches = 3 // and the service whose config is unreadable
			}
			if len(rootfs.attachIDMaps) != wantAttaches {
				t.Fatalf("attached volumes = %v, want %d", rootfs.attachIDMaps, wantAttaches)
			}
			var want []string
			for volumeID := range rootfs.attachIDMaps {
				if !rootfs.alreadyAttached[volumeID] {
					want = append(want, volumeID)
				}
			}
			slices.Sort(want)
			got := slices.Sorted(slices.Values(rootfs.detached))
			if !slices.Equal(got, want) {
				t.Fatalf("detached %v, want only the volumes this call attached %v", got, want)
			}
		})
	}
}
