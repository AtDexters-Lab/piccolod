package persistence

import (
	"log"
	"path/filepath"

	"piccolod/internal/state/paths"
)

// VolumeDiagnostic is a read-only view of volume on-disk metadata.
// No crypto material (wrapped keys, nonces) is exposed.
type VolumeDiagnostic struct {
	VolumeID        string `json:"volume_id"`
	Type            string `json:"type"`
	LVName          string `json:"lv_name,omitempty"`
	VGName          string `json:"vg_name,omitempty"`
	SizeBytes       int64  `json:"size_bytes,omitempty"`
	FSType          string `json:"fs_type,omitempty"`
	ReadOnly        bool   `json:"read_only,omitempty"`
	BaseImageDigest string `json:"base_image_digest,omitempty"`
	BaseImageRef    string `json:"base_image_ref,omitempty"`
	GoldenLV        string `json:"golden_lv,omitempty"`
	CloneOf         string `json:"clone_of,omitempty"`
	FlattenComplete string `json:"flatten_complete,omitempty"`
}

// ScanAllVolumes reads all volume metadata from disk and returns a diagnostic
// view. No crypto material is exposed. Read-only — races with concurrent
// volume creation/deletion are benign for a diagnostic snapshot.
func (m *luksVolumeManager) ScanAllVolumes() ([]VolumeDiagnostic, error) {
	volIDs, err := listVolumeIDs()
	if err != nil {
		return nil, err
	}

	var result []VolumeDiagnostic
	for _, volID := range volIDs {
		metaPath := filepath.Join(paths.VolumeMetaDir(volID), metadataV2File)

		version, err := readVolumeMetaVersion(metaPath)
		if err != nil {
			log.Printf("WARN: ScanAllVolumes: skip %s: %v", volID, err)
			continue
		}

		var diag VolumeDiagnostic
		diag.VolumeID = volID

		switch version {
		case metadataV2Version:
			meta, err := readVolumeMetaV2(metaPath)
			if err != nil {
				log.Printf("WARN: ScanAllVolumes: skip v2 %s: %v", volID, err)
				continue
			}
			diag.Type = meta.Type
			diag.LVName = meta.LVName
			diag.VGName = meta.VGName
			diag.SizeBytes = meta.SizeBytes
			diag.FSType = meta.FSType

		case metadataV3Version:
			meta, err := readVolumeMetaV3(metaPath)
			if err != nil {
				log.Printf("WARN: ScanAllVolumes: skip v3 %s: %v", volID, err)
				continue
			}
			diag.Type = meta.Type
			diag.LVName = meta.LVName
			diag.VGName = meta.VGName
			diag.SizeBytes = meta.SizeBytes
			diag.FSType = meta.FSType
			diag.ReadOnly = meta.ReadOnly
			diag.BaseImageDigest = meta.BaseImageDigest
			diag.BaseImageRef = meta.BaseImageRef
			diag.GoldenLV = meta.GoldenLV
			diag.CloneOf = meta.CloneOf
			diag.FlattenComplete = meta.FlattenComplete

		default:
			log.Printf("WARN: ScanAllVolumes: skip %s: unknown version %d", volID, version)
			continue
		}

		result = append(result, diag)
	}
	return result, nil
}
