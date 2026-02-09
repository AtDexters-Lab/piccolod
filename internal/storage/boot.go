package storage

import (
	"context"
	"fmt"
	"os"
	"strings"

	"piccolod/internal/runner"
)

// BootMode identifies how the system was booted.
type BootMode string

const (
	BootModeInternal BootMode = "internal" // Booted from internal disk (sata, nvme, ata, mmc)
	BootModeUSB      BootMode = "usb"      // Booted from USB drive
	BootModeUnknown  BootMode = "unknown"  // Ambiguous transport (virtio, iSCSI, some RAID)
)

// testImageSentinel gates the PICCOLO_BOOT_MODE_OVERRIDE env var.
var testImageSentinel = "/etc/piccolo-test-image"

// setTestImageSentinelForTest overrides the sentinel path for testing.
func setTestImageSentinelForTest(path string) { testImageSentinel = path }

// DetectBootMode determines boot mode by inspecting the root device's transport
// type via lsblk. A CI override is available for test images.
func DetectBootMode(ctx context.Context, run runner.CommandRunner) (BootMode, error) {
	// CI/QA override — only honored on test/CI images.
	if override := os.Getenv("PICCOLO_BOOT_MODE_OVERRIDE"); override != "" {
		if _, err := os.Stat(testImageSentinel); err != nil {
			return "", fmt.Errorf("PICCOLO_BOOT_MODE_OVERRIDE is set but %s does not exist; "+
				"this override is only supported on test/CI images", testImageSentinel)
		}
		switch BootMode(override) {
		case BootModeInternal, BootModeUSB, BootModeUnknown:
			return BootMode(override), nil
		default:
			return "", fmt.Errorf("invalid PICCOLO_BOOT_MODE_OVERRIDE value: %q", override)
		}
	}

	rootDev, err := getRootDevice(ctx, run)
	if err != nil {
		return "", fmt.Errorf("detect root device: %w", err)
	}

	disk := getParentDisk(rootDev)
	transport, err := getTransportType(ctx, run, disk)
	if err != nil {
		return "", fmt.Errorf("detect transport: %w", err)
	}

	return classifyTransport(transport), nil
}

// classifyTransport maps a lsblk TRAN value to BootMode.
func classifyTransport(transport string) BootMode {
	switch strings.ToLower(transport) {
	case "usb":
		return BootModeUSB
	case "sata", "nvme", "ata", "mmc":
		return BootModeInternal
	default:
		return BootModeUnknown
	}
}

// getTransportType runs lsblk -ndo TRAN on a disk device.
func getTransportType(ctx context.Context, run runner.CommandRunner, disk string) (string, error) {
	out, err := run.RunWithOutput(ctx, "lsblk", "-ndo", "TRAN", disk)
	if err != nil {
		return "", fmt.Errorf("lsblk TRAN: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
