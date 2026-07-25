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
	_ ...string,
) ([]byte, error) {
	if name == "lvs" && r.physicalExists {
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
	retryMeta, err := readVolumeMetaV3(metaPath)
	if err != nil {
		t.Fatalf("read retry metadata: %v", err)
	}
	retrySize := goldenLVSizeForImage(req.ImageSizeHint)
	if retryMeta.SizeBytes != retrySize || goldenReadyTimestamp(retryMeta) != "" {
		t.Fatalf("retry marker = %+v, want incomplete size %d", retryMeta, retrySize)
	}

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
