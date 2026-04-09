package tpm

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrInsecureAKStateDir is returned by OpenWithAKRecovery when the AK state
// directory has permissions or ownership wider than expected. This is a
// security gate: auto-recovery implicitly trusts that the blobs on disk are
// only writable by piccolod itself. If that assumption is violated, we refuse
// to delete/regenerate the AK to prevent a local attacker from weaponizing
// recovery into a re-enrollment/audit-poisoning generator.
var ErrInsecureAKStateDir = errors.New("insecure akStateDir permissions or ownership")

// verifySecureAKStateDir is the security gate for auto-recovery. It
// requires akStateDir to exist as a real directory (not a symlink) with
// mode 0o700 owned by the process UID, and any present ak_pub/ak_priv
// files to be 0o6xx (no group/world bits), not symlinks, and process-owned.
// Missing blob files are fine — RecoverAK would delete them anyway. On
// non-POSIX filesystems the UID check is skipped (Stat_t unavailable);
// POSIX is a documented deployment requirement. Lstat is used so an
// attacker cannot replace akStateDir or its contents with symlinks.
func verifySecureAKStateDir(akStateDir string) error {
	fi, err := os.Lstat(akStateDir)
	if err != nil {
		log.Printf("ERROR: tpm: cannot stat akStateDir %s: %v (caller invariant violated — directory must exist)", akStateDir, err)
		return fmt.Errorf("stat akStateDir: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		log.Printf("ERROR: tpm: akStateDir %s is a symlink — refusing auto-recovery", akStateDir)
		return fmt.Errorf("%w: dir is symlink", ErrInsecureAKStateDir)
	}
	if !fi.IsDir() {
		log.Printf("ERROR: tpm: akStateDir %s is not a directory — refusing auto-recovery", akStateDir)
		return fmt.Errorf("%w: not a directory", ErrInsecureAKStateDir)
	}
	if fi.Mode().Perm() != 0o700 {
		log.Printf("ERROR: tpm: akStateDir %s has insecure permissions (mode=%o, want 0700) — refusing auto-recovery to prevent tampering attacks", akStateDir, fi.Mode().Perm())
		return fmt.Errorf("%w: dir mode=%o", ErrInsecureAKStateDir, fi.Mode().Perm())
	}
	procUID := uint32(os.Geteuid())
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if st.Uid != procUID {
			log.Printf("ERROR: tpm: akStateDir %s has unexpected owner (uid=%d, want %d) — refusing auto-recovery", akStateDir, st.Uid, procUID)
			return fmt.Errorf("%w: dir uid=%d", ErrInsecureAKStateDir, st.Uid)
		}
	}

	for _, name := range []string{"ak_pub", "ak_priv"} {
		path := filepath.Join(akStateDir, name)
		ffi, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			log.Printf("ERROR: tpm: cannot stat %s: %v — refusing auto-recovery", path, err)
			return fmt.Errorf("stat %s: %w", name, err)
		}
		if ffi.Mode()&os.ModeSymlink != 0 {
			log.Printf("ERROR: tpm: %s is a symlink — refusing auto-recovery", path)
			return fmt.Errorf("%w: %s is symlink", ErrInsecureAKStateDir, name)
		}
		if ffi.Mode().Perm()&0o077 != 0 {
			log.Printf("ERROR: tpm: %s has insecure permissions (mode=%o, group/other bits must be 0) — refusing auto-recovery", path, ffi.Mode().Perm())
			return fmt.Errorf("%w: %s mode=%o", ErrInsecureAKStateDir, name, ffi.Mode().Perm())
		}
		if fst, ok := ffi.Sys().(*syscall.Stat_t); ok {
			if fst.Uid != procUID {
				log.Printf("ERROR: tpm: %s has unexpected owner (uid=%d, want %d) — refusing auto-recovery", path, fst.Uid, procUID)
				return fmt.Errorf("%w: %s uid=%d", ErrInsecureAKStateDir, name, fst.Uid)
			}
		}
	}
	return nil
}

// RecoverAK deletes stale AK files from akStateDir and retries Open.
// Called when TPM ownership was cleared between reboots (AK blobs reference old SRK).
func RecoverAK(akStateDir, swtpmStateDir string) (*OpenResult, error) {
	log.Printf("INFO: tpm: recovering AK — deleting stale blobs from %s", akStateDir)
	for _, name := range []string{"ak_pub", "ak_priv"} {
		path := filepath.Join(akStateDir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("WARN: tpm: failed to remove %s: %v", path, err)
		}
	}
	return Open(akStateDir, swtpmStateDir)
}

// IsStaleAKError reports whether err indicates the persisted AK blob cannot
// be unsealed under the current TPM hierarchy (typically after an SRK reseed
// via tpm2_clear or a vTPM rewind). The classifier is prefix-anchored to
// avoid false positives on nested substrings and tolerates go-tpm version
// drift across multiple forms of the TPM_RC_INTEGRITY response code.
func IsStaleAKError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.HasPrefix(msg, "load ak:") {
		return false
	}
	discriminators := []string{
		"integrity check", "tpm_rc_integrity", "rc_integrity", "0x1f", "0x9f",
	}
	for _, d := range discriminators {
		if strings.Contains(msg, d) {
			return true
		}
	}
	return false
}

// OpenWithAKRecovery is the boot-time counterpart to the runtime
// recoverAndReenroll path. It enforces akStateDir's security posture,
// calls Open, and on IsStaleAKError invokes RecoverAK once. The second
// return value reports whether recovery happened, so callers can force
// re-enrollment against namek (the server's stored AK pubkey is now stale).
//
// Caller must ensure akStateDir exists (os.MkdirAll with 0o700) before
// invoking. The gate refuses if the directory is absent.
//
// Synchronous and blocks boot by the AK-regen time (typically 1–3s on hw
// TPM, longer on swtpm). A timeout wrapper would be a footgun: partial
// recovery (blobs deleted but Open aborted) leaves worse state than a
// slow but completing recovery.
func OpenWithAKRecovery(akStateDir, swtpmStateDir string) (*OpenResult, bool, error) {
	if err := verifySecureAKStateDir(akStateDir); err != nil {
		return nil, false, err
	}

	result, err := Open(akStateDir, swtpmStateDir)
	if err == nil {
		return result, false, nil
	}
	if !IsStaleAKError(err) {
		log.Printf("DEBUG: tpm: OpenWithAKRecovery: error not classified as stale AK (%v) — classifier may need tuning", err)
		return nil, false, err
	}
	log.Printf("INFO: tpm: stale AK blob detected (%v), attempting recovery", err)
	result, recErr := RecoverAK(akStateDir, swtpmStateDir)
	if recErr != nil {
		return nil, false, fmt.Errorf("stale AK recovery: %w", recErr)
	}
	return result, true, nil
}
