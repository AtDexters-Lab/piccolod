package identity

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

// testFingerprint returns a valid 64-char hex fingerprint with a given prefix (padded with zeros).
func testFingerprint(prefix string) string {
	fp := prefix
	for len(fp) < 64 {
		fp += "0"
	}
	return fp[:64]
}

func makeVoucherData(issuerEK string) string {
	return base64.StdEncoding.EncodeToString(
		[]byte(`{"issuer_ek_fingerprint":"` + issuerEK + `","subject_ek_fingerprint":"subj123","account_id":"acct-1","type":"peer_membership","version":1}`),
	)
}

func TestSaveAndLoadVouchers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vouchers")

	v1 := namekclient.VoucherArtifact{
		Data:           makeVoucherData(testFingerprint("aa")),
		Quote:          "quote1",
		IssuerAKPubKey: "ak1",
		IssuerEKCert:   "cert1",
	}
	v2 := namekclient.VoucherArtifact{
		Data:           makeVoucherData(testFingerprint("bb")),
		Quote:          "quote2",
		IssuerAKPubKey: "ak2",
	}

	if _, err := saveVoucher(dir, v1); err != nil {
		t.Fatalf("save v1: %v", err)
	}
	if _, err := saveVoucher(dir, v2); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	loaded, err := loadAllVouchers(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 vouchers, got %d", len(loaded))
	}
}

func TestSaveVoucher_OverwritesExisting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vouchers")

	v1 := namekclient.VoucherArtifact{
		Data:           makeVoucherData(testFingerprint("cc")),
		Quote:          "old_quote",
		IssuerAKPubKey: "old_ak",
	}
	v2 := namekclient.VoucherArtifact{
		Data:           makeVoucherData(testFingerprint("cc")),
		Quote:          "new_quote",
		IssuerAKPubKey: "new_ak",
	}

	if _, err := saveVoucher(dir, v1); err != nil {
		t.Fatal(err)
	}
	if _, err := saveVoucher(dir, v2); err != nil {
		t.Fatal(err)
	}

	loaded, _ := loadAllVouchers(dir)
	if len(loaded) != 1 {
		t.Fatalf("expected 1 voucher (overwrite), got %d", len(loaded))
	}
	if loaded[0].Quote != "new_quote" {
		t.Errorf("expected new_quote, got %s", loaded[0].Quote)
	}
}

func TestLoadAllVouchers_MissingDir(t *testing.T) {
	vouchers, err := loadAllVouchers(filepath.Join(t.TempDir(), "nonexistent"))
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if vouchers != nil {
		t.Fatal("expected nil for missing dir")
	}
}

func TestLoadAllVouchers_SkipsCorrupt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vouchers")
	os.MkdirAll(dir, 0o700)

	// Write one valid voucher
	v := namekclient.VoucherArtifact{
		Data:           makeVoucherData(testFingerprint("dd")),
		Quote:          "q",
		IssuerAKPubKey: "ak",
	}
	_, _ = saveVoucher(dir, v)

	// Write a corrupt file
	os.WriteFile(filepath.Join(dir, "corrupt.voucher"), []byte("not json"), 0o600)

	loaded, err := loadAllVouchers(dir)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 valid voucher, got %d", len(loaded))
	}
}

func TestExtractVoucherFingerprint(t *testing.T) {
	expected := testFingerprint("abcdef")
	v := namekclient.VoucherArtifact{Data: makeVoucherData(expected)}
	fp, err := extractVoucherFingerprint(v)
	if err != nil {
		t.Fatal(err)
	}
	if fp != expected {
		t.Errorf("fingerprint = %q, want %q", fp, expected)
	}
}

func TestExtractVoucherFingerprint_InvalidBase64(t *testing.T) {
	v := namekclient.VoucherArtifact{Data: "!!!invalid!!!"}
	_, err := extractVoucherFingerprint(v)
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestExtractVoucherFingerprint_MissingField(t *testing.T) {
	v := namekclient.VoucherArtifact{
		Data: base64.StdEncoding.EncodeToString([]byte(`{"type":"peer_membership"}`)),
	}
	_, err := extractVoucherFingerprint(v)
	if err == nil {
		t.Fatal("expected error for missing fingerprint")
	}
}

func TestExtractVoucherFingerprint_PathTraversal(t *testing.T) {
	v := namekclient.VoucherArtifact{
		Data: base64.StdEncoding.EncodeToString(
			[]byte(`{"issuer_ek_fingerprint":"../../../etc/cron.d/evil","type":"peer_membership","version":1}`),
		),
	}
	_, err := extractVoucherFingerprint(v)
	if err == nil {
		t.Fatal("expected error for path traversal fingerprint")
	}
}

func TestExtractVoucherFingerprint_InvalidHex(t *testing.T) {
	v := namekclient.VoucherArtifact{
		Data: base64.StdEncoding.EncodeToString(
			[]byte(`{"issuer_ek_fingerprint":"not-hex-at-all!","type":"peer_membership","version":1}`),
		),
	}
	_, err := extractVoucherFingerprint(v)
	if err == nil {
		t.Fatal("expected error for non-hex fingerprint")
	}
}

func TestClearVouchers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vouchers")
	v := namekclient.VoucherArtifact{
		Data:           makeVoucherData(testFingerprint("ee")),
		Quote:          "q",
		IssuerAKPubKey: "ak",
	}
	_, _ = saveVoucher(dir, v)

	if err := clearVouchers(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("expected dir to be removed")
	}
}
