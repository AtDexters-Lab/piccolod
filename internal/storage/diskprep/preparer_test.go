package diskprep

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"piccolod/internal/state/paths"
	"piccolod/internal/storage"
)

// fakeRunner is a test double for runner.CommandRunner.
type fakeRunner struct {
	outputs map[string]string
	errs    map[string]error
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) error {
	key := buildKey(name, args)
	if f.errs != nil {
		if err, ok := f.errs[key]; ok {
			return err
		}
	}
	return nil
}

func (f *fakeRunner) RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	key := buildKey(name, args)
	if f.errs != nil {
		if err, ok := f.errs[key]; ok {
			return nil, err
		}
	}
	if f.outputs != nil {
		if out, ok := f.outputs[key]; ok {
			return []byte(out), nil
		}
	}
	return nil, fmt.Errorf("fakeRunner: no output for %q", key)
}

func (f *fakeRunner) RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error {
	key := buildKey(name, args)
	if f.errs != nil {
		if err, ok := f.errs[key]; ok {
			return err
		}
	}
	return nil
}

func buildKey(name string, args []string) string {
	parts := append([]string{name}, args...)
	return strings.Join(parts, " ")
}

func TestFindNextSlot_TwoPartitions(t *testing.T) {
	sfdisk := storage.SfdiskOutput{}
	sfdisk.PartitionTable.SectorSize = 512
	sfdisk.PartitionTable.Partitions = []storage.SfdiskPartition{
		{Node: "/dev/sda1", Start: 2048, Size: 1048576},
		{Node: "/dev/sda2", Start: 1050624, Size: 41943040},
	}
	got := FindNextSlot(sfdisk)
	if got != 3 {
		t.Errorf("FindNextSlot = %d, want 3", got)
	}
}

func TestFindNextSlot_NVMe(t *testing.T) {
	sfdisk := storage.SfdiskOutput{}
	sfdisk.PartitionTable.Partitions = []storage.SfdiskPartition{
		{Node: "/dev/nvme0n1p1"},
		{Node: "/dev/nvme0n1p2"},
	}
	got := FindNextSlot(sfdisk)
	if got != 3 {
		t.Errorf("FindNextSlot = %d, want 3", got)
	}
}

func TestFindNextSlot_GapInSlots(t *testing.T) {
	sfdisk := storage.SfdiskOutput{}
	sfdisk.PartitionTable.Partitions = []storage.SfdiskPartition{
		{Node: "/dev/sda1"},
		{Node: "/dev/sda3"}, // slot 2 is free
	}
	got := FindNextSlot(sfdisk)
	if got != 2 {
		t.Errorf("FindNextSlot = %d, want 2 (gap)", got)
	}
}

func TestExtractSlotNumber(t *testing.T) {
	tests := []struct {
		node string
		want int
	}{
		{"/dev/sda1", 1},
		{"/dev/sda3", 3},
		{"/dev/nvme0n1p2", 2},
		{"/dev/mmcblk0p1", 1},
		{"/dev/sda", 0},
	}
	for _, tt := range tests {
		t.Run(tt.node, func(t *testing.T) {
			got := extractSlotNumber(tt.node)
			if got != tt.want {
				t.Errorf("extractSlotNumber(%q) = %d, want %d", tt.node, got, tt.want)
			}
		})
	}
}

func TestVerifyPiccoloCoreExists(t *testing.T) {
	p := NewPreparer(&fakeRunner{})
	dir := t.TempDir()
	if !p.VerifyPiccoloCoreExists(context.Background(), dir) {
		t.Error("expected true for existing directory")
	}
	if p.VerifyPiccoloCoreExists(context.Background(), dir+"/nonexistent") {
		t.Error("expected false for missing directory")
	}
}

func TestBuildSfdiskJSON_Roundtrip(t *testing.T) {
	parts := []storage.SfdiskPartition{
		{Node: "/dev/sda1", Start: 2048, Size: 1048576, Type: "efi"},
		{Node: "/dev/sda2", Start: 1050624, Size: 41943040, Type: "linux"},
	}
	data := BuildSfdiskJSON(512, parts)
	out, err := storage.ParseSfdiskJSON(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out.PartitionTable.Partitions) != 2 {
		t.Fatalf("expected 2 partitions, got %d", len(out.PartitionTable.Partitions))
	}
	if out.PartitionTable.SectorSize != 512 {
		t.Errorf("SectorSize = %d, want 512", out.PartitionTable.SectorSize)
	}
}

func TestEnsureDirectories(t *testing.T) {
	_, data := paths.SetRootsForTest(t)

	p := NewPreparer(&fakeRunner{})
	if err := p.EnsureDirectories(context.Background()); err != nil {
		t.Fatalf("EnsureDirectories: %v", err)
	}

	expected := []string{
		"node",
		filepath.Join("user", "volumes"),
		"federation",
		filepath.Join("system-objects", "control-plane-backups"),
		filepath.Join("system-objects", "volume-checkpoints"),
	}
	for _, rel := range expected {
		full := filepath.Join(data, rel)
		info, err := os.Stat(full)
		if err != nil {
			t.Errorf("expected directory %s to exist: %v", rel, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("expected %s to be a directory", rel)
		}
	}
}

func TestEnsureDirectories_Idempotent(t *testing.T) {
	paths.SetRootsForTest(t)

	p := NewPreparer(&fakeRunner{})
	if err := p.EnsureDirectories(context.Background()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := p.EnsureDirectories(context.Background()); err != nil {
		t.Fatalf("second call (idempotent): %v", err)
	}
}

func TestExpandRootPartition_CallsCorrectCommands(t *testing.T) {
	var called []string
	run := &recordingRunner{calls: &called}

	p := NewPreparer(run)
	err := p.ExpandRootPartition(context.Background(), "/dev/sda", "/dev/sda2")
	if err != nil {
		t.Fatalf("ExpandRootPartition: %v", err)
	}

	if len(called) != 2 {
		t.Fatalf("expected 2 commands, got %d: %v", len(called), called)
	}
	if called[0] != "growpart /dev/sda 2" {
		t.Errorf("first command = %q, want growpart", called[0])
	}
	if called[1] != "btrfs filesystem resize max /var" {
		t.Errorf("second command = %q, want btrfs resize", called[1])
	}
}

func TestExpandRootPartition_BadSlot(t *testing.T) {
	p := NewPreparer(&fakeRunner{})
	err := p.ExpandRootPartition(context.Background(), "/dev/sda", "/dev/sda")
	if err == nil {
		t.Fatal("expected error for device without partition number")
	}
}

func TestSetNOCOWAttributes_NonFatal(t *testing.T) {
	paths.SetRootsForTest(t)

	// chattr will fail (no real chattr in test), but SetNOCOWAttributes is non-fatal.
	run := &fakeRunner{
		errs: map[string]error{
			"chattr +C " + paths.DataJoin("node"):       fmt.Errorf("chattr failed"),
			"chattr +C " + paths.DataJoin("federation"): fmt.Errorf("chattr failed"),
		},
	}

	p := NewPreparer(run)
	// Ensure dirs exist first so chattr has targets.
	_ = p.EnsureDirectories(context.Background())
	// Should not panic or return error.
	p.SetNOCOWAttributes(context.Background())
}

// recordingRunner records Run calls for verifying command sequences.
type recordingRunner struct {
	calls *[]string
}

func (r *recordingRunner) Run(ctx context.Context, name string, args ...string) error {
	*r.calls = append(*r.calls, buildKey(name, args))
	return nil
}

func (r *recordingRunner) RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	*r.calls = append(*r.calls, buildKey(name, args))
	return nil, nil
}

func (r *recordingRunner) RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error {
	*r.calls = append(*r.calls, buildKey(name, args))
	return nil
}
