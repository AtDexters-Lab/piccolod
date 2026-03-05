package blockdev

import (
	"context"
	"fmt"

	"piccolod/internal/storage/drbd"
)

// DRBDDevice wraps a DRBD resource as a BlockDevice.
// This is the top layer of the device stack — what LUKS operates on.
type DRBDDevice struct {
	ops       *drbd.ResourceOps
	name      string
	sizeBytes int64
}

// NewDRBDDevice creates a DRBD device layer.
func NewDRBDDevice(ops *drbd.ResourceOps, name string, sizeBytes int64) *DRBDDevice {
	return &DRBDDevice{
		ops:       ops,
		name:      name,
		sizeBytes: sizeBytes,
	}
}

func (d *DRBDDevice) Name() string     { return "drbd:" + d.name }
func (d *DRBDDevice) Path() string     { return drbd.DevicePath(d.name) }
func (d *DRBDDevice) SizeBytes() int64 { return d.sizeBytes }

// Open brings the DRBD resource up and promotes to primary.
func (d *DRBDDevice) Open(ctx context.Context) error {
	if err := d.ops.Up(ctx); err != nil {
		return fmt.Errorf("drbd up: %w", err)
	}
	return nil
}

// Close brings the DRBD resource down.
func (d *DRBDDevice) Close(ctx context.Context) error {
	if err := d.ops.Down(ctx); err != nil {
		return fmt.Errorf("drbd down: %w", err)
	}
	return nil
}
