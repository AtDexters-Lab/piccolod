package blockdev

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// BlockDevice represents a single layer in the block device stack.
// Each layer wraps a backing device and exposes a new device path.
type BlockDevice interface {
	// Name returns a human-readable identifier (e.g., "thinlv:vol-abc", "nbd:vol-abc").
	Name() string

	// Path returns the device path (e.g., /dev/piccolo-data-vg/vol-abc, /dev/nbd0).
	Path() string

	// Open activates the device. Must be called before I/O.
	Open(ctx context.Context) error

	// Close deactivates the device. Must be called in reverse stack order.
	Close(ctx context.Context) error

	// SizeBytes returns the device capacity.
	SizeBytes() int64
}

// DeviceStack composes block device layers into a pipeline.
// Open activates bottom-up; Close deactivates top-down.
// The stack is always: [ThinLVDevice, NBDDevice, DRBDDevice].
type DeviceStack struct {
	volumeID string
	layers   []BlockDevice
}

// NewDeviceStack creates a stack from the given layers (bottom to top).
// At least one layer is required.
func NewDeviceStack(volumeID string, layers ...BlockDevice) (*DeviceStack, error) {
	if len(layers) == 0 {
		return nil, fmt.Errorf("device stack requires at least one layer")
	}
	return &DeviceStack{
		volumeID: volumeID,
		layers:   layers,
	}, nil
}

// VolumeID returns the volume this stack belongs to.
func (s *DeviceStack) VolumeID() string {
	return s.volumeID
}

// Top returns the topmost device — what LUKS/ext4 operates on.
func (s *DeviceStack) Top() BlockDevice {
	return s.layers[len(s.layers)-1]
}

// Bottom returns the bottommost device (the thin LV).
func (s *DeviceStack) Bottom() BlockDevice {
	return s.layers[0]
}

// Open activates all layers bottom-up. If any layer fails,
// already-opened layers are closed in reverse order using a fresh
// background context (so rollback isn't blocked by the original ctx's cancellation).
func (s *DeviceStack) Open(ctx context.Context) error {
	for i, layer := range s.layers {
		if err := layer.Open(ctx); err != nil {
			// Rollback with a fresh context — the original may be cancelled.
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			for j := i - 1; j >= 0; j-- {
				_ = s.layers[j].Close(rollbackCtx)
			}
			cancel()
			return fmt.Errorf("open %s: %w", layer.Name(), err)
		}
	}
	return nil
}

// Close deactivates all layers top-down. Continues closing remaining
// layers even on error to avoid leaked devices. Returns all errors joined.
func (s *DeviceStack) Close(ctx context.Context) error {
	var errs []error
	for i := len(s.layers) - 1; i >= 0; i-- {
		if err := s.layers[i].Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", s.layers[i].Name(), err))
		}
	}
	return errors.Join(errs...)
}

// Layers returns the ordered layers (bottom to top) for inspection.
func (s *DeviceStack) Layers() []BlockDevice {
	out := make([]BlockDevice, len(s.layers))
	copy(out, s.layers)
	return out
}
