package nbd

import "context"

// ServerConfig configures the NBD server for a volume.
type ServerConfig struct {
	// SocketDir is the directory for Unix domain sockets.
	// Default: /run/piccolo/nbd/
	SocketDir string

	// ReadOnly sets the export to read-only mode.
	ReadOnly bool
}

// DefaultServerConfig returns the standard NBD server configuration.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		SocketDir: "/run/piccolo/nbd",
	}
}

// VolumeSession represents an active NBD session for a volume.
type VolumeSession struct {
	VolumeID   string // volume identifier
	DevicePath string // backing block device path (thin LV)
	SocketPath string // Unix socket path for this session
	NBDDevice  string // /dev/nbdN assigned to this session
	SizeBytes  int64  // export size
}

// ColdRecallHook intercepts reads on evicted ranges for PSFN cold recall.
// The no-op implementation passes through all reads unchanged.
type ColdRecallHook interface {
	// OnRead is called before serving a read. Returns true to proceed with
	// the local read, false to indicate the range was recalled from remote.
	OnRead(ctx context.Context, offset, length uint64) (proceed bool, err error)
}

// TierEvictionHook handles LRU eviction of cold ranges via BLKDISCARD.
// The no-op implementation never evicts.
type TierEvictionHook interface {
	// ShouldEvict returns ranges to evict based on current capacity pressure.
	ShouldEvict(ctx context.Context) (ranges []EvictRange, err error)
}

// EvictRange represents a byte range to discard from the thin LV.
type EvictRange struct {
	Offset uint64
	Length uint64
}

// DirtyBitmapHook tracks per-chunk dirty state for incremental sync.
// The no-op implementation performs no tracking.
type DirtyBitmapHook interface {
	// MarkDirty records that the given range has been written.
	MarkDirty(offset, length uint64)

	// DirtyRanges returns all dirty ranges since the last call to Clear.
	DirtyRanges() []DirtyRange

	// Clear resets the dirty bitmap.
	Clear()
}

// DirtyRange represents a byte range that has been modified.
type DirtyRange struct {
	Offset uint64
	Length uint64
}

// CoalescingHook aggregates writes for erasure coding.
// The no-op implementation performs no aggregation.
type CoalescingHook interface {
	// OnWrite is called after a write completes. The hook may buffer
	// the write for batched erasure coding.
	OnWrite(ctx context.Context, offset, length uint64) error
}
