package pcv

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"piccolod/internal/runner"
	"piccolod/internal/state/paths"
)

var (
	ErrControlPlaneExists = errors.New("pcv: functional control plane exists; import is only available during setup or recovery")
	ErrHashMismatch       = errors.New("pcv: SHA-256 hash mismatch")
)

// Importer handles PCV archive import for device recovery and setup.
type Importer struct {
	run runner.CommandRunner
	mu  sync.Mutex
}

// NewImporter creates a PCV importer.
func NewImporter(run runner.CommandRunner) *Importer {
	return &Importer{run: run}
}

// Import extracts a PCV archive to the live core root layout.
//
// Preconditions:
//   - No functional control plane must exist (returns ErrControlPlaneExists otherwise)
//   - expectedSHA256 must match the archive content (hex-encoded)
//
// The import:
//  1. Verifies precondition (no functional CP)
//  2. SHA-256 verifies the archive
//  3. Extracts to staging area
//  4. Backs up any existing corrupt control plane
//  5. Moves staged contents to live layout
func (imp *Importer) Import(ctx context.Context, archivePath, expectedSHA256 string) error {
	imp.mu.Lock()
	defer imp.mu.Unlock()

	coreRoot := paths.CoreRoot()

	// Precondition: no functional control plane.
	if imp.hasFunctionalControlPlane(coreRoot) {
		return ErrControlPlaneExists
	}

	// SHA-256 verification.
	if err := imp.verifyHash(archivePath, expectedSHA256); err != nil {
		return err
	}

	// Extract to staging.
	stagingID := fmt.Sprintf("import-%d", os.Getpid())
	stagingDir := filepath.Join(coreRoot, "recovery", "import-staging", stagingID)
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return fmt.Errorf("create import staging: %w", err)
	}
	defer os.RemoveAll(stagingDir)

	if err := imp.extractArchive(ctx, archivePath, stagingDir); err != nil {
		return fmt.Errorf("extract archive: %w", err)
	}

	// Back up existing corrupt ciphertext if present.
	cpDir := filepath.Join(coreRoot, "ciphertext", "control-plane")
	if _, err := os.Stat(cpDir); err == nil {
		if err := imp.backupCorruptCP(ctx, coreRoot, cpDir); err != nil {
			return fmt.Errorf("backup corrupt control plane: %w", err)
		}
	}

	// Move staged contents to live layout.
	return imp.applyStaging(ctx, coreRoot, stagingDir)
}

// hasFunctionalControlPlane checks if a working control plane exists.
// Returns true if keyset.json exists AND ciphertext/control-plane/ has content.
func (imp *Importer) hasFunctionalControlPlane(coreRoot string) bool {
	keysetPath := filepath.Join(coreRoot, "crypto", "keyset.json")
	if _, err := os.Stat(keysetPath); err != nil {
		return false
	}

	cpDir := filepath.Join(coreRoot, "ciphertext", "control-plane")
	entries, err := os.ReadDir(cpDir)
	if err != nil || len(entries) == 0 {
		return false
	}

	return true
}

func (imp *Importer) verifyHash(archivePath, expectedSHA256 string) error {
	if expectedSHA256 == "" {
		return nil // intentionally optional: during recovery the manifest may be unavailable
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive for hash: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash archive: %w", err)
	}

	actual := fmt.Sprintf("%x", h.Sum(nil))
	if actual != expectedSHA256 {
		return fmt.Errorf("%w: expected %s, got %s", ErrHashMismatch, expectedSHA256, actual)
	}
	return nil
}

// maxExtractedSize is the maximum total decompressed size allowed during extraction.
// This guards against gzip/tar decompression bombs on the unauthenticated import endpoint.
const maxExtractedSize = 500 * 1024 * 1024 // 500 MiB

func (imp *Importer) extractArchive(ctx context.Context, archivePath, stagingDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	var totalExtracted int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		// Sanitize path to prevent directory traversal.
		cleanName := filepath.Clean(hdr.Name)
		if strings.HasPrefix(cleanName, "..") || filepath.IsAbs(cleanName) {
			return fmt.Errorf("invalid tar entry path: %s", hdr.Name)
		}

		target := filepath.Join(stagingDir, cleanName)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			n, copyErr := io.Copy(out, io.LimitReader(tr, maxExtractedSize-totalExtracted+1))
			out.Close()
			if copyErr != nil {
				return copyErr
			}
			totalExtracted += n
			if totalExtracted > maxExtractedSize {
				return fmt.Errorf("extracted data exceeds %d bytes limit", maxExtractedSize)
			}
		default:
			// Skip symlinks and other types for security.
			continue
		}
	}
	return nil
}

// backupCorruptCP renames existing ciphertext/control-plane/ to .bak.<pid>.
func (imp *Importer) backupCorruptCP(ctx context.Context, coreRoot, cpDir string) error {
	// Clean up old backups first.
	cipherDir := filepath.Dir(cpDir)
	entries, _ := os.ReadDir(cipherDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "control-plane.bak.") {
			old := filepath.Join(cipherDir, e.Name())
			if imp.run != nil {
				_ = imp.run.Run(ctx, "btrfs", "subvolume", "delete", old)
			}
			os.RemoveAll(old)
		}
	}

	backupName := fmt.Sprintf("control-plane.bak.%d", os.Getpid())
	backupPath := filepath.Join(cipherDir, backupName)
	return os.Rename(cpDir, backupPath)
}

// applyStaging moves staged files from the import staging area to the live layout.
func (imp *Importer) applyStaging(ctx context.Context, coreRoot, stagingDir string) error {
	// Walk the staging directory and move files to their live locations.
	return filepath.WalkDir(stagingDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(stagingDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		dest := filepath.Join(coreRoot, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o700)
		}

		// Ensure parent directory exists.
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}

		// Streaming copy (rename might fail across filesystems or read-only snapshot).
		if err := copyFileStreaming(path, dest); err != nil {
			return fmt.Errorf("copy staged file %s: %w", rel, err)
		}
		return nil
	})
}

func copyFileStreaming(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
