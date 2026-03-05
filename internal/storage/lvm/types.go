package lvm

import "fmt"

const (
	// DefaultVGName is the LVM volume group for app data.
	DefaultVGName = "piccolo-data-vg"

	// DefaultThinPoolName is the thin pool LV within the VG.
	DefaultThinPoolName = "thinpool"

	// ThinPoolExtentPercent is the percentage of VG extents allocated to the thin pool.
	ThinPoolExtentPercent = 95

	// Thin pool fill thresholds for health monitoring events.
	ThresholdWarning  = 80
	ThresholdCritical = 90
	ThresholdUrgent   = 95
)

// ThinPoolConfig configures the LVM thin pool.
type ThinPoolConfig struct {
	VGName       string // volume group name
	PoolName     string // thin pool LV name
	ExtentPct    int    // percentage of VG to allocate (0 = default 95%)
	ErrorOnFull  bool   // use error_if_no_space (true) vs queue_if_no_space
}

// DefaultThinPoolConfig returns the standard thin pool configuration.
func DefaultThinPoolConfig() ThinPoolConfig {
	return ThinPoolConfig{
		VGName:      DefaultVGName,
		PoolName:    DefaultThinPoolName,
		ExtentPct:   ThinPoolExtentPercent,
		ErrorOnFull: true,
	}
}

// PoolPath returns the device mapper path for the thin pool.
func (c ThinPoolConfig) PoolPath() string {
	return fmt.Sprintf("/dev/%s/%s", c.VGName, c.PoolName)
}

// ThinLV represents a thin logical volume.
type ThinLV struct {
	Name      string // LV name (e.g., "vol-app-abc123")
	VGName    string // parent VG
	PoolName  string // parent thin pool
	SizeBytes int64  // virtual size
	Active    bool   // dm device active
	Path      string // /dev/<vg>/<lv>
}

// PoolStats holds thin pool utilization metrics.
type PoolStats struct {
	DataPercent     float64 // 0.0–100.0
	MetadataPercent float64 // 0.0–100.0
	TotalDataBytes  int64
	UsedDataBytes   int64
}

// ThresholdLevel returns the highest exceeded threshold, or 0 if none.
func (s PoolStats) ThresholdLevel() int {
	switch {
	case s.DataPercent >= ThresholdUrgent:
		return ThresholdUrgent
	case s.DataPercent >= ThresholdCritical:
		return ThresholdCritical
	case s.DataPercent >= ThresholdWarning:
		return ThresholdWarning
	default:
		return 0
	}
}
