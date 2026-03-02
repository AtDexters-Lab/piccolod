package server

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"piccolod/internal/persistence"
)

// volumeDiagnosticScanner is satisfied by luksVolumeManager.
type volumeDiagnosticScanner interface {
	ScanAllVolumes() ([]persistence.VolumeDiagnostic, error)
}

type storageDiagnosticsResponse struct {
	ThinPool    *thinPoolDiagnostic            `json:"thin_pool,omitempty"`
	GoldenLVs   []goldenLVDiagnostic           `json:"golden_lvs"`
	Volumes     []persistence.VolumeDiagnostic `json:"volumes"`
	TypeSummary map[string]typeSummary         `json:"type_summary"`
	CloneGraph  map[string][]string            `json:"clone_graph"`
}

type thinPoolDiagnostic struct {
	DataPercent     float64 `json:"data_percent"`
	MetadataPercent float64 `json:"metadata_percent"`
	TotalDataBytes  int64   `json:"total_data_bytes"`
	UsedDataBytes   int64   `json:"used_data_bytes"`
	FreeDataBytes   int64   `json:"free_data_bytes"`
	LVCount         int     `json:"lv_count"`
}

type goldenLVDiagnostic struct {
	VolumeID        string `json:"volume_id"`
	ImageRef        string `json:"image_ref"`
	ImageDigest     string `json:"image_digest"`
	SizeBytes       int64  `json:"size_bytes"`
	FlattenComplete string `json:"flatten_complete"`
	SnapshotCount   int    `json:"snapshot_count"`
}

type typeSummary struct {
	Count      int   `json:"count"`
	TotalBytes int64 `json:"total_bytes"`
}

// handleStorageDiagnostics handles GET /api/v1/system/storage-diagnostics.
func (s *GinServer) handleStorageDiagnostics(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	// Type-assert to get the diagnostic scanner.
	if s.persistence == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "persistence not available"})
		return
	}
	scanner, ok := s.persistence.Volumes().(volumeDiagnosticScanner)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "storage diagnostics not available"})
		return
	}

	volumes, err := scanner.ScanAllVolumes()
	if err != nil {
		log.Printf("WARN: storage-diagnostics: volume scan failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "volume scan failed"})
		return
	}
	if volumes == nil {
		volumes = []persistence.VolumeDiagnostic{}
	}

	// Thin pool stats (nil-safe chain).
	var poolDiag *thinPoolDiagnostic
	if s.storageMgr != nil {
		if poolMgr := s.storageMgr.LVMPool(); poolMgr != nil {
			stats, err := poolMgr.PoolStatus(ctx)
			if err != nil {
				log.Printf("WARN: storage-diagnostics: pool status: %v", err)
			} else {
				pd := thinPoolDiagnostic{
					DataPercent:     stats.DataPercent,
					MetadataPercent: stats.MetadataPercent,
					TotalDataBytes:  stats.TotalDataBytes,
					UsedDataBytes:   stats.UsedDataBytes,
					FreeDataBytes:   max(stats.TotalDataBytes-stats.UsedDataBytes, 0),
				}
				// LV count from listing.
				if lvMgr := s.storageMgr.LVMVolumes(); lvMgr != nil {
					lvs, err := lvMgr.ListLVs(ctx)
					if err != nil {
						log.Printf("WARN: storage-diagnostics: list LVs: %v", err)
					} else {
						pd.LVCount = len(lvs)
					}
				}
				poolDiag = &pd
			}
		}
	}

	resp := storageDiagnosticsResponse{
		ThinPool:    poolDiag,
		GoldenLVs:   buildGoldenInventory(volumes),
		Volumes:     volumes,
		TypeSummary: buildTypeSummary(volumes),
		CloneGraph:  buildCloneGraph(volumes),
	}

	c.JSON(http.StatusOK, resp)
}

// buildGoldenInventory extracts golden LV entries and counts how many
// other volumes reference each as their GoldenLV.
func buildGoldenInventory(volumes []persistence.VolumeDiagnostic) []goldenLVDiagnostic {
	// Count references to each golden LV.
	refCount := make(map[string]int)
	for _, v := range volumes {
		if v.GoldenLV != "" {
			refCount[v.GoldenLV]++
		}
	}

	var result []goldenLVDiagnostic
	for _, v := range volumes {
		if v.Type != "golden" {
			continue
		}
		result = append(result, goldenLVDiagnostic{
			VolumeID:        v.VolumeID,
			ImageRef:        v.BaseImageRef,
			ImageDigest:     v.BaseImageDigest,
			SizeBytes:       v.SizeBytes,
			FlattenComplete: v.FlattenComplete,
			// GoldenLV stores the LV name, matching v.LVName for golden volumes.
			SnapshotCount:   refCount[v.LVName],
		})
	}
	if result == nil {
		result = []goldenLVDiagnostic{}
	}
	return result
}

// buildTypeSummary groups volumes by type and aggregates counts and sizes.
func buildTypeSummary(volumes []persistence.VolumeDiagnostic) map[string]typeSummary {
	summary := make(map[string]typeSummary)
	for _, v := range volumes {
		s := summary[v.Type]
		s.Count++
		s.TotalBytes += v.SizeBytes
		summary[v.Type] = s
	}
	return summary
}

// buildCloneGraph builds a map from origin volume IDs to their clone volume IDs.
func buildCloneGraph(volumes []persistence.VolumeDiagnostic) map[string][]string {
	graph := make(map[string][]string)
	for _, v := range volumes {
		if v.CloneOf != "" {
			graph[v.CloneOf] = append(graph[v.CloneOf], v.VolumeID)
		}
	}
	return graph
}
