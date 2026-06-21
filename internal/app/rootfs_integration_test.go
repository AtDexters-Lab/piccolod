package app

import (
	"context"
	"slices"
	"testing"

	"piccolod/internal/persistence"
)

func TestReadImageConfigForRootfsFallsBackToLegacyRepoQualifiedGolden(t *testing.T) {
	rawDigest := "docker.io/example/piclu@sha256:legacy"
	canonicalDigest := "sha256:legacy"
	canonicalGolden := "golden-" + persistence.ShortDigest(canonicalDigest)
	legacyGolden := "golden-" + persistence.ShortDigest(rawDigest)
	rootfs := newStubRootfsManager(t.TempDir())
	rootfs.goldenConfigs = map[string]persistence.GoldenImageConfig{
		legacyGolden: {Cmd: []string{"/legacy"}},
	}

	cfg, err := (&AppManager{}).readImageConfigForRootfs(context.Background(), rootfs, rawDigest)
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
