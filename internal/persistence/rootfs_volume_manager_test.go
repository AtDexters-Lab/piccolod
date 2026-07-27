package persistence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"piccolod/internal/crypt"
	"piccolod/internal/state/paths"
	"piccolod/internal/storage/lvm"
	"piccolod/internal/testutil"
)

type goldenRecreateOrderRunner struct {
	metaPath          string
	goldenID          string
	physicalExists    bool
	removeFailures    int
	removeFailure     error
	createFailure     error
	publicationEvents []string
}

type goldenTeardownRunner struct {
	calls             []string
	umountFailures    int
	cryptsetupFailure error
	deactivateFailure error
}

type goldenEnsureRunner struct {
	calls      []string
	goldenID   string
	physical   bool
	udevadmErr error
	umountErr  error
}

type goldenDestroyRunner struct {
	calls         []string
	goldenID      string
	physical      bool
	mounted       bool
	mountReadOnly bool
	mapperOpen    bool
	umountErr     error
	removeErr     error
}

type goldenGCContinueRunner struct {
	calls            []string
	physical         map[string]bool
	failFirstRemoval bool
	removalFailure   error
	removalCallCount int
}

func (r *goldenGCContinueRunner) Run(_ context.Context, name string, args ...string) error {
	r.calls = append(r.calls, testutil.BuildKey(name, args))
	if name != "lvremove" {
		return nil
	}
	r.removalCallCount++
	lvPath := args[len(args)-1]
	goldenID := strings.TrimPrefix(lvPath, lvm.DefaultVGName+"/")
	if r.failFirstRemoval {
		r.failFirstRemoval = false
		return r.removalFailure
	}
	r.physical[goldenID] = false
	return nil
}

func (r *goldenGCContinueRunner) RunWithOutput(
	_ context.Context,
	name string,
	args ...string,
) ([]byte, error) {
	if name != "lvs" {
		return nil, nil
	}
	var names []string
	for goldenID, exists := range r.physical {
		if exists {
			names = append(names, goldenID)
		}
	}
	sort.Strings(names)
	return []byte(strings.Join(names, "\n") + "\n"), nil
}

func (r *goldenGCContinueRunner) RunWithStdin(
	ctx context.Context,
	_ []byte,
	name string,
	args ...string,
) error {
	return r.Run(ctx, name, args...)
}

func (r *goldenDestroyRunner) Run(_ context.Context, name string, args ...string) error {
	r.calls = append(r.calls, testutil.BuildKey(name, args))
	switch name {
	case "lvs":
		requestedLV := ""
		if len(args) > 0 && strings.HasPrefix(args[len(args)-1], lvm.DefaultVGName+"/") {
			requestedLV = strings.TrimPrefix(args[len(args)-1], lvm.DefaultVGName+"/")
		}
		if !r.physical || (requestedLV != "" && requestedLV != r.goldenID) {
			return errors.New("LV not found")
		}
	case "umount":
		if r.umountErr != nil {
			return r.umountErr
		}
		r.mounted = false
	case "cryptsetup":
		r.mapperOpen = false
	case "lvremove":
		if r.removeErr != nil {
			return r.removeErr
		}
		r.physical = false
	}
	return nil
}

func (r *goldenDestroyRunner) RunWithOutput(
	_ context.Context,
	name string,
	args ...string,
) ([]byte, error) {
	if name == "lvs" && r.physical {
		if strings.Join(args, " ") == "--noheadings -o lv_name "+lvm.DefaultVGName {
			return []byte(r.goldenID + "\n"), nil
		}
		return []byte(fmt.Sprintf(
			"%s 1073741824 Vwi-a-tz-- %s\n",
			r.goldenID,
			lvm.DefaultThinPoolName,
		)), nil
	}
	return nil, nil
}

func (r *goldenDestroyRunner) RunWithStdin(
	ctx context.Context,
	_ []byte,
	name string,
	args ...string,
) error {
	return r.Run(ctx, name, args...)
}

func (r *goldenDestroyRunner) snapshot(_ []string) (kernelSnapshot, error) {
	snapshot := kernelSnapshot{
		mounts:   make(map[string]mountEntry),
		dmByName: make(map[string]string),
		dmByDev:  make(map[string]string),
	}
	mapper := volMapperName(r.goldenID)
	if r.mapperOpen {
		snapshot.dmByName[mapper] = "/sys/block/dm-10"
		snapshot.dmByDev["253:10"] = mapper
	}
	if r.mounted {
		snapshot.mounts[filepath.Clean(paths.MountDir(r.goldenID))] = mountEntry{
			Major:    253,
			Minor:    10,
			FSType:   "btrfs",
			Source:   "/dev/mapper/" + mapper,
			ReadOnly: r.mountReadOnly,
		}
	}
	return snapshot, nil
}

func (r *goldenEnsureRunner) Run(_ context.Context, name string, args ...string) error {
	r.calls = append(r.calls, testutil.BuildKey(name, args))
	switch name {
	case "lvs":
		if !r.physical {
			return errors.New("LV not found")
		}
	case "lvcreate":
		for index := 0; index+1 < len(args); index++ {
			if args[index] == "--name" {
				r.goldenID = args[index+1]
				break
			}
		}
		r.physical = true
	case "lvremove":
		r.physical = false
	case "udevadm":
		return r.udevadmErr
	case "umount":
		return r.umountErr
	}
	return nil
}

func (r *goldenEnsureRunner) RunWithOutput(
	_ context.Context,
	name string,
	args ...string,
) ([]byte, error) {
	if name == "lvs" && r.physical {
		if strings.Join(args, " ") == "--noheadings -o lv_name "+lvm.DefaultVGName {
			return []byte(r.goldenID + "\n"), nil
		}
		return []byte(r.goldenID + " 1073741824 Vwi-a-tz-- " + lvm.DefaultThinPoolName + "\n"), nil
	}
	return nil, nil
}

func (r *goldenEnsureRunner) RunWithStdin(
	ctx context.Context,
	_ []byte,
	name string,
	args ...string,
) error {
	return r.Run(ctx, name, args...)
}

func (r *goldenTeardownRunner) Run(_ context.Context, name string, args ...string) error {
	key := testutil.BuildKey(name, args)
	r.calls = append(r.calls, key)
	switch name {
	case "umount":
		if r.umountFailures > 0 {
			r.umountFailures--
			return errors.New("target is busy")
		}
	case "cryptsetup":
		return r.cryptsetupFailure
	case "lvchange":
		return r.deactivateFailure
	}
	return nil
}

func (r *goldenTeardownRunner) RunWithOutput(
	_ context.Context,
	_ string,
	_ ...string,
) ([]byte, error) {
	return nil, nil
}

func (r *goldenTeardownRunner) RunWithStdin(
	ctx context.Context,
	_ []byte,
	name string,
	args ...string,
) error {
	return r.Run(ctx, name, args...)
}

func (r *goldenRecreateOrderRunner) recordPublicationEvent(operation string) {
	meta, err := readVolumeMetaV3(r.metaPath)
	if err != nil {
		r.publicationEvents = append(r.publicationEvents, operation+":metadata-unavailable")
		return
	}
	r.publicationEvents = append(
		r.publicationEvents,
		fmt.Sprintf("%s:%d", operation, meta.SizeBytes),
	)
}

func (r *goldenRecreateOrderRunner) Run(_ context.Context, name string, args ...string) error {
	switch name {
	case "lvs":
		if r.physicalExists {
			return nil
		}
		return errors.New("LV not found")
	case "lvremove":
		r.recordPublicationEvent("remove")
		if r.removeFailures > 0 {
			r.removeFailures--
			return r.removeFailure
		}
		r.physicalExists = false
		return nil
	case "lvcreate":
		r.recordPublicationEvent("create")
		return r.createFailure
	default:
		return nil
	}
}

func (r *goldenRecreateOrderRunner) RunWithOutput(
	_ context.Context,
	name string,
	args ...string,
) ([]byte, error) {
	if name == "lvs" && r.physicalExists {
		if strings.Join(args, " ") == "--noheadings -o lv_name "+lvm.DefaultVGName {
			return []byte(r.goldenID + "\n"), nil
		}
		return []byte(fmt.Sprintf(
			"%s 1073741824 Vwi-a-tz-- %s\n",
			r.goldenID,
			lvm.DefaultThinPoolName,
		)), nil
	}
	return nil, nil
}

func (r *goldenRecreateOrderRunner) RunWithStdin(
	ctx context.Context,
	_ []byte,
	name string,
	args ...string,
) error {
	return r.Run(ctx, name, args...)
}

func TestServiceRootfsVolumeID(t *testing.T) {
	tests := []struct {
		name        string
		instanceID  string
		serviceName string
		want        string
	}{
		{
			name:        "empty_service_name_returns_legacy",
			instanceID:  "myapp-123",
			serviceName: "",
			want:        "svc-rootfs-myapp-123",
		},
		{
			name:        "single_service",
			instanceID:  "myapp-123",
			serviceName: "web",
			want:        "svc-rootfs-myapp-123--web",
		},
		{
			name:        "postgres_service",
			instanceID:  "nextcloud-456",
			serviceName: "postgres",
			want:        "svc-rootfs-nextcloud-456--postgres",
		},
		{
			name:        "service_with_hyphens",
			instanceID:  "app-1",
			serviceName: "my-service",
			want:        "svc-rootfs-app-1--my-service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ServiceRootfsVolumeID(tt.instanceID, tt.serviceName)
			if got != tt.want {
				t.Errorf("ServiceRootfsVolumeID(%q, %q) = %q, want %q",
					tt.instanceID, tt.serviceName, got, tt.want)
			}
		})
	}
}

func TestVersionedServiceRootfsVolumeID(t *testing.T) {
	tests := []struct {
		name        string
		instanceID  string
		serviceName string
		shortDigest string
		want        string
	}{
		{
			name:        "single_service_with_digest",
			instanceID:  "myapp",
			serviceName: "web",
			shortDigest: "a1b2c3d4e5f6",
			want:        "svc-rootfs-myapp--web--a1b2c3d4e5f6",
		},
		{
			name:        "legacy_service_empty_name",
			instanceID:  "myapp",
			serviceName: "",
			shortDigest: "abc123",
			want:        "svc-rootfs-myapp--abc123",
		},
		{
			name:        "long_instance_and_service",
			instanceID:  "my-home-assistant-app",
			serviceName: "core-service",
			shortDigest: "deadbeef1234",
			want:        "svc-rootfs-my-home-assistant-app--core-service--deadbeef1234",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := VersionedServiceRootfsVolumeID(tt.instanceID, tt.serviceName, tt.shortDigest)
			if got != tt.want {
				t.Errorf("VersionedServiceRootfsVolumeID(%q, %q, %q) = %q, want %q",
					tt.instanceID, tt.serviceName, tt.shortDigest, got, tt.want)
			}
		})
	}
}

func TestGoldenLVSizeForImage(t *testing.T) {
	tests := []struct {
		name           string
		imageSizeBytes int64
		want           int64
	}{
		{
			name:           "zero_falls_back_to_default",
			imageSizeBytes: 0,
			want:           defaultGoldenLVSize,
		},
		{
			name:           "negative_falls_back_to_default",
			imageSizeBytes: -100,
			want:           defaultGoldenLVSize,
		},
		{
			name:           "small_image_plus_1GiB_wins",
			imageSizeBytes: 50 << 20, // 50 MiB
			// 1.5x = 75 MiB, +1 GiB = ~1074 MiB → +1 GiB wins
			want: 50<<20 + 1<<30,
		},
		{
			name:           "medium_image_plus_1GiB_wins",
			imageSizeBytes: 500 << 20, // 500 MiB
			// 1.5x = 750 MiB, +1 GiB = 1524 MiB → +1 GiB wins
			want: 500<<20 + 1<<30,
		},
		{
			name:           "large_image_1.5x_wins",
			imageSizeBytes: 4 << 30, // 4 GiB
			// 1.5x = 6 GiB, +1 GiB = 5 GiB → 1.5x wins
			want: 4<<30 + 4<<30/2,
		},
		{
			name:           "boundary_at_2GiB",
			imageSizeBytes: 2 << 30, // 2 GiB
			// 1.5x = 3 GiB, +1 GiB = 3 GiB → equal, sizeA is used (both 3 GiB)
			want: 2<<30 + 2<<30/2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := goldenLVSizeForImage(tt.imageSizeBytes)
			if got != tt.want {
				t.Errorf("goldenLVSizeForImage(%d) = %d, want %d", tt.imageSizeBytes, got, tt.want)
			}
		})
	}
}

func TestEnsureGoldenLVRecreatePublishesOnlyAfterVerifiedStaleTeardown(t *testing.T) {
	root := t.TempDir()
	paths.SetCoreRootForTest(t, root)

	digest := "sha256:" + strings.Repeat("a", 64)
	identity := GoldenContentIdentity{
		SourceKind:       GoldenSourceOCI,
		ResolvedIdentity: digest,
		Projection:       GoldenProjectionOCIImageRootfs,
	}
	candidates, err := candidateGoldenIDs(identity, digest)
	if err != nil {
		t.Fatalf("candidateGoldenIDs: %v", err)
	}
	goldenID := candidates[0]
	metaPath := filepath.Join(paths.VolumeMetaDir(goldenID), metadataV2File)
	const staleSize = int64(12345)
	if err := os.MkdirAll(paths.VolumeMetaDir(goldenID), 0o700); err != nil {
		t.Fatalf("create stale golden metadata directory: %v", err)
	}
	if err := writeVolumeMetaV3(metaPath, &volumeMetaV3{
		Version:        metadataV3Version,
		Type:           volumeTypeGolden,
		LVName:         goldenID,
		VGName:         lvm.DefaultVGName,
		SizeBytes:      staleSize,
		GoldenIdentity: &identity,
	}); err != nil {
		t.Fatalf("write stale golden metadata: %v", err)
	}

	removeFailure := errors.New("injected stale LV removal failure")
	createFailure := errors.New("injected replacement LV creation failure")
	run := &goldenRecreateOrderRunner{
		metaPath:       metaPath,
		goldenID:       goldenID,
		physicalExists: true,
		removeFailures: 1,
		removeFailure:  removeFailure,
		createFailure:  createFailure,
	}
	manager, err := NewLUKSVolumeManager(LUKSVolumeManagerConfig{
		Run:   run,
		LVMgr: lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
		FlattenFn: func(context.Context, string, string, string) (GoldenImageConfig, error) {
			return GoldenImageConfig{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewLUKSVolumeManager: %v", err)
	}
	req := GoldenLVRequest{
		ImageDigest:   digest,
		ImageRef:      "registry.example/provider:latest",
		ImageSizeHint: 64 << 20,
	}

	if _, err := manager.EnsureGoldenLV(context.Background(), req); !errors.Is(err, removeFailure) {
		t.Fatalf("first EnsureGoldenLV error = %v, want stale removal failure", err)
	}
	staleMeta, err := readVolumeMetaV3(metaPath)
	if err != nil {
		t.Fatalf("read retained stale metadata: %v", err)
	}
	if staleMeta.SizeBytes != staleSize {
		t.Fatalf("stale marker was replaced before teardown: size=%d want=%d", staleMeta.SizeBytes, staleSize)
	}

	if _, err := manager.EnsureGoldenLV(context.Background(), req); !errors.Is(err, createFailure) {
		t.Fatalf("retry EnsureGoldenLV error = %v, want replacement creation failure", err)
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Fatalf("failed creation retained metadata without a physical LV: %v", err)
	}

	retrySize := goldenLVSizeForImage(req.ImageSizeHint)
	wantEvents := []string{
		fmt.Sprintf("remove:%d", staleSize),
		fmt.Sprintf("remove:%d", staleSize),
		fmt.Sprintf("create:%d", retrySize),
	}
	if len(run.publicationEvents) != len(wantEvents) {
		t.Fatalf("publication events = %v, want %v", run.publicationEvents, wantEvents)
	}
	for index := range wantEvents {
		if run.publicationEvents[index] != wantEvents[index] {
			t.Fatalf("publication events = %v, want %v", run.publicationEvents, wantEvents)
		}
	}
}

func TestTeardownGoldenStagingRetriesInOrder(t *testing.T) {
	run := &goldenTeardownRunner{umountFailures: 2}
	manager := &luksVolumeManager{
		run:   run,
		lvMgr: lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
	}
	mounted, luksOpened, lvActive := true, true, true

	err := manager.teardownGoldenStaging(
		context.Background(),
		"/mount/golden",
		"golden-mapper",
		"golden-content",
		&mounted,
		&luksOpened,
		&lvActive,
		0,
	)
	if err != nil {
		t.Fatalf("teardownGoldenStaging: %v", err)
	}
	if mounted || luksOpened || lvActive {
		t.Fatalf(
			"staging state mounted=%v luksOpened=%v lvActive=%v, want all false",
			mounted,
			luksOpened,
			lvActive,
		)
	}
	want := []string{
		"umount /mount/golden",
		"umount /mount/golden",
		"umount /mount/golden",
		"cryptsetup close golden-mapper",
		"lvchange -an " + lvm.DefaultVGName + "/golden-content",
	}
	if len(run.calls) != len(want) {
		t.Fatalf("teardown calls = %v, want %v", run.calls, want)
	}
	for index := range want {
		if run.calls[index] != want[index] {
			t.Fatalf("teardown calls = %v, want %v", run.calls, want)
		}
	}
}

func TestTeardownGoldenStagingDoesNotDescendPastBusyMount(t *testing.T) {
	run := &goldenTeardownRunner{umountFailures: goldenStagingTeardownAttempts}
	manager := &luksVolumeManager{
		run:   run,
		lvMgr: lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
	}
	mounted, luksOpened, lvActive := true, true, true

	err := manager.teardownGoldenStaging(
		context.Background(),
		"/mount/golden",
		"golden-mapper",
		"golden-content",
		&mounted,
		&luksOpened,
		&lvActive,
		0,
	)
	if err == nil || !strings.Contains(err.Error(), "target is busy") {
		t.Fatalf("teardown error = %v, want busy mount failure", err)
	}
	if !mounted || !luksOpened || !lvActive {
		t.Fatalf(
			"failed teardown state mounted=%v luksOpened=%v lvActive=%v, want all retained",
			mounted,
			luksOpened,
			lvActive,
		)
	}
	if len(run.calls) != goldenStagingTeardownAttempts {
		t.Fatalf(
			"teardown calls = %v, want %d unmount attempts",
			run.calls,
			goldenStagingTeardownAttempts,
		)
	}
	for _, call := range run.calls {
		if call != "umount /mount/golden" {
			t.Fatalf("teardown descended below busy mount: %v", run.calls)
		}
	}
}

func TestEnsureGoldenLVDeactivatesAfterUdevSettleFailure(t *testing.T) {
	root := t.TempDir()
	paths.SetCoreRootForTest(t, root)
	run := &goldenEnsureRunner{udevadmErr: errors.New("udev settle failed")}
	manager, err := NewLUKSVolumeManager(LUKSVolumeManagerConfig{
		Run:    run,
		Crypto: newResizeTestCryptManager(t, root),
		LVMgr:  lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
	})
	if err != nil {
		t.Fatalf("NewLUKSVolumeManager: %v", err)
	}
	manager.tmpfsDir = t.TempDir()

	_, err = manager.EnsureGoldenLV(context.Background(), GoldenLVRequest{
		ImageDigest:   "sha256:" + strings.Repeat("b", 64),
		ImageRef:      "registry.example/model:latest",
		ImageSizeHint: 1 << 20,
		Materialize: func(context.Context, string) (GoldenMaterializationResult, error) {
			return GoldenMaterializationResult{}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "udev settle failed") {
		t.Fatalf("EnsureGoldenLV error = %v, want udev settle failure", err)
	}
	requireCallContaining(t, run.calls, "lvchange -an "+lvm.DefaultVGName+"/golden-")
	requireCallContaining(t, run.calls, "lvremove -f "+lvm.DefaultVGName+"/golden-")
}

func TestEnsureGoldenLVRetainsIncompleteLVWhenUnmountStaysBusy(t *testing.T) {
	root := t.TempDir()
	paths.SetCoreRootForTest(t, root)
	run := &goldenEnsureRunner{umountErr: errors.New("target is busy")}
	manager, err := NewLUKSVolumeManager(LUKSVolumeManagerConfig{
		Run:    run,
		Crypto: newResizeTestCryptManager(t, root),
		LVMgr:  lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
	})
	if err != nil {
		t.Fatalf("NewLUKSVolumeManager: %v", err)
	}
	manager.tmpfsDir = t.TempDir()

	digest := "sha256:" + strings.Repeat("c", 64)
	_, err = manager.EnsureGoldenLV(context.Background(), GoldenLVRequest{
		ImageDigest:   digest,
		ImageRef:      "registry.example/model:latest",
		ImageSizeHint: 1 << 20,
		Materialize: func(_ context.Context, targetDir string) (GoldenMaterializationResult, error) {
			return GoldenMaterializationResult{}, os.WriteFile(
				filepath.Join(targetDir, "model.bin"),
				[]byte("model"),
				0o644,
			)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "target is busy") {
		t.Fatalf("EnsureGoldenLV error = %v, want busy mount failure", err)
	}
	for _, call := range run.calls {
		if strings.HasPrefix(call, "lvremove ") {
			t.Fatalf("busy staging stack triggered destructive LV removal: %v", run.calls)
		}
	}
	if !run.physical {
		t.Fatal("incomplete golden LV was removed despite busy staging stack")
	}
	goldenID := goldenLVPrefix + ShortDigest(digest)
	meta, err := readVolumeMetaV3(filepath.Join(paths.VolumeMetaDir(goldenID), metadataV2File))
	if err != nil {
		t.Fatalf("read retained incomplete metadata: %v", err)
	}
	if goldenReadyTimestamp(meta) != "" {
		t.Fatalf("busy golden metadata was published Ready: %+v", meta)
	}
}

func TestReconcileRetainsGoldenEvidenceWhenStackIsBusy(t *testing.T) {
	root := t.TempDir()
	paths.SetCoreRootForTest(t, root)
	goldenID := goldenLVPrefix + "busy-content"
	metaDir := paths.VolumeMetaDir(goldenID)
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := &volumeMetaV3{
		Version: metadataV3Version,
		Type:    volumeTypeGolden,
		LVName:  goldenID,
		VGName:  lvm.DefaultVGName,
		FSType:  "btrfs",
		GoldenIdentity: &GoldenContentIdentity{
			SourceKind:       GoldenSourceHuggingFace,
			ResolvedIdentity: strings.Repeat("d", 40),
			Projection:       GoldenProjectionHuggingFace + ":models",
		},
	}
	metaPath := filepath.Join(metaDir, metadataV2File)
	if err := writeVolumeMetaV3(metaPath, meta); err != nil {
		t.Fatal(err)
	}

	run := &goldenDestroyRunner{
		goldenID:   goldenID,
		physical:   true,
		mounted:    true,
		mapperOpen: true,
		umountErr:  errors.New("target is busy"),
	}
	manager, err := NewLUKSVolumeManager(LUKSVolumeManagerConfig{
		Run:   run,
		LVMgr: lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
	})
	if err != nil {
		t.Fatalf("NewLUKSVolumeManager: %v", err)
	}
	manager.kernelSnapshotFn = run.snapshot

	err = manager.ReconcileRootfsStates(context.Background())
	if err == nil || !strings.Contains(err.Error(), "target is busy") {
		t.Fatalf("ReconcileRootfsStates error = %v, want busy stack failure", err)
	}
	if !run.physical {
		t.Fatal("reconciliation removed the LV below a busy mount")
	}
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("reconciliation removed durable cleanup evidence: %v", err)
	}
	for _, call := range run.calls {
		if strings.HasPrefix(call, "cryptsetup ") ||
			strings.HasPrefix(call, "lvchange ") ||
			strings.HasPrefix(call, "lvremove ") {
			t.Fatalf("reconciliation descended below a busy mount: %v", run.calls)
		}
	}
}

func TestReadyGoldenReuseFailsClosedAfterReconcileSettlementFailure(t *testing.T) {
	root := t.TempDir()
	paths.SetCoreRootForTest(t, root)
	identity := GoldenContentIdentity{
		SourceKind:       GoldenSourceHuggingFace,
		ResolvedIdentity: strings.Repeat("9", 40),
		Projection:       GoldenProjectionHuggingFace + ":models",
	}
	candidates, err := candidateGoldenIDs(identity, "")
	if err != nil {
		t.Fatalf("candidateGoldenIDs: %v", err)
	}
	goldenID := candidates[0]
	metaDir := paths.VolumeMetaDir(goldenID)
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(metaDir, metadataV2File)
	if err := writeVolumeMetaV3(metaPath, &volumeMetaV3{
		Version:             metadataV3Version,
		Type:                volumeTypeGolden,
		LVName:              goldenID,
		VGName:              lvm.DefaultVGName,
		FSType:              "btrfs",
		GoldenIdentity:      &identity,
		MaterializeComplete: "2026-07-25T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	referenceDir := paths.VolumeMetaDir("svc-rootfs-ready-reuse-reference")
	if err := os.MkdirAll(referenceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeVolumeMetaV3(filepath.Join(referenceDir, metadataV2File), &volumeMetaV3{
		Version:  metadataV3Version,
		Type:     volumeTypeServiceRootfs,
		LVName:   "svc-rootfs-ready-reuse-reference",
		VGName:   lvm.DefaultVGName,
		FSType:   "btrfs",
		GoldenLV: goldenID,
	}); err != nil {
		t.Fatal(err)
	}

	run := &goldenDestroyRunner{
		goldenID:   goldenID,
		physical:   true,
		mounted:    true,
		mapperOpen: true,
		umountErr:  errors.New("target is busy"),
	}
	manager, err := NewLUKSVolumeManager(LUKSVolumeManagerConfig{
		Run:   run,
		LVMgr: lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
	})
	if err != nil {
		t.Fatalf("NewLUKSVolumeManager: %v", err)
	}
	manager.kernelSnapshotFn = run.snapshot

	if err := manager.ReconcileRootfsStates(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "target is busy") {
		t.Fatalf("ReconcileRootfsStates error = %v, want busy settlement failure", err)
	}
	materialized := false
	_, err = manager.EnsureGoldenContent(context.Background(), GoldenContentRequest{
		Identity:          identity,
		SourceRef:         "huggingface.example/models",
		PreferredGoldenID: goldenID,
		Materialize: func(context.Context, string) (GoldenMaterializationResult, error) {
			materialized = true
			return GoldenMaterializationResult{}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "target is busy") {
		t.Fatalf("EnsureGoldenContent error = %v, want busy settlement failure", err)
	}
	if materialized {
		t.Fatal("Ready reuse unexpectedly rematerialized content")
	}
	if !run.physical {
		t.Fatal("Ready reuse removed content after settlement failure")
	}
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("Ready reuse removed durable metadata: %v", err)
	}
	manager.mu.Lock()
	cached := manager.goldenLVs[goldenID]
	manager.mu.Unlock()
	if cached != nil {
		t.Fatal("unsettled Ready content was cached after reuse failure")
	}
}

func TestReconcileSettlesLegacyReadyGoldenBeforeCaching(t *testing.T) {
	root := t.TempDir()
	paths.SetCoreRootForTest(t, root)
	goldenID := goldenLVPrefix + "legacy-ready"
	identity := GoldenContentIdentity{
		SourceKind:       GoldenSourceHuggingFace,
		ResolvedIdentity: strings.Repeat("e", 40),
		Projection:       GoldenProjectionHuggingFace + ":models",
	}
	metaDir := paths.VolumeMetaDir(goldenID)
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(metaDir, metadataV2File)
	if err := writeVolumeMetaV3(metaPath, &volumeMetaV3{
		Version:             metadataV3Version,
		Type:                volumeTypeGolden,
		LVName:              goldenID,
		VGName:              lvm.DefaultVGName,
		FSType:              "btrfs",
		GoldenIdentity:      &identity,
		MaterializeComplete: "2026-07-25T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	referenceDir := paths.VolumeMetaDir("svc-rootfs-reference")
	if err := os.MkdirAll(referenceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeVolumeMetaV3(filepath.Join(referenceDir, metadataV2File), &volumeMetaV3{
		Version:  metadataV3Version,
		Type:     volumeTypeServiceRootfs,
		LVName:   "svc-rootfs-reference",
		VGName:   lvm.DefaultVGName,
		FSType:   "btrfs",
		GoldenLV: goldenID,
	}); err != nil {
		t.Fatal(err)
	}

	run := &goldenDestroyRunner{
		goldenID:   goldenID,
		physical:   true,
		mounted:    true,
		mapperOpen: true,
	}
	manager, err := NewLUKSVolumeManager(LUKSVolumeManagerConfig{
		Run:   run,
		LVMgr: lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
	})
	if err != nil {
		t.Fatalf("NewLUKSVolumeManager: %v", err)
	}
	manager.kernelSnapshotFn = run.snapshot

	if err := manager.ReconcileRootfsStates(context.Background()); err != nil {
		t.Fatalf("ReconcileRootfsStates: %v", err)
	}
	if run.mounted || run.mapperOpen {
		t.Fatalf("legacy Ready staging stack remains live: mounted=%v mapper=%v", run.mounted, run.mapperOpen)
	}
	if !run.physical {
		t.Fatal("Ready content was deleted while settling legacy staging")
	}
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("Ready metadata was removed: %v", err)
	}
	manager.mu.Lock()
	cached := manager.goldenLVs[goldenID]
	manager.mu.Unlock()
	if cached == nil {
		t.Fatal("settled Ready golden was not cached")
	}
	requireCallContaining(t, run.calls, "umount "+paths.MountDir(goldenID))
	requireCallContaining(t, run.calls, "cryptsetup close "+volMapperName(goldenID))
	requireCallContaining(t, run.calls, "lvchange -an "+lvm.DefaultVGName+"/"+goldenID)
}

func TestReconcilePreservesReadOnlyReadyGoldenAttachment(t *testing.T) {
	root := t.TempDir()
	paths.SetCoreRootForTest(t, root)
	goldenID := goldenLVPrefix + "consumer-mounted"
	identity := GoldenContentIdentity{
		SourceKind:       GoldenSourceHuggingFace,
		ResolvedIdentity: strings.Repeat("f", 40),
		Projection:       GoldenProjectionHuggingFace + ":models",
	}
	if err := os.MkdirAll(paths.VolumeMetaDir(goldenID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeVolumeMetaV3(
		filepath.Join(paths.VolumeMetaDir(goldenID), metadataV2File),
		&volumeMetaV3{
			Version:             metadataV3Version,
			Type:                volumeTypeGolden,
			LVName:              goldenID,
			VGName:              lvm.DefaultVGName,
			FSType:              "btrfs",
			GoldenIdentity:      &identity,
			MaterializeComplete: "2026-07-25T00:00:00Z",
		},
	); err != nil {
		t.Fatal(err)
	}
	referenceDir := paths.VolumeMetaDir("svc-rootfs-consumer-reference")
	if err := os.MkdirAll(referenceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeVolumeMetaV3(filepath.Join(referenceDir, metadataV2File), &volumeMetaV3{
		Version:  metadataV3Version,
		Type:     volumeTypeServiceRootfs,
		LVName:   "svc-rootfs-consumer-reference",
		VGName:   lvm.DefaultVGName,
		FSType:   "btrfs",
		GoldenLV: goldenID,
	}); err != nil {
		t.Fatal(err)
	}

	run := &goldenDestroyRunner{
		goldenID:      goldenID,
		physical:      true,
		mounted:       true,
		mountReadOnly: true,
		mapperOpen:    true,
	}
	manager, err := NewLUKSVolumeManager(LUKSVolumeManagerConfig{
		Run:   run,
		LVMgr: lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
	})
	if err != nil {
		t.Fatalf("NewLUKSVolumeManager: %v", err)
	}
	manager.kernelSnapshotFn = run.snapshot

	if err := manager.ReconcileRootfsStates(context.Background()); err != nil {
		t.Fatalf("ReconcileRootfsStates: %v", err)
	}
	for _, call := range run.calls {
		if strings.HasPrefix(call, "umount ") ||
			strings.HasPrefix(call, "cryptsetup ") ||
			strings.HasPrefix(call, "lvchange ") ||
			strings.HasPrefix(call, "lvremove ") {
			t.Fatalf("legitimate read-only consumer attachment was disturbed: %v", run.calls)
		}
	}
}

func TestGenericDestroyAPIsRejectGoldenContent(t *testing.T) {
	root := t.TempDir()
	paths.SetCoreRootForTest(t, root)
	goldenID := goldenLVPrefix + "reference-owned"
	metaDir := paths.VolumeMetaDir(goldenID)
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(metaDir, metadataV2File)
	if err := writeVolumeMetaV3(metaPath, &volumeMetaV3{
		Version: metadataV3Version,
		Type:    volumeTypeGolden,
		LVName:  goldenID,
		VGName:  lvm.DefaultVGName,
		FSType:  "btrfs",
	}); err != nil {
		t.Fatal(err)
	}
	run := &goldenDestroyRunner{
		goldenID:   goldenID,
		physical:   true,
		mounted:    true,
		mapperOpen: true,
		umountErr:  errors.New("target is busy"),
	}
	manager, err := NewLUKSVolumeManager(LUKSVolumeManagerConfig{
		Run:   run,
		LVMgr: lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
	})
	if err != nil {
		t.Fatalf("NewLUKSVolumeManager: %v", err)
	}
	manager.kernelSnapshotFn = run.snapshot

	for _, test := range []struct {
		name string
		run  func() error
	}{
		{name: "DestroyRootfs", run: func() error {
			return manager.DestroyRootfs(context.Background(), goldenID)
		}},
		{name: "DestroyVolume", run: func() error {
			return manager.DestroyVolume(context.Background(), goldenID)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			if err == nil || !strings.Contains(err.Error(), "refuse generic destruction") {
				t.Fatalf("%s error = %v, want reference-owned refusal", test.name, err)
			}
			if !run.physical {
				t.Fatalf("%s removed physical golden content", test.name)
			}
			if _, err := os.Stat(metaPath); err != nil {
				t.Fatalf("%s removed durable golden metadata: %v", test.name, err)
			}
		})
	}
	for _, call := range run.calls {
		if strings.HasPrefix(call, "umount ") ||
			strings.HasPrefix(call, "cryptsetup ") ||
			strings.HasPrefix(call, "lvchange ") ||
			strings.HasPrefix(call, "lvremove ") {
			t.Fatalf("generic destroy disturbed golden content: %v", run.calls)
		}
	}
}

func TestGenericDestroyAPIsRejectMetadataLessGoldenNamespace(t *testing.T) {
	root := t.TempDir()
	paths.SetCoreRootForTest(t, root)
	goldenID := goldenLVPrefix + "metadata-less"
	mountDir := paths.MountDir(goldenID)
	sentinel := filepath.Join(mountDir, "payload", "sentinel")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("golden payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := &goldenDestroyRunner{
		goldenID:   goldenID,
		physical:   true,
		mounted:    true,
		mapperOpen: true,
	}
	manager, err := NewLUKSVolumeManager(LUKSVolumeManagerConfig{
		Run:   run,
		LVMgr: lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
	})
	if err != nil {
		t.Fatalf("NewLUKSVolumeManager: %v", err)
	}
	manager.kernelSnapshotFn = run.snapshot

	for _, test := range []struct {
		name string
		run  func() error
	}{
		{name: "DestroyRootfs", run: func() error {
			return manager.DestroyRootfs(context.Background(), goldenID)
		}},
		{name: "DestroyVolume", run: func() error {
			return manager.DestroyVolume(context.Background(), goldenID)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			if err == nil || !strings.Contains(err.Error(), "refuse generic destruction") {
				t.Fatalf("%s error = %v, want golden namespace refusal", test.name, err)
			}
			if !run.physical {
				t.Fatalf("%s removed physical golden content", test.name)
			}
			if _, err := os.Stat(sentinel); err != nil {
				t.Fatalf("%s traversed metadata-less golden payload: %v", test.name, err)
			}
		})
	}
	if len(run.calls) != 0 {
		t.Fatalf("generic destroy touched metadata-less golden stack: %v", run.calls)
	}
}

func TestDestroyGoldenLVRefusesNonEmptyMountDirectory(t *testing.T) {
	root := t.TempDir()
	paths.SetCoreRootForTest(t, root)
	goldenID := goldenLVPrefix + "unexpected-mount-content"
	metaDir := paths.VolumeMetaDir(goldenID)
	mountDir := paths.MountDir(goldenID)
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mountDir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(metaDir, metadataV2File)
	if err := writeVolumeMetaV3(metaPath, &volumeMetaV3{
		Version: metadataV3Version,
		Type:    volumeTypeGolden,
		LVName:  goldenID,
		VGName:  lvm.DefaultVGName,
		FSType:  "btrfs",
	}); err != nil {
		t.Fatal(err)
	}

	run := &goldenDestroyRunner{goldenID: goldenID}
	manager, err := NewLUKSVolumeManager(LUKSVolumeManagerConfig{
		Run:   run,
		LVMgr: lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
	})
	if err != nil {
		t.Fatalf("NewLUKSVolumeManager: %v", err)
	}
	manager.kernelSnapshotFn = run.snapshot

	err = manager.destroyGoldenLVLocked(context.Background(), goldenID)
	if err == nil {
		t.Fatal("destroyGoldenLVLocked removed a non-empty mount directory")
	}
	if _, err := os.Stat(filepath.Join(mountDir, "nested")); err != nil {
		t.Fatalf("unexpected mount content was traversed: %v", err)
	}
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("metadata evidence was removed after mount-directory refusal: %v", err)
	}
}

func TestGarbageCollectGoldenLVsContinuesAfterIndependentRemovalFailure(t *testing.T) {
	root := t.TempDir()
	paths.SetCoreRootForTest(t, root)
	goldenIDs := []string{goldenLVPrefix + "gc-first", goldenLVPrefix + "gc-second"}
	for index, goldenID := range goldenIDs {
		if err := os.MkdirAll(paths.VolumeMetaDir(goldenID), 0o700); err != nil {
			t.Fatal(err)
		}
		identity := GoldenContentIdentity{
			SourceKind:       GoldenSourceHuggingFace,
			ResolvedIdentity: fmt.Sprintf("%040d", index+1),
			Projection:       GoldenProjectionHuggingFace + ":models",
		}
		if err := writeVolumeMetaV3(
			filepath.Join(paths.VolumeMetaDir(goldenID), metadataV2File),
			&volumeMetaV3{
				Version:             metadataV3Version,
				Type:                volumeTypeGolden,
				LVName:              goldenID,
				VGName:              lvm.DefaultVGName,
				FSType:              "btrfs",
				GoldenIdentity:      &identity,
				MaterializeComplete: "2026-07-27T00:00:00Z",
			},
		); err != nil {
			t.Fatal(err)
		}
	}

	removalFailure := errors.New("injected first golden removal failure")
	run := &goldenGCContinueRunner{
		physical: map[string]bool{
			goldenIDs[0]: true,
			goldenIDs[1]: true,
		},
		failFirstRemoval: true,
		removalFailure:   removalFailure,
	}
	manager, err := NewLUKSVolumeManager(LUKSVolumeManagerConfig{
		Run:   run,
		LVMgr: lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
	})
	if err != nil {
		t.Fatalf("NewLUKSVolumeManager: %v", err)
	}
	manager.kernelSnapshotFn = func([]string) (kernelSnapshot, error) {
		return kernelSnapshot{
			mounts:   make(map[string]mountEntry),
			dmByName: make(map[string]string),
			dmByDev:  make(map[string]string),
		}, nil
	}

	err = manager.GarbageCollectGoldenLVs(context.Background())
	if !errors.Is(err, removalFailure) {
		t.Fatalf("GarbageCollectGoldenLVs error = %v, want removal failure", err)
	}
	if run.removalCallCount != len(goldenIDs) {
		t.Fatalf("lvremove calls = %d, want %d independent attempts", run.removalCallCount, len(goldenIDs))
	}
	remaining := 0
	for _, exists := range run.physical {
		if exists {
			remaining++
		}
	}
	if remaining != 1 {
		t.Fatalf("physical golden LVs remaining = %d, want only failed removal", remaining)
	}
}

func TestListClones(t *testing.T) {
	tmpDir := t.TempDir()
	paths.SetCoreRootForTest(t, tmpDir)

	volDir := filepath.Join(tmpDir, "volumes")
	originVolumeID := "ws-origin"

	// Create origin metadata.
	originMeta := &volumeMetaV3{
		Version: metadataV3Version,
		Type:    "workspace",
		LVName:  originVolumeID,
		VGName:  "piccolo",
		FSType:  "btrfs",
	}
	originDir := filepath.Join(volDir, originVolumeID)
	if err := os.MkdirAll(originDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeVolumeMetaV3(filepath.Join(originDir, metadataV2File), originMeta); err != nil {
		t.Fatal(err)
	}

	// Create two clones.
	for _, cloneID := range []string{"ws-clone1", "ws-clone2"} {
		cloneMeta := &volumeMetaV3{
			Version: metadataV3Version,
			Type:    "workspace",
			LVName:  cloneID,
			VGName:  "piccolo",
			FSType:  "btrfs",
			CloneOf: originVolumeID,
		}
		cloneDir := filepath.Join(volDir, cloneID)
		if err := os.MkdirAll(cloneDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeVolumeMetaV3(filepath.Join(cloneDir, metadataV2File), cloneMeta); err != nil {
			t.Fatal(err)
		}
	}

	// Create an unrelated volume (should not be returned).
	otherMeta := &volumeMetaV3{
		Version: metadataV3Version,
		Type:    "workspace",
		LVName:  "ws-other",
		VGName:  "piccolo",
		FSType:  "btrfs",
	}
	otherDir := filepath.Join(volDir, "ws-other")
	if err := os.MkdirAll(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeVolumeMetaV3(filepath.Join(otherDir, metadataV2File), otherMeta); err != nil {
		t.Fatal(err)
	}

	mgr := &luksVolumeManager{}
	clones, err := mgr.ListClones(context.Background(), originVolumeID)
	if err != nil {
		t.Fatalf("ListClones: %v", err)
	}

	sort.Strings(clones)
	if len(clones) != 2 {
		t.Fatalf("expected 2 clones, got %d: %v", len(clones), clones)
	}
	if clones[0] != "ws-clone1" || clones[1] != "ws-clone2" {
		t.Fatalf("unexpected clones: %v", clones)
	}
}

func TestListClones_NoClones(t *testing.T) {
	tmpDir := t.TempDir()
	paths.SetCoreRootForTest(t, tmpDir)

	volDir := filepath.Join(tmpDir, "volumes")
	originVolumeID := "ws-solo"

	// Create origin with no clones.
	originMeta := &volumeMetaV3{
		Version: metadataV3Version,
		Type:    "workspace",
		LVName:  originVolumeID,
		VGName:  "piccolo",
		FSType:  "btrfs",
	}
	originDir := filepath.Join(volDir, originVolumeID)
	if err := os.MkdirAll(originDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeVolumeMetaV3(filepath.Join(originDir, metadataV2File), originMeta); err != nil {
		t.Fatal(err)
	}

	mgr := &luksVolumeManager{}
	clones, err := mgr.ListClones(context.Background(), originVolumeID)
	if err != nil {
		t.Fatalf("ListClones: %v", err)
	}
	if len(clones) != 0 {
		t.Fatalf("expected 0 clones, got %d: %v", len(clones), clones)
	}
}

// TestLiveResizeFromState pins the strict-resize state→spec contract.
// Codex iter-7 follow-up surfaced two pre-existing holes when this lived
// inline at the resize sites: (1) PartialMapperOnly advanced LV+meta
// while the open cryptsetup mapper stayed at the old size; (2)
// ambiguous / hostile states (Foreign / Unknown / Corrupted / Stale)
// collapsed into the Detached branch and silently advanced LV+meta.
// The helper now refuses on (2) and runs cryptsetup-only on (1).
func TestLiveResizeFromState(t *testing.T) {
	mapper := "test-mapper"
	fsResize := func(context.Context) error { return nil }

	cases := []struct {
		name         string
		state        AttachState
		wantErr      bool
		wantMapper   string
		wantFSResize bool
	}{
		{"Attached_full_live", AttachStateAttached, false, mapper, true},
		{"Detached_lv_only", AttachStateDetached, false, "", false},
		// PartialMapperOnly is refused on purpose: the enum doesn't
		// distinguish mapper-only from mapper+raw-fs-mounted, so resizing
		// could leave a mounted raw fs un-grown. Caller retries after
		// the reconciler completes the missing layer.
		{"PartialMapperOnly_refuses", AttachStatePartialMapperOnly, true, "", false},
		{"Foreign_refuses", AttachStateForeignMountAtPath, true, "", false},
		{"Unknown_refuses", AttachStateUnknown, true, "", false},
		{"Corrupted_refuses", AttachStateKernelStateCorrupted, true, "", false},
		{"Stale_refuses", AttachStateStaleMountRecord, true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live, err := liveResizeFromState(tc.state, mapper, fsResize)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error for state %s, got nil", tc.state)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for state %s: %v", tc.state, err)
			}
			if live.Mapper != tc.wantMapper {
				t.Fatalf("mapper: got %q, want %q", live.Mapper, tc.wantMapper)
			}
			if (live.FSResize != nil) != tc.wantFSResize {
				t.Fatalf("FSResize presence: got %v, want %v", live.FSResize != nil, tc.wantFSResize)
			}
		})
	}
}

func TestResizeConvergeRecoversPartialApplicationResize(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)
	cryptoMgr := newResizeTestCryptManager(t, core)
	run := &fakeRunner{
		Outputs: map[string]string{
			testutil.BuildKey("lvs", []string{
				"--noheadings", "--nosuffix", "--units", "b",
				"-o", "lv_size",
				"piccolo-data-vg/vol-app-drawguess",
			}): "21474836480\n",
		},
	}
	mgr := &luksVolumeManager{
		run:      run,
		crypto:   cryptoMgr,
		tmpfsDir: t.TempDir(),
		lvMgr:    lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
	}

	metaPath := filepath.Join(paths.VolumeMetaDir("app-drawguess"), metadataV2File)
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := &volumeMetaV3{
		Version:   metadataV3Version,
		Type:      volumeTypeServiceData,
		LVName:    "vol-app-drawguess",
		VGName:    lvm.DefaultVGName,
		SizeBytes: 10 << 30,
		FSType:    "ext4",
	}
	if err := writeVolumeMetaV3(metaPath, meta); err != nil {
		t.Fatal(err)
	}

	live := liveResizeSpec{
		Mapper: "piccolo-vol-app-drawguess",
		FSResize: func(ctx context.Context) error {
			return run.Run(ctx, "resize2fs", "/dev/mapper/piccolo-vol-app-drawguess")
		},
	}
	if err := mgr.resizeConverge(context.Background(), meta, metaPath, 20<<30, live); err != nil {
		t.Fatalf("resizeConverge: %v", err)
	}

	updated, err := readVolumeMetaV3(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SizeBytes != 20<<30 {
		t.Fatalf("metadata size: got %d, want %d", updated.SizeBytes, int64(20<<30))
	}

	calls := run.GetCalls()
	for _, call := range calls {
		if strings.HasPrefix(call, "lvresize ") {
			t.Fatalf("did not expect lvresize when LV already matched target, calls=%v", calls)
		}
	}
	requireCallContaining(t, calls, "cryptsetup resize --key-file ")
	requireCallContaining(t, calls, " piccolo-vol-app-drawguess")
	requireCallContaining(t, calls, "resize2fs /dev/mapper/piccolo-vol-app-drawguess")
}

func TestResizeConvergeDoesNotShrinkLVWhenMetadataIsStale(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)
	cryptoMgr := newResizeTestCryptManager(t, core)
	run := &fakeRunner{
		Outputs: map[string]string{
			testutil.BuildKey("lvs", []string{
				"--noheadings", "--nosuffix", "--units", "b",
				"-o", "lv_size",
				"piccolo-data-vg/vol-app-drawguess",
			}): "32212254720\n",
		},
	}
	mgr := &luksVolumeManager{
		run:      run,
		crypto:   cryptoMgr,
		tmpfsDir: t.TempDir(),
		lvMgr:    lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
	}

	metaPath := filepath.Join(paths.VolumeMetaDir("app-drawguess"), metadataV2File)
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := &volumeMetaV3{
		Version:   metadataV3Version,
		Type:      volumeTypeServiceData,
		LVName:    "vol-app-drawguess",
		VGName:    lvm.DefaultVGName,
		SizeBytes: 10 << 30,
		FSType:    "ext4",
	}
	if err := writeVolumeMetaV3(metaPath, meta); err != nil {
		t.Fatal(err)
	}

	live := liveResizeSpec{Mapper: "piccolo-vol-app-drawguess"}
	if err := mgr.resizeConverge(context.Background(), meta, metaPath, 20<<30, live); err != nil {
		t.Fatalf("resizeConverge: %v", err)
	}

	updated, err := readVolumeMetaV3(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SizeBytes != 30<<30 {
		t.Fatalf("metadata size: got %d, want %d", updated.SizeBytes, int64(30<<30))
	}

	calls := run.GetCalls()
	for _, call := range calls {
		if strings.HasPrefix(call, "lvresize ") {
			t.Fatalf("did not expect lvresize shrink from larger actual LV, calls=%v", calls)
		}
	}
	requireCallContaining(t, calls, "cryptsetup resize --key-file ")
	requireCallContaining(t, calls, " piccolo-vol-app-drawguess")
}

func newResizeTestCryptManager(t *testing.T, core string) *crypt.Manager {
	t.Helper()
	mgr, err := crypt.NewManager(core)
	if err != nil {
		t.Fatalf("crypt.NewManager: %v", err)
	}
	if err := mgr.Setup("test-password"); err != nil {
		t.Fatalf("crypt.Setup: %v", err)
	}
	if err := mgr.Unlock("test-password"); err != nil {
		t.Fatalf("crypt.Unlock: %v", err)
	}
	rawKey := make([]byte, 64)
	for i := range rawKey {
		rawKey[i] = byte(i + 1)
	}
	if err := mgr.StorePoolKeyfile(rawKey); err != nil {
		t.Fatalf("StorePoolKeyfile: %v", err)
	}
	return mgr
}

func requireCallContaining(t *testing.T, calls []string, needle string) {
	t.Helper()
	for _, call := range calls {
		if strings.Contains(call, needle) {
			return
		}
	}
	t.Fatalf("expected call containing %q, got %v", needle, calls)
}
