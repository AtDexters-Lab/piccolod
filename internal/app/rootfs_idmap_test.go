package app

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/persistence"
	"piccolod/internal/state/paths"
)

// liveTestIDMap is the map of an app user re-created as 468/468 with an
// unchanged subordinate range, the identity pa-piccolospace has today. The
// GID range differs from the UID range to prove each comes from its own file.
var liveTestIDMap = persistence.IDMapConfig{
	AppUID: 468, AppGID: 468,
	SubUIDStart: 327680, SubUIDCount: 65536,
	SubGIDStart: 393216, SubGIDCount: 65536,
}

func liveTestRuntime() container.PodmanRuntime {
	return container.PodmanRuntime{Credential: &syscall.Credential{Uid: 468, Gid: 468}}
}

func liveTestSubIDRanges(string) (container.SubIDRanges, error) {
	return container.SubIDRanges{UIDStart: 327680, UIDCount: 65536, GIDStart: 393216, GIDCount: 65536}, nil
}

func TestAppIDMapFollowsTheLiveAppUser(t *testing.T) {
	var looked []string
	mgr := &AppManager{subIDRangeResolver: func(instanceID string) (container.SubIDRanges, error) {
		looked = append(looked, instanceID)
		return liveTestSubIDRanges(instanceID)
	}}
	got, err := mgr.appIDMap("site", liveTestRuntime())
	if err != nil {
		t.Fatalf("appIDMap: %v", err)
	}
	if got != liveTestIDMap {
		t.Fatalf("appIDMap = %+v, want %+v", got, liveTestIDMap)
	}
	if len(looked) != 1 || looked[0] != "site" {
		t.Fatalf("subordinate ranges looked up for %v, want [site]", looked)
	}
}

// A partial map would make attach rewrite the volume's record to drop the
// subordinate range, so a failed lookup or a missing credential stops it.
func TestAppIDMapFailsClosed(t *testing.T) {
	mgr := &AppManager{subIDRangeResolver: func(string) (container.SubIDRanges, error) {
		return container.SubIDRanges{}, errors.New("no subgid allocation for pa-site")
	}}
	if _, err := mgr.appIDMap("site", liveTestRuntime()); err == nil {
		t.Fatal("appIDMap must fail when the subordinate ranges are unknown")
	}
	mgr.subIDRangeResolver = liveTestSubIDRanges
	if _, err := mgr.appIDMap("site", container.PodmanRuntime{}); err == nil {
		t.Fatal("appIDMap must fail without an app user credential")
	}
}

func TestEnsureAllServiceRootfsAttachedPassesTheLiveIDMap(t *testing.T) {
	def := &api.AppDefinition{Services: map[string]api.AppService{"main": {Image: "registry.example/site:1"}}}
	for _, mode := range []PiccoloMode{ModeService, ModeWorkspace} {
		t.Run(string(mode), func(t *testing.T) {
			dir := t.TempDir()
			mgr, err := NewAppManagerForTest(NewMockContainerManager(), dir)
			if err != nil {
				t.Fatal(err)
			}
			mgr.subIDRangeResolver = liveTestSubIDRanges
			rootfs := newStubRootfsManager(dir)
			mgr.SetRootfsManager(rootfs)

			attached, err := mgr.ensureAllServiceRootfsAttached(context.Background(), "site", mode, def, nil, liveTestRuntime())
			if err != nil {
				t.Fatalf("ensureAllServiceRootfsAttached: %v", err)
			}
			if attached[networkAnchorServiceName] == nil || attached["main"] == nil {
				t.Fatalf("attached = %v, want the anchor and main rootfs", attached)
			}
			if len(rootfs.attachIDMaps) != 2 {
				t.Fatalf("attached volumes = %v, want the anchor and main rootfs", rootfs.attachIDMaps)
			}
			for volumeID, idmap := range rootfs.attachIDMaps {
				if idmap == nil || *idmap != liveTestIDMap {
					t.Fatalf("%s attached with %+v, want the live map %+v", volumeID, idmap, liveTestIDMap)
				}
			}
		})
	}
}

// Orphan cleanup treats surviving volumes as an installed app, so an app
// whose record or directory is unreadable keeps its user and IDs.
func TestReconcileOrphanCleanupKeepsUsersOfAppsWithVolumes(t *testing.T) {
	dir := t.TempDir()
	mgr, err := NewAppManagerForTest(NewMockContainerManager(), dir)
	if err != nil {
		t.Fatal(err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)
	var known map[string]bool
	mgr.orphanUserCleanup = func(ids map[string]bool) { known = ids }

	for _, volumeID := range []string{"ws-site", "svc-rootfs-site--__netns__", "app-notes", "svc-rootfs-blog--main--0123abcd"} {
		if err := os.MkdirAll(paths.VolumeMetaDir(volumeID), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(paths.CoreJoin("golden-references"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.CoreJoin("golden-references", "model"+artifactReferenceSeparator+"0123abcd.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr.ReconcileOnce(context.Background())
	if known == nil {
		t.Fatal("orphan cleanup did not run")
	}
	for _, instanceID := range []string{"site", "notes", "blog", "model"} {
		if !known[instanceID] {
			t.Errorf("orphan cleanup would remove pa-%s while its volumes exist; known=%v", instanceID, known)
		}
	}
}

// Without volume evidence the known set may be incomplete, so cleanup waits
// for a later pass instead of running on it.
func TestReconcileOrphanCleanupWaitsWhenVolumesCannotBeListed(t *testing.T) {
	dir := t.TempDir()
	mgr, err := NewAppManagerForTest(NewMockContainerManager(), dir)
	if err != nil {
		t.Fatal(err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)
	ran := false
	mgr.orphanUserCleanup = func(map[string]bool) { ran = true }

	// A file where the volume metadata directory belongs fails the listing.
	if err := os.RemoveAll(paths.CoreJoin("volumes")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.CoreJoin("volumes"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr.ReconcileOnce(context.Background())
	if ran {
		t.Fatal("orphan cleanup ran without volume evidence")
	}

	if err := os.Remove(paths.CoreJoin("volumes")); err != nil {
		t.Fatal(err)
	}
	mgr.ReconcileOnce(context.Background())
	if !ran {
		t.Fatal("orphan cleanup did not run once volumes could be listed")
	}
}
