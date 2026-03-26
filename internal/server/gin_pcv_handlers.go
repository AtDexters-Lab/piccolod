package server

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"

	"piccolod/internal/pcv"
	"piccolod/internal/state/paths"
)

// handlePCVPublish: POST /api/v1/system/pcv/publish
// Triggers an on-demand PCV export.
func (s *GinServer) handlePCVPublish(c *gin.Context) {
	if s.pcvPublisher == nil {
		writeGinError(c, http.StatusServiceUnavailable, "PCV publisher not available")
		return
	}

	manifest, err := s.pcvPublisher.PublishNow(c.Request.Context())
	if err != nil {
		writeGinError(c, http.StatusInternalServerError, "PCV publish failed: "+err.Error())
		return
	}

	writeGinSuccess(c, gin.H{
		"generation": manifest.Gen,
		"sha256":     manifest.SHA256,
		"size_bytes": manifest.SizeBytes,
	}, "PCV export published")
}

// handlePCVExport: GET /api/v1/system/pcv/export
// Downloads the current PCV archive.
func (s *GinServer) handlePCVExport(c *gin.Context) {
	archivePath := filepath.Join(paths.CoreRoot(), "recovery", "current.enc")
	info, err := os.Stat(archivePath)
	if err != nil {
		if os.IsNotExist(err) {
			writeGinError(c, http.StatusNotFound, "no PCV archive available")
		} else {
			writeGinError(c, http.StatusInternalServerError, "stat archive: "+err.Error())
		}
		return
	}

	f, err := os.Open(archivePath)
	if err != nil {
		writeGinError(c, http.StatusInternalServerError, "open archive: "+err.Error())
		return
	}
	defer f.Close()

	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", "attachment; filename=\"piccolo-backup.pcv\"")
	c.Status(http.StatusOK)
	io.CopyN(c.Writer, f, info.Size())
}

// handlePCVImport: POST /api/v1/system/pcv/import
// Imports a PCV archive for device recovery. LAN-only (enforced by middleware).
func (s *GinServer) handlePCVImport(c *gin.Context) {
	if s.pcvImporter == nil {
		writeGinError(c, http.StatusServiceUnavailable, "PCV importer not available")
		return
	}

	// Read uploaded file.
	file, _, err := c.Request.FormFile("archive")
	if err != nil {
		writeGinError(c, http.StatusBadRequest, "missing archive file in request")
		return
	}
	defer file.Close()

	expectedHash := c.Request.FormValue("sha256")

	// Write to temp file with size limit.
	const maxImportSize = 100 * 1024 * 1024 // 100 MiB
	importStaging := filepath.Join(paths.CoreRoot(), "recovery", "import-staging")
	if err := os.MkdirAll(importStaging, 0o700); err != nil {
		writeGinError(c, http.StatusInternalServerError, "create import staging dir: "+err.Error())
		return
	}
	tmpFile, err := os.CreateTemp(importStaging, "pcv-import-*.enc")
	if err != nil {
		writeGinError(c, http.StatusInternalServerError, "create temp file: "+err.Error())
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	limited := io.LimitReader(file, maxImportSize+1)
	n, err := io.Copy(tmpFile, limited)
	if err != nil {
		tmpFile.Close()
		writeGinError(c, http.StatusInternalServerError, "save uploaded archive: "+err.Error())
		return
	}
	tmpFile.Close()
	if n > maxImportSize {
		writeGinError(c, http.StatusRequestEntityTooLarge, "archive exceeds maximum size of 100 MiB")
		return
	}

	// Run import.
	if err := s.pcvImporter.Import(c.Request.Context(), tmpPath, expectedHash); err != nil {
		if errors.Is(err, pcv.ErrControlPlaneExists) {
			writeGinError(c, http.StatusConflict,
				"Import is only available during setup or recovery. The existing control plane must be absent or structurally corrupted.")
			return
		}
		if errors.Is(err, pcv.ErrHashMismatch) {
			writeGinError(c, http.StatusBadRequest, "SHA-256 hash mismatch: archive integrity check failed")
			return
		}
		writeGinError(c, http.StatusInternalServerError, "PCV import failed: "+err.Error())
		return
	}

	writeGinSuccess(c, nil, "PCV archive imported successfully")
}
