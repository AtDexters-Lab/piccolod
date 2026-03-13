package tpm

import (
	"context"
	"log"
	"os"

	"github.com/AtDexters-Lab/namek-server/pkg/swtpm"
	"github.com/AtDexters-Lab/namek-server/pkg/tpmdevice"
)

// Device wraps tpmdevice.Device for use across piccolod.
// Multiple consumers share one Device instance (opened once at boot).
type Device = tpmdevice.Device

// OpenResult holds the opened device and how it was opened.
type OpenResult struct {
	Device    Device
	Kind      string          // "hardware" or "software"
	SwtpmProc *swtpm.Process  // non-nil only for software TPM
}

// Open opens a TPM device and loads/creates AK blobs.
//   - akStateDir: where AK blobs are persisted (inside network-bootstrap)
//   - swtpmStateDir: where swtpm virtual TPM state is persisted (outside network-bootstrap)
//
// For hw TPM: tries /dev/tpmrm0 then /dev/tpm0, loads AK from akStateDir.
// For swtpm: starts swtpm from swtpmStateDir, loads AK from akStateDir.
func Open(akStateDir string, swtpmStateDir string) (*OpenResult, error) {
	ctx := context.Background()

	if os.Getenv("PICCOLO_USE_SWTPM") == "1" {
		return openSwtpm(ctx, akStateDir, swtpmStateDir)
	}

	// Try hardware TPM first.
	// If a device node exists but fails to open, return an error rather than
	// falling back to swtpm — silent fallback would create a new software
	// identity, changing EK/AK and breaking Namek device association.
	var hwErr error
	for _, path := range []string{"/dev/tpmrm0", "/dev/tpm0"} {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		dev, err := tpmdevice.Open(ctx, path, tpmdevice.WithStateDir(akStateDir))
		if err != nil {
			log.Printf("WARN: tpm: failed to open %s: %v", path, err)
			hwErr = err
			continue
		}
		log.Printf("INFO: tpm: opened hardware TPM at %s", path)
		return &OpenResult{Device: dev, Kind: "hardware"}, nil
	}
	if hwErr != nil {
		return nil, hwErr
	}

	// No hardware TPM device nodes found — fall back to swtpm
	log.Printf("INFO: tpm: no hardware TPM found, falling back to swtpm")
	return openSwtpm(ctx, akStateDir, swtpmStateDir)
}

func openSwtpm(ctx context.Context, akStateDir, swtpmStateDir string) (*OpenResult, error) {
	if err := os.MkdirAll(swtpmStateDir, 0o700); err != nil {
		return nil, err
	}
	proc, err := swtpm.Start(ctx, swtpmStateDir)
	if err != nil {
		return nil, err
	}
	dev, err := tpmdevice.Open(ctx, proc.Addr(), tpmdevice.WithStateDir(akStateDir))
	if err != nil {
		proc.Stop()
		return nil, err
	}
	log.Printf("INFO: tpm: opened software TPM (swtpm) from %s", swtpmStateDir)
	return &OpenResult{Device: dev, Kind: "software", SwtpmProc: proc}, nil
}

// Close releases the TPM device and stops swtpm process if running.
func (r *OpenResult) Close() error {
	if r == nil {
		return nil
	}
	if r.Device != nil {
		r.Device.Close()
	}
	if r.SwtpmProc != nil {
		r.SwtpmProc.Stop()
	}
	return nil
}
