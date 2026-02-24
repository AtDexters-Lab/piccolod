package container

import (
	"os"
	"strings"
	"testing"
)

func TestAppUsername_short_name(t *testing.T) {
	got := appUsername("myapp")
	want := "pa-myapp"
	if got != want {
		t.Errorf("appUsername(%q) = %q, want %q", "myapp", got, want)
	}
}

func TestAppUsername_exact_32_chars(t *testing.T) {
	// "pa-" (3 chars) + 29 chars = 32 total, no truncation
	instanceID := strings.Repeat("a", 29)
	got := appUsername(instanceID)
	want := "pa-" + instanceID
	if got != want {
		t.Errorf("appUsername(%q) = %q, want %q", instanceID, got, want)
	}
	if len(got) != 32 {
		t.Errorf("expected length 32, got %d", len(got))
	}
}

func TestAppUsername_truncation(t *testing.T) {
	// "pa-" (3 chars) + 30 chars = 33 total, needs truncation
	instanceID := strings.Repeat("b", 30)
	got := appUsername(instanceID)
	if len(got) != 32 {
		t.Errorf("expected truncated length 32, got %d for %q", len(got), got)
	}
	if !strings.HasPrefix(got, "pa-") {
		t.Errorf("expected prefix 'pa-', got %q", got)
	}
	// Should end with an 8-char hex hash after a dash
	parts := strings.Split(got, "-")
	lastPart := parts[len(parts)-1]
	if len(lastPart) != 8 {
		t.Errorf("expected 8-char hash suffix, got %q (len %d)", lastPart, len(lastPart))
	}
}

func TestAppUsername_deterministic(t *testing.T) {
	id := "my-long-app-instance-name-that-is-very-long"
	a := appUsername(id)
	b := appUsername(id)
	if a != b {
		t.Errorf("appUsername should be deterministic: %q != %q", a, b)
	}
}

func TestAppUsername_different_inputs_different_outputs(t *testing.T) {
	// Two different long inputs that would have the same prefix after truncation
	// should differ due to hash suffix.
	id1 := strings.Repeat("x", 40)
	id2 := strings.Repeat("x", 39) + "y"
	a := appUsername(id1)
	b := appUsername(id2)
	if a == b {
		t.Errorf("different inputs should produce different usernames: both got %q", a)
	}
}

func TestParseSubUIDFile_valid(t *testing.T) {
	content := `piccolo-runtime:100000:65536
pa-myapp:200000:65536
# comment line
pa-other:265536:65536
`
	tmpFile := writeTempFile(t, content)
	entries, err := parseSubUIDFile(tmpFile)
	if err != nil {
		t.Fatalf("parseSubUIDFile: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	tests := []struct {
		username string
		start    uint32
		count    uint32
	}{
		{"piccolo-runtime", 100000, 65536},
		{"pa-myapp", 200000, 65536},
		{"pa-other", 265536, 65536},
	}
	for i, tt := range tests {
		if entries[i].Username != tt.username {
			t.Errorf("entry[%d].Username = %q, want %q", i, entries[i].Username, tt.username)
		}
		if entries[i].Start != tt.start {
			t.Errorf("entry[%d].Start = %d, want %d", i, entries[i].Start, tt.start)
		}
		if entries[i].Count != tt.count {
			t.Errorf("entry[%d].Count = %d, want %d", i, entries[i].Count, tt.count)
		}
	}
}

func TestParseSubUIDFile_empty(t *testing.T) {
	tmpFile := writeTempFile(t, "")
	entries, err := parseSubUIDFile(tmpFile)
	if err != nil {
		t.Fatalf("parseSubUIDFile: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
}

func TestParseSubUIDFile_nonexistent(t *testing.T) {
	entries, err := parseSubUIDFile("/nonexistent/subuid")
	if err != nil {
		t.Fatalf("expected nil error for nonexistent file, got: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
}

func TestFindNextSlot_empty(t *testing.T) {
	start, err := findNextSlot(nil, subUIDBase, subUIDRangeSize)
	if err != nil {
		t.Fatalf("findNextSlot: %v", err)
	}
	if start != subUIDBase {
		t.Errorf("expected %d, got %d", subUIDBase, start)
	}
}

func TestFindNextSlot_after_existing(t *testing.T) {
	entries := []subUIDEntry{
		{Username: "piccolo-runtime", Start: 100000, Count: 65536},
		{Username: "pa-app1", Start: 200000, Count: 65536},
	}
	start, err := findNextSlot(entries, subUIDBase, subUIDRangeSize)
	if err != nil {
		t.Fatalf("findNextSlot: %v", err)
	}
	// First slot at 200000 is occupied (200000-265535).
	// Next aligned boundary: ceil(265536 / 65536) * 65536 = 5 * 65536 = 327680
	want := uint32(327680)
	if start != want {
		t.Errorf("expected %d, got %d", want, start)
	}
}

func TestFindNextSlot_gap_filling(t *testing.T) {
	// Second slot (265536) is occupied but first (200000) is free.
	entries := []subUIDEntry{
		{Username: "piccolo-runtime", Start: 100000, Count: 65536},
		{Username: "pa-app2", Start: 265536, Count: 65536},
	}
	start, err := findNextSlot(entries, subUIDBase, subUIDRangeSize)
	if err != nil {
		t.Fatalf("findNextSlot: %v", err)
	}
	// First slot at 200000 is free, should pick it.
	want := uint32(200000)
	if start != want {
		t.Errorf("expected %d, got %d", want, start)
	}
}

func TestFindNextSlot_unsorted_entries(t *testing.T) {
	// Entries in reverse order should still work correctly.
	entries := []subUIDEntry{
		{Username: "pa-app2", Start: 265536, Count: 65536},
		{Username: "pa-app1", Start: 200000, Count: 65536},
		{Username: "piccolo-runtime", Start: 100000, Count: 65536},
	}
	start, err := findNextSlot(entries, subUIDBase, subUIDRangeSize)
	if err != nil {
		t.Fatalf("findNextSlot: %v", err)
	}
	// 200000-265535 and 265536-331071 both occupied.
	// After aligning past 331071: ceil(331072/65536)*65536 = 6*65536 = 393216.
	want := uint32(393216)
	if start != want {
		t.Errorf("expected %d, got %d", want, start)
	}
}

func TestFindNextSlot_no_overlap_below_base(t *testing.T) {
	// Entries below base should not affect allocation.
	entries := []subUIDEntry{
		{Username: "other-user", Start: 50000, Count: 65536},
	}
	start, err := findNextSlot(entries, subUIDBase, subUIDRangeSize)
	if err != nil {
		t.Fatalf("findNextSlot: %v", err)
	}
	if start != subUIDBase {
		t.Errorf("expected %d, got %d", subUIDBase, start)
	}
}

func TestDestroyAppUser_nonexistent(t *testing.T) {
	// Destroying a nonexistent user should be a no-op.
	err := DestroyAppUser("nonexistent-app-xyz-12345")
	if err != nil {
		t.Fatalf("DestroyAppUser for nonexistent user should return nil, got: %v", err)
	}
}

func TestResolveAppUser_nonexistent(t *testing.T) {
	_, err := ResolveAppUser("nonexistent-app-xyz-12345")
	if err == nil {
		t.Fatal("ResolveAppUser for nonexistent user should return error")
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	tmpDir := os.TempDir()
	f, err := os.CreateTemp(tmpDir, "subuid-test-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		t.Fatalf("write temp file: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}
