package tpm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-tpm/legacy/tpm2"
)

func TestIsStaleAKError(t *testing.T) {
	// Construct a real go-tpm error to version-pin the classifier against
	// the pinned go-tpm output. If go-tpm ever changes its error format, this
	// test breaks loudly before it can silently break the boot-time recovery.
	realStaleErr := fmt.Errorf("load ak: %w", tpm2.ParameterError{
		Code:      tpm2.RCIntegrity,
		Parameter: 1,
	})

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "real go-tpm ParameterError wrapped as load ak",
			err:  realStaleErr,
			want: true,
		},
		{
			name: "exact log-observed dev VM error string",
			err:  errors.New("load ak: parameter 1, error code 0x1f : integrity check failed"),
			want: true,
		},
		{
			name: "uppercase variant",
			err:  errors.New("LOAD AK: PARAMETER 1, ERROR CODE 0X1F : INTEGRITY CHECK FAILED"),
			want: true,
		},
		{
			name: "full RC code 0x9f form",
			err:  errors.New("load ak: tpm response code 0x9f"),
			want: true,
		},
		{
			name: "symbolic tpm_rc_integrity",
			err:  errors.New("load ak: tpm_rc_integrity"),
			want: true,
		},
		{
			name: "load ak prefix but wrong discriminator (not stale)",
			err:  errors.New("load ak: the handle is not correct for the use"),
			want: false,
		},
		{
			name: "nested substring — create ak key wraps load ak sub-step (false positive guard)",
			err:  errors.New("create ak key: load ak sub-step: integrity check failed"),
			want: false, // prefix is "create ak key:", not "load ak:"
		},
		{
			name: "load ak state (filesystem error, different wrapper)",
			err:  errors.New("load ak state: no such file or directory"),
			want: false, // prefix is "load ak state:", not "load ak:"
		},
		{
			name: "unrelated TPM error",
			err:  errors.New("create srk primary: handle not available"),
			want: false,
		},
		{
			name: "random non-tpm error",
			err:  errors.New("http: connection refused"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsStaleAKError(tc.err)
			if got != tc.want {
				t.Errorf("IsStaleAKError(%q) = %v, want %v", safeErr(tc.err), got, tc.want)
			}
		})
	}
}

func safeErr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func TestOpenWithAKRecovery_InsecurePermissions(t *testing.T) {
	// Create a tmpdir with wider-than-0o700 permissions. OpenWithAKRecovery
	// should refuse to touch it and return ErrInsecureAKStateDir, without
	// ever calling the underlying TPM Open.
	tmp := t.TempDir()
	akDir := filepath.Join(tmp, "ak")
	if err := os.Mkdir(akDir, 0o755); err != nil {
		t.Fatalf("setup: mkdir: %v", err)
	}
	// os.Mkdir applies the umask; explicitly chmod to the target mode to
	// guarantee the state the test expects.
	if err := os.Chmod(akDir, 0o755); err != nil {
		t.Fatalf("setup: chmod: %v", err)
	}

	swtpmDir := filepath.Join(tmp, "swtpm") // won't be used — gate refuses before Open
	_, recovered, err := OpenWithAKRecovery(akDir, swtpmDir)

	if err == nil {
		t.Fatal("OpenWithAKRecovery returned nil error on insecure dir; expected ErrInsecureAKStateDir")
	}
	if !errors.Is(err, ErrInsecureAKStateDir) {
		t.Errorf("err = %v; want wrapped ErrInsecureAKStateDir", err)
	}
	if recovered {
		t.Error("recovered = true; want false on security gate rejection")
	}
}

func TestOpenWithAKRecovery_InsecureAKFile(t *testing.T) {
	// Setup: a dir with correct 0o700 mode, but ak_priv has wider perms.
	// The file-level gate should reject auto-recovery before calling Open.
	tmp := t.TempDir()
	akDir := filepath.Join(tmp, "ak")
	if err := os.Mkdir(akDir, 0o700); err != nil {
		t.Fatalf("setup: mkdir: %v", err)
	}
	// Create a dummy ak_priv with 0o644 (group/world readable).
	akPriv := filepath.Join(akDir, "ak_priv")
	if err := os.WriteFile(akPriv, []byte("stub"), 0o644); err != nil {
		t.Fatalf("setup: write ak_priv: %v", err)
	}
	// WriteFile respects the umask; force the exact mode we want to test.
	if err := os.Chmod(akPriv, 0o644); err != nil {
		t.Fatalf("setup: chmod ak_priv: %v", err)
	}

	swtpmDir := filepath.Join(tmp, "swtpm")
	_, recovered, err := OpenWithAKRecovery(akDir, swtpmDir)

	if err == nil {
		t.Fatal("expected ErrInsecureAKStateDir on wide ak_priv perms")
	}
	if !errors.Is(err, ErrInsecureAKStateDir) {
		t.Errorf("err = %v; want wrapped ErrInsecureAKStateDir", err)
	}
	if recovered {
		t.Error("recovered = true; want false on security gate rejection")
	}
}

func TestOpenWithAKRecovery_SymlinkRejected(t *testing.T) {
	// Defense in depth: a symlink substituted for akStateDir or for an
	// ak_priv file must be rejected even if the target has correct
	// permissions and ownership. Uses Lstat.
	tmp := t.TempDir()
	target := filepath.Join(tmp, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("setup: mkdir target: %v", err)
	}
	akDir := filepath.Join(tmp, "ak")
	if err := os.Symlink(target, akDir); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	_, _, err := OpenWithAKRecovery(akDir, filepath.Join(tmp, "swtpm"))
	if err == nil {
		t.Fatal("expected ErrInsecureAKStateDir on symlinked akStateDir")
	}
	if !errors.Is(err, ErrInsecureAKStateDir) {
		t.Errorf("err = %v; want wrapped ErrInsecureAKStateDir", err)
	}
}

func TestOpenWithAKRecovery_MissingAKFilesOK(t *testing.T) {
	// A freshly created 0o700 dir with no ak_pub/ak_priv files should
	// pass the gate (missing files are fine — the recovery path would
	// create them). The subsequent Open call will fail in CI because
	// there's no TPM, but that failure must NOT be ErrInsecureAKStateDir.
	tmp := t.TempDir()
	akDir := filepath.Join(tmp, "ak")
	if err := os.Mkdir(akDir, 0o700); err != nil {
		t.Fatalf("setup: mkdir: %v", err)
	}
	swtpmDir := filepath.Join(tmp, "swtpm")

	_, _, err := OpenWithAKRecovery(akDir, swtpmDir)
	if err == nil {
		return // unlikely in CI, but not an error
	}
	if errors.Is(err, ErrInsecureAKStateDir) {
		t.Errorf("gate incorrectly rejected a well-formed empty 0o700 dir: %v", err)
	}
}

func TestOpenWithAKRecovery_WrongOwner(t *testing.T) {
	// This test can only meaningfully assert the UID check when run as a
	// non-root user against a dir owned by a different UID. In most CI
	// environments the test runs as a single user, so we skip the positive
	// case and only verify that a dir we own with 0o700 passes the gate's
	// permission/ownership check (the subsequent Open call will fail because
	// there's no TPM, but that's beyond what this test verifies).
	//
	// The gate should PASS the perm/owner check on a well-formed dir and
	// proceed to call tpm.Open, which will then fail with a TPM-specific
	// error. We assert that the error is NOT ErrInsecureAKStateDir.
	tmp := t.TempDir()
	akDir := filepath.Join(tmp, "ak")
	if err := os.Mkdir(akDir, 0o700); err != nil {
		t.Fatalf("setup: mkdir: %v", err)
	}
	swtpmDir := filepath.Join(tmp, "swtpm")

	_, _, err := OpenWithAKRecovery(akDir, swtpmDir)
	if err == nil {
		// Unlikely in CI, but not a test failure if somehow a TPM was available.
		return
	}
	if errors.Is(err, ErrInsecureAKStateDir) {
		t.Errorf("gate incorrectly rejected a well-formed 0o700-owned dir: %v", err)
	}
}
