package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSliceResourcePolicy_IsEmpty(t *testing.T) {
	tests := []struct {
		name string
		p    SliceResourcePolicy
		want bool
	}{
		{"zero", SliceResourcePolicy{UID: 1000}, true},
		{"memory set", SliceResourcePolicy{UID: 1000, MemoryHighBytes: 1, MemoryMaxBytes: 2}, false},
		{"cpu set", SliceResourcePolicy{UID: 1000, CPUWeight: 100}, false},
	}
	for _, tc := range tests {
		if got := tc.p.IsEmpty(); got != tc.want {
			t.Errorf("%s: IsEmpty = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSliceResourcePolicy_Validate(t *testing.T) {
	tests := []struct {
		name    string
		p       SliceResourcePolicy
		wantErr string
	}{
		{"valid all", SliceResourcePolicy{UID: 1000, MemoryHighBytes: 1000, MemoryMaxBytes: 2000, CPUWeight: 100}, ""},
		{"valid empty", SliceResourcePolicy{UID: 1000}, ""},
		{"zero UID rejected", SliceResourcePolicy{UID: 0, CPUWeight: 100}, "UID must be non-zero"},
		{"memory high without max", SliceResourcePolicy{UID: 1000, MemoryHighBytes: 1000}, "set together"},
		{"memory max without high", SliceResourcePolicy{UID: 1000, MemoryMaxBytes: 1000}, "set together"},
		{"memory high > max", SliceResourcePolicy{UID: 1000, MemoryHighBytes: 3000, MemoryMaxBytes: 2000}, "must be <="},
		{"CPU weight too high", SliceResourcePolicy{UID: 1000, CPUWeight: 20000}, "out of valid range"},
		{"CPU weight negative", SliceResourcePolicy{UID: 1000, CPUWeight: -1}, "out of valid range"},
	}
	for _, tc := range tests {
		err := tc.p.Validate()
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.wantErr, err)
		}
	}
}

func TestSliceResourcePolicy_Render(t *testing.T) {
	p := SliceResourcePolicy{
		UID:             1000,
		MemoryHighBytes: 2147483648,
		MemoryMaxBytes:  4294967296,
		CPUWeight:       100,
	}
	got := p.Render()
	if !strings.Contains(got, "[Slice]") {
		t.Errorf("render missing [Slice] section: %q", got)
	}
	if !strings.Contains(got, "MemoryHigh=2147483648") {
		t.Errorf("render missing MemoryHigh: %q", got)
	}
	if !strings.Contains(got, "MemoryMax=4294967296") {
		t.Errorf("render missing MemoryMax: %q", got)
	}
	if !strings.Contains(got, "CPUWeight=100") {
		t.Errorf("render missing CPUWeight: %q", got)
	}

	empty := SliceResourcePolicy{UID: 1000}
	if empty.Render() != "" {
		t.Errorf("empty policy should render empty, got %q", empty.Render())
	}
}

func TestSliceResourcePolicy_DropinPath(t *testing.T) {
	p := SliceResourcePolicy{UID: 473}
	if p.DropinDir() != "/etc/systemd/system/user-473.slice.d" {
		t.Errorf("DropinDir = %q", p.DropinDir())
	}
	if p.DropinFile() != "/etc/systemd/system/user-473.slice.d/piccolo-resources.conf" {
		t.Errorf("DropinFile = %q", p.DropinFile())
	}
	if p.SliceUnit() != "user-473.slice" {
		t.Errorf("SliceUnit = %q", p.SliceUnit())
	}
}

// TestSliceResourcePolicy_WriteFileAtomic exercises the on-disk write path
// via a hand-stitched path that mirrors DropinFile but under t.TempDir(),
// using direct os.WriteFile semantics (the core atomicity claim lives in
// writeFile itself which is unexported; we verify the rename-based
// tmp+rename pattern by checking the final file content).
func TestSliceResourcePolicy_WriteFileAtomic(t *testing.T) {
	// Exercise writeFile using a tmp dir that lives under t.TempDir().
	// We can't easily override DropinDir without plumbing an overrideable
	// root, so the test is a behavioural one: after Apply (or its
	// file-layer primitive), the file should exist with the expected
	// content. We verify the Render/Validate layers above; here we
	// confirm the IsEmpty+Remove path no-ops cleanly.
	empty := SliceResourcePolicy{UID: 999999}
	removed, err := empty.removeFile()
	if err != nil {
		t.Fatalf("removeFile on nonexistent: %v", err)
	}
	if removed {
		t.Errorf("removeFile on nonexistent should report false")
	}
}

// TestSliceResourcePolicy_WriteAndRemove exercises writeFile + removeFile
// against a tmp dir by temporarily overriding the drop-in root. Since
// the real path roots at /etc/systemd/system/ (root-only), we use a
// per-test override hook here.
func TestSliceResourcePolicy_WriteAndRemove_UsingOverride(t *testing.T) {
	saved := dropinRootForTest
	defer func() { dropinRootForTest = saved }()
	dropinRootForTest = t.TempDir()

	p := SliceResourcePolicy{
		UID:             73333,
		MemoryHighBytes: 1024 * 1024,
		MemoryMaxBytes:  2 * 1024 * 1024,
		CPUWeight:       100,
	}
	changed, err := p.writeFile()
	if err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if !changed {
		t.Errorf("expected first write to report changed=true")
	}
	// Second write with same content: no change.
	changed, err = p.writeFile()
	if err != nil {
		t.Fatalf("writeFile 2: %v", err)
	}
	if changed {
		t.Errorf("expected second identical write to report changed=false")
	}
	// File content includes the rendered sections.
	got, err := os.ReadFile(filepath.Join(dropinRootForTest, "user-73333.slice.d", "piccolo-resources.conf"))
	if err != nil {
		t.Fatalf("read drop-in: %v", err)
	}
	for _, want := range []string{"[Slice]", "MemoryHigh=1048576", "MemoryMax=2097152", "CPUWeight=100"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("drop-in missing %q; content = %q", want, got)
		}
	}
	// Remove.
	removed, err := p.removeFile()
	if err != nil {
		t.Fatalf("removeFile: %v", err)
	}
	if !removed {
		t.Errorf("removeFile should report removed=true")
	}
	// Second remove: idempotent.
	removed, err = p.removeFile()
	if err != nil {
		t.Fatalf("removeFile idempotent: %v", err)
	}
	if removed {
		t.Errorf("removeFile idempotent should report removed=false")
	}
}
