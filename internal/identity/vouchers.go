package identity

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"piccolod/internal/fsutil"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

// validFingerprint matches hex-encoded SHA-256 fingerprints (64 lowercase hex chars).
var validFingerprint = regexp.MustCompile(`^[a-f0-9]{64}$`)

// voucherDataPayload is the subset of voucher data fields we need to extract the fingerprint.
type voucherDataPayload struct {
	IssuerEKFingerprint string `json:"issuer_ek_fingerprint"`
}

// saveVoucher writes a voucher artifact to vouchers/<issuer_ek_fingerprint>.voucher.
// Overwrites existing file (handles AK rotation — new voucher from same issuer EK).
// Returns the issuer EK fingerprint used as the filename stem.
func saveVoucher(vouchersDir string, v namekclient.VoucherArtifact) (string, error) {
	fp, err := extractVoucherFingerprint(v)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(vouchersDir, 0o700); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(&v, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(vouchersDir, fp+".voucher")
	return fp, fsutil.AtomicWriteFile(path, data, 0o600)
}

// loadAllVouchers reads all .voucher files from the directory.
// Tolerates missing directory (returns nil). Skips corrupt files with a warning.
func loadAllVouchers(vouchersDir string) ([]namekclient.VoucherArtifact, error) {
	entries, err := os.ReadDir(vouchersDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var vouchers []namekclient.VoucherArtifact
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".voucher") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(vouchersDir, e.Name()))
		if err != nil {
			log.Printf("WARN: identity: skip voucher %s: read: %v", e.Name(), err)
			continue
		}
		var v namekclient.VoucherArtifact
		if err := json.Unmarshal(data, &v); err != nil {
			log.Printf("WARN: identity: skip voucher %s: parse: %v", e.Name(), err)
			continue
		}
		vouchers = append(vouchers, v)
	}
	return vouchers, nil
}

// extractVoucherFingerprint extracts the issuer_ek_fingerprint from a voucher's Data field.
// The Data field is base64-encoded canonical JSON containing issuer_ek_fingerprint.
func extractVoucherFingerprint(v namekclient.VoucherArtifact) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(v.Data)
	if err != nil {
		return "", err
	}
	var payload voucherDataPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	if payload.IssuerEKFingerprint == "" {
		return "", errors.New("voucher data missing issuer_ek_fingerprint")
	}
	if !validFingerprint.MatchString(payload.IssuerEKFingerprint) {
		return "", fmt.Errorf("invalid voucher fingerprint format: %q", payload.IssuerEKFingerprint)
	}
	return payload.IssuerEKFingerprint, nil
}

// clearVouchers removes all voucher files from the directory.
func clearVouchers(vouchersDir string) error {
	return os.RemoveAll(vouchersDir)
}
