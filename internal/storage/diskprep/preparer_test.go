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
	data := BuildSfdiskJSON(512, "", parts) // empty label defaults to "gpt"
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
	if out.PartitionTable.Label != storage.PartitionTableGPT {
		t.Errorf("Label = %q, want %q", out.PartitionTable.Label, storage.PartitionTableGPT)
	}
}

func TestBuildSfdiskJSON_MBR(t *testing.T) {
	parts := []storage.SfdiskPartition{
		{Node: "/dev/mmcblk0p1", Start: 8192, Size: 524288, Type: "c"},
		{Node: "/dev/mmcblk0p2", Start: 532480, Size: 4194304, Type: "83"},
	}
	data := BuildSfdiskJSON(512, "dos", parts)
	out, err := storage.ParseSfdiskJSON(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !out.IsMBR() {
		t.Error("IsMBR() = false, want true")
	}
	if out.PartitionTable.Partitions[0].Type != "c" {
		t.Errorf("partition 1 type = %q, want \"c\"", out.PartitionTable.Partitions[0].Type)
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
	calls         *[]string
	stdinCaptures *[][]byte // captures RunWithStdin input
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
	if r.stdinCaptures != nil {
		*r.stdinCaptures = append(*r.stdinCaptures, append([]byte(nil), stdin...))
	}
	return nil
}

func TestFindNextSlot_MBR_FourSlotLimit(t *testing.T) {
	sfdisk := storage.SfdiskOutput{}
	sfdisk.PartitionTable.Label = storage.PartitionTableMBR
	sfdisk.PartitionTable.Partitions = []storage.SfdiskPartition{
		{Node: "/dev/mmcblk0p1"},
		{Node: "/dev/mmcblk0p2"},
		{Node: "/dev/mmcblk0p3"},
		{Node: "/dev/mmcblk0p4"},
	}
	got := FindNextSlot(sfdisk)
	if got != 0 {
		t.Errorf("FindNextSlot = %d, want 0 (all 4 MBR slots used)", got)
	}
}

func TestFindNextSlot_MBR_TwoPartitions(t *testing.T) {
	sfdisk := storage.SfdiskOutput{}
	sfdisk.PartitionTable.Label = storage.PartitionTableMBR
	sfdisk.PartitionTable.Partitions = []storage.SfdiskPartition{
		{Node: "/dev/mmcblk0p1"},
		{Node: "/dev/mmcblk0p2"},
	}
	got := FindNextSlot(sfdisk)
	if got != 3 {
		t.Errorf("FindNextSlot = %d, want 3", got)
	}
}

func TestCreateDataPartition_MBR(t *testing.T) {
	// RPi layout: 256MB FAT32 boot (p1) + 2GB btrfs root (p2) on 32GB MBR disk.
	// Sector size 512, so 256MB = 524288 sectors, 2GB = 4194304 sectors.
	sfdiskJSON := BuildSfdiskJSON(512, "dos", []storage.SfdiskPartition{
		{Node: "/dev/mmcblk0p1", Start: 8192, Size: 524288, Type: "c"},
		{Node: "/dev/mmcblk0p2", Start: 532480, Size: 4194304, Type: "83"},
	})

	var calls []string
	var stdinCaptures [][]byte
	run := &fakeRunner{
		outputs: map[string]string{
			"findmnt -nro SOURCE /":         "/dev/mmcblk0p2",
			"sfdisk -J /dev/mmcblk0":        string(sfdiskJSON),
			"lsblk -bndo SIZE /dev/mmcblk0": "34359738368", // 32GB
		},
	}
	// We need a runner that both returns fake outputs AND records calls+stdin.
	// Use a composite: fakeRunner for outputs, wrap with recording for sfdisk -N.
	compositeRun := &compositeRunner{
		fake:          run,
		calls:         &calls,
		stdinCaptures: &stdinCaptures,
	}

	p := NewPreparer(compositeRun)
	// CreateDataPartition calls os.Stat to verify partition exists — this will
	// fail in tests. We test the command dispatch, not kernel registration.
	// The function will return an error about the partition not appearing, but
	// we can verify the commands that were issued.
	_, _, _ = p.CreateDataPartition(context.Background(), "/dev/mmcblk0")

	// Verify sgdisk was NOT called
	for _, c := range calls {
		if strings.HasPrefix(c, "sgdisk") {
			t.Errorf("sgdisk should not be called on MBR disk, got: %s", c)
		}
	}

	// Verify sfdisk -N 3 was called
	found := false
	for _, c := range calls {
		if strings.HasPrefix(c, "sfdisk -N 3 /dev/mmcblk0") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected sfdisk -N 3 call, got calls: %v", calls)
	}

	// Verify stdin contains correct start sector and type
	if len(stdinCaptures) == 0 {
		t.Fatal("no stdin captured for sfdisk -N")
	}
	stdinStr := string(stdinCaptures[0])
	if !strings.Contains(stdinStr, "type=83") {
		t.Errorf("sfdisk stdin should contain type=83, got: %q", stdinStr)
	}
	// Root starts at sector 532480, target root = 20GB = 20*1073741824/512 = 41943040 sectors.
	// rootTargetEnd = 532480 + 41943040 = 42475520. lastEnd = 532480+4194304 = 4726784.
	// max(42475520, 4726784) = 42475520. Already 1 MiB aligned (42475520 % 2048 == 0).
	if !strings.Contains(stdinStr, "start=42475520") {
		t.Errorf("sfdisk stdin should contain start=42475520, got: %q", stdinStr)
	}
}

func TestCreateDataPartition_GPT(t *testing.T) {
	sfdiskJSON := BuildSfdiskJSON(512, "gpt", []storage.SfdiskPartition{
		{Node: "/dev/sda1", Start: 2048, Size: 1048576, Type: "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"},
		{Node: "/dev/sda2", Start: 1050624, Size: 41943040, Type: "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709"},
	})

	var calls []string
	run := &fakeRunner{
		outputs: map[string]string{
			"sfdisk -J /dev/sda":        string(sfdiskJSON),
			"lsblk -bndo SIZE /dev/sda": "128849018880", // 120GB
		},
	}
	compositeRun := &compositeRunner{
		fake:  run,
		calls: &calls,
	}

	p := NewPreparer(compositeRun)
	_, _, _ = p.CreateDataPartition(context.Background(), "/dev/sda")

	// Verify sgdisk -e (GPT repair) was called
	foundRepair := false
	foundCreate := false
	for _, c := range calls {
		if c == "sgdisk -e /dev/sda" {
			foundRepair = true
		}
		if strings.HasPrefix(c, "sgdisk -n 3:") {
			foundCreate = true
		}
	}
	if !foundRepair {
		t.Errorf("expected sgdisk -e call for GPT repair, got calls: %v", calls)
	}
	if !foundCreate {
		t.Errorf("expected sgdisk -n call for partition creation, got calls: %v", calls)
	}

	// Verify sfdisk -N was NOT called
	for _, c := range calls {
		if strings.HasPrefix(c, "sfdisk -N") {
			t.Errorf("sfdisk -N should not be called on GPT disk, got: %s", c)
		}
	}
}

func TestFindDataPartition_MBR_SkipsRoot(t *testing.T) {
	// On MBR, both root (0x83) and data (0x83) have the same type code.
	// findDataPartition should skip root and find data via LUKS header probe.
	sfdiskJSON := BuildSfdiskJSON(512, "dos", []storage.SfdiskPartition{
		{Node: "/dev/mmcblk0p1", Start: 8192, Size: 524288, Type: "c"},
		{Node: "/dev/mmcblk0p2", Start: 532480, Size: 4194304, Type: "83"},
		{Node: "/dev/mmcblk0p3", Start: 43294720, Size: 24117248, Type: "83"},
	})

	sfdisk, _ := storage.ParseSfdiskJSON(sfdiskJSON)
	run := &fakeRunner{
		outputs: map[string]string{
			"findmnt -nro SOURCE /": "/dev/mmcblk0p2",
		},
		errs: map[string]error{
			// p1 (boot) is not LUKS
			"cryptsetup isLuks /dev/mmcblk0p1": fmt.Errorf("exit status 1"),
			// p3 (data) IS LUKS — but we simulate it succeeding (nil error = LUKS)
		},
	}
	p := NewPreparer(run)
	node, slot := p.findDataPartition(context.Background(), "/dev/mmcblk0", sfdisk)
	if node != "/dev/mmcblk0p3" {
		t.Errorf("findDataPartition node = %q, want /dev/mmcblk0p3", node)
	}
	if slot != 3 {
		t.Errorf("findDataPartition slot = %d, want 3", slot)
	}
}

// compositeRunner combines fakeRunner outputs with call recording + stdin capture.
type compositeRunner struct {
	fake          *fakeRunner
	calls         *[]string
	stdinCaptures *[][]byte
}

func (c *compositeRunner) Run(ctx context.Context, name string, args ...string) error {
	*c.calls = append(*c.calls, buildKey(name, args))
	return c.fake.Run(ctx, name, args...)
}

func (c *compositeRunner) RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	*c.calls = append(*c.calls, buildKey(name, args))
	return c.fake.RunWithOutput(ctx, name, args...)
}

func (c *compositeRunner) RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error {
	*c.calls = append(*c.calls, buildKey(name, args))
	if c.stdinCaptures != nil {
		*c.stdinCaptures = append(*c.stdinCaptures, append([]byte(nil), stdin...))
	}
	return c.fake.RunWithStdin(ctx, stdin, name, args...)
}
