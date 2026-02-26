package blockdev

import (
	"context"
	"fmt"

	"piccolod/internal/storage/lvm"
)

// ThinLVDevice wraps an LVM thin logical volume as a BlockDevice.
// This is the bottom layer of the device stack.
type ThinLVDevice struct {
	mgr       *lvm.LVManager
	lvName    string
	sizeBytes int64
}

// NewThinLVDevice creates a thin LV device layer.
func NewThinLVDevice(mgr *lvm.LVManager, lvName string, sizeBytes int64) *ThinLVDevice {
	return &ThinLVDevice{
		mgr:       mgr,
		lvName:    lvName,
		sizeBytes: sizeBytes,
	}
}

func (d *ThinLVDevice) Name() string  { return "thinlv:" + d.lvName }
func (d *ThinLVDevice) Path() string  { return d.mgr.LVPath(d.lvName) }
func (d *ThinLVDevice) SizeBytes() int64 { return d.sizeBytes }

// Open activates the thin LV.
func (d *ThinLVDevice) Open(ctx context.Context) error {
	if err := d.mgr.ActivateLV(ctx, d.lvName); err != nil {
		return fmt.Errorf("activate thin LV %s: %w", d.lvName, err)
	}
	return nil
}

// Close deactivates the thin LV.
func (d *ThinLVDevice) Close(ctx context.Context) error {
	if err := d.mgr.DeactivateLV(ctx, d.lvName); err != nil {
		return fmt.Errorf("deactivate thin LV %s: %w", d.lvName, err)
	}
	return nil
}
