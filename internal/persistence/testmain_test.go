package persistence

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Tests use fake mount implementations that don't create real mount points.
	// Disable mountpoint immutability in this package so tests can safely write
	// marker files inside the mount directory.
	_ = os.Setenv("PICCOLO_DISABLE_MOUNTPOINT_GUARD", "1")
	os.Exit(m.Run())
}
