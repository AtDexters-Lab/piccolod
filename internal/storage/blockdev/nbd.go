package blockdev

import (
	"context"
	"fmt"

	"piccolod/internal/storage/nbd"
)

// NBDDevice wraps an NBD session as a BlockDevice.
// This is the middle layer of the device stack.
type NBDDevice struct {
	server    *nbd.Server
	volumeID  string
	sizeBytes int64
	hooks     nbd.Hooks

	// backingPath is set by the Open call to the path of the layer below.
	backingPath string
	session     *nbd.VolumeSession
}

// NewNBDDevice creates an NBD device layer.
// backingPath is the device path from the layer below (thin LV).
func NewNBDDevice(server *nbd.Server, volumeID, backingPath string, sizeBytes int64, hooks nbd.Hooks) *NBDDevice {
	return &NBDDevice{
		server:      server,
		volumeID:    volumeID,
		backingPath: backingPath,
		sizeBytes:   sizeBytes,
		hooks:       hooks,
	}
}

func (d *NBDDevice) Name() string { return "nbd:" + d.volumeID }

// Path returns the /dev/nbdN device path. Only valid after Open.
func (d *NBDDevice) Path() string {
	if d.session != nil {
		return d.session.NBDDevice
	}
	return ""
}

func (d *NBDDevice) SizeBytes() int64 { return d.sizeBytes }

// Open starts the NBD server session and connects the kernel client.
func (d *NBDDevice) Open(ctx context.Context) error {
	sess, err := d.server.StartSession(ctx, d.volumeID, d.backingPath, d.sizeBytes, d.hooks)
	if err != nil {
		return fmt.Errorf("start nbd session: %w", err)
	}
	d.session = sess
	return nil
}

// Close stops the NBD session.
func (d *NBDDevice) Close(ctx context.Context) error {
	if err := d.server.StopSession(ctx, d.volumeID); err != nil {
		return fmt.Errorf("stop nbd session: %w", err)
	}
	d.session = nil
	return nil
}
