package container

import (
	"fmt"
	"os"
	"os/user"
	"strings"
	"syscall"
	"testing"
)

// mockResolver implements CredentialResolver for testing.
type mockResolver struct {
	users  map[string]*user.User
	groups map[string]*user.Group
}

func (m *mockResolver) LookupUser(username string) (*user.User, error) {
	if u, ok := m.users[username]; ok {
		return u, nil
	}
	return nil, fmt.Errorf("user: unknown user %s", username)
}

func (m *mockResolver) LookupGroup(groupName string) (*user.Group, error) {
	if g, ok := m.groups[groupName]; ok {
		return g, nil
	}
	return nil, fmt.Errorf("group: unknown group %s", groupName)
}

// mockExecutor implements SystemExecutor for testing.
type mockExecutor struct {
	// calls records all (command, args) invocations for assertion.
	calls [][]string
	// results maps "command arg1 arg2..." to (output, error).
	results map[string]mockResult
	// defaultResult is returned when no specific result is configured.
	defaultResult mockResult
	// onRun is an optional callback invoked before returning from Run.
	// Tests use this to simulate side effects (e.g., useradd making a user visible).
	onRun func(name string, args ...string)
}

type mockResult struct {
	output []byte
	err    error
}

func (m *mockExecutor) Run(name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	m.calls = append(m.calls, call)
	if m.onRun != nil {
		m.onRun(name, args...)
	}
	key := strings.Join(call, " ")
	if r, ok := m.results[key]; ok {
		return r.output, r.err
	}
	return m.defaultResult.output, m.defaultResult.err
}

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
	start, err := findNextSlot(nil, SubUIDBase, SubUIDRangeSize)
	if err != nil {
		t.Fatalf("findNextSlot: %v", err)
	}
	if start != SubUIDBase {
		t.Errorf("expected %d, got %d", SubUIDBase, start)
	}
}

func TestFindNextSlot_after_existing(t *testing.T) {
	entries := []subUIDEntry{
		{Username: "piccolo-runtime", Start: 100000, Count: 65536},
		{Username: "pa-app1", Start: 200000, Count: 65536},
	}
	start, err := findNextSlot(entries, SubUIDBase, SubUIDRangeSize)
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
	start, err := findNextSlot(entries, SubUIDBase, SubUIDRangeSize)
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
	start, err := findNextSlot(entries, SubUIDBase, SubUIDRangeSize)
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
	start, err := findNextSlot(entries, SubUIDBase, SubUIDRangeSize)
	if err != nil {
		t.Fatalf("findNextSlot: %v", err)
	}
	if start != SubUIDBase {
		t.Errorf("expected %d, got %d", SubUIDBase, start)
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

func TestResolveRuntimeCredential_with_mock(t *testing.T) {
	old := defaultResolver
	defer func() { defaultResolver = old }()

	// NOTE: The mock replaces LookupUser, but u.GroupIds() still hits the OS.
	// This works because "test-runtime" doesn't exist in /etc/group, so GroupIds()
	// returns only the primary GID. If full group isolation is needed, add
	// GroupIds to the CredentialResolver interface.
	defaultResolver = &mockResolver{
		users: map[string]*user.User{
			"test-runtime": {
				Uid:      "1001",
				Gid:      "1001",
				Username: "test-runtime",
				HomeDir:  "/home/test-runtime",
			},
		},
	}

	ru, err := ResolveRuntimeCredential("test-runtime")
	if err != nil {
		t.Fatalf("ResolveRuntimeCredential: %v", err)
	}
	if ru.Credential.Uid != 1001 {
		t.Errorf("expected UID 1001, got %d", ru.Credential.Uid)
	}
	if ru.Credential.Gid != 1001 {
		t.Errorf("expected GID 1001, got %d", ru.Credential.Gid)
	}
	if ru.HomeDir != "/home/test-runtime" {
		t.Errorf("expected HomeDir /home/test-runtime, got %s", ru.HomeDir)
	}
}

func TestResolveRuntimeCredential_mock_not_found(t *testing.T) {
	old := defaultResolver
	defer func() { defaultResolver = old }()

	defaultResolver = &mockResolver{users: map[string]*user.User{}}

	_, err := ResolveRuntimeCredential("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent user")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestDestroyAppUser_UID0_guard_with_mock(t *testing.T) {
	oldResolver := defaultResolver
	oldExecutor := defaultExecutor
	defer func() {
		defaultResolver = oldResolver
		defaultExecutor = oldExecutor
	}()

	defaultResolver = &mockResolver{
		users: map[string]*user.User{
			"pa-evil": {Uid: "0", Gid: "0", Username: "pa-evil", HomeDir: "/root"},
		},
	}
	exec := &mockExecutor{results: map[string]mockResult{}}
	defaultExecutor = exec

	err := destroyAppUserByName("pa-evil")
	if err == nil {
		t.Fatal("expected error when UID is 0")
	}
	if !strings.Contains(err.Error(), "UID is 0") {
		t.Errorf("expected 'UID is 0' in error, got: %v", err)
	}
	// Verify no system commands were executed (no kill/userdel for root).
	if len(exec.calls) != 0 {
		t.Errorf("expected no system commands for UID 0, got: %v", exec.calls)
	}
}

func TestPodmanRuntime_Validate(t *testing.T) {
	tests := []struct {
		name    string
		rt      PodmanRuntime
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid_runtime",
			rt: PodmanRuntime{
				Credential: &syscall.Credential{Uid: 1000, Gid: 1000},
				HomeDir:    "/home/user",
			},
			wantErr: false,
		},
		{
			name:    "nil_credential",
			rt:      PodmanRuntime{HomeDir: "/home/user"},
			wantErr: true,
			errMsg:  "Credential is nil",
		},
		{
			name: "uid_zero",
			rt: PodmanRuntime{
				Credential: &syscall.Credential{Uid: 0, Gid: 0},
				HomeDir:    "/root",
			},
			wantErr: true,
			errMsg:  "UID is 0",
		},
		{
			name: "empty_homedir",
			rt: PodmanRuntime{
				Credential: &syscall.Credential{Uid: 1000, Gid: 1000},
			},
			wantErr: true,
			errMsg:  "HomeDir is empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.rt.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.errMsg != "" && err != nil && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("expected error containing %q, got: %v", tt.errMsg, err)
			}
		})
	}
}

func TestProvisionAppUser_useradd_failure_returns_error(t *testing.T) {
	oldResolver := defaultResolver
	oldExecutor := defaultExecutor
	defer func() {
		defaultResolver = oldResolver
		defaultExecutor = oldExecutor
	}()

	// NOTE: hasSubUIDAllocation/allocateSubUIDRange still read the real /etc/subuid.
	// This test works because the mock username won't appear in /etc/subuid.

	// Mock: user does not exist, group exists.
	defaultResolver = &mockResolver{
		users:  map[string]*user.User{},
		groups: map[string]*user.Group{AppsGroupName: {Name: AppsGroupName, Gid: "2000"}},
	}

	// Mock: groupadd succeeds, useradd fails.
	exec := &mockExecutor{
		results: map[string]mockResult{},
		defaultResult: mockResult{
			output: []byte("command failed"),
			err:    fmt.Errorf("exit status 1"),
		},
	}
	exec.results["groupadd -f "+AppsGroupName] = mockResult{output: nil, err: nil}
	defaultExecutor = exec

	_, err := ProvisionAppUser("test-fail-app")
	if err == nil {
		t.Fatal("expected error from ProvisionAppUser when useradd fails")
	}
	if !strings.Contains(err.Error(), "useradd") {
		t.Errorf("expected 'useradd' in error, got: %v", err)
	}
}

func TestProvisionAppUser_usermod_failure_triggers_rollback(t *testing.T) {
	oldResolver := defaultResolver
	oldExecutor := defaultExecutor
	defer func() {
		defaultResolver = oldResolver
		defaultExecutor = oldExecutor
	}()

	// NOTE: hasSubUIDAllocation/allocateSubUIDRange still read the real /etc/subuid.
	// This test works because the mock username won't appear in /etc/subuid.

	username := appUsername("test-rollback-app")

	// Resolver: user doesn't exist initially, group exists.
	// The onRun callback makes the user visible after useradd succeeds.
	resolver := &mockResolver{
		users:  map[string]*user.User{},
		groups: map[string]*user.Group{AppsGroupName: {Name: AppsGroupName, Gid: "2000"}},
	}
	defaultResolver = resolver

	exec := &mockExecutor{
		results: map[string]mockResult{},
		defaultResult: mockResult{
			output: []byte("command failed"),
			err:    fmt.Errorf("exit status 1"),
		},
	}
	// groupadd and useradd succeed; usermod (subuid) fails via default.
	exec.results["groupadd -f "+AppsGroupName] = mockResult{output: nil, err: nil}
	exec.results[strings.Join([]string{"useradd", "--system", "--shell", "/usr/sbin/nologin",
		"--create-home", "--groups", AppsGroupName, username}, " ")] = mockResult{output: nil, err: nil}
	defaultExecutor = exec

	// When useradd runs, make the user visible in the resolver so that
	// the rollback defer (which checks userExists) can find it.
	exec.onRun = func(name string, args ...string) {
		if name == "useradd" {
			resolver.users[username] = &user.User{
				Uid: "5000", Gid: "5000", Username: username, HomeDir: "/home/" + username,
			}
		}
	}

	_, err := ProvisionAppUser("test-rollback-app")
	if err == nil {
		t.Fatal("expected error from ProvisionAppUser when usermod fails")
	}

	// The rollback defer should attempt userdel since useradd created the user
	// (userExists was false) and a subsequent step failed.
	var foundRollback bool
	for _, call := range exec.calls {
		if len(call) >= 3 && call[0] == "userdel" && call[1] == "--remove" && call[2] == username {
			foundRollback = true
			break
		}
	}
	if !foundRollback {
		t.Errorf("expected rollback userdel for %s, calls were: %v", username, exec.calls)
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
