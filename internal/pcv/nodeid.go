package pcv

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
)

const nodeIDLength = 16 // hex characters (8 bytes)

// DeriveNodeID produces a stable, deterministic node identifier from hardware
// signals. The result is SHA-256(machine-id || board_serial) truncated to 16
// hex characters. If DMI board_serial is unavailable (e.g. VMs without DMI),
// machine-id alone is used.
func DeriveNodeID() (string, error) {
	machineID, err := readTrimmedFile("/etc/machine-id")
	if err != nil {
		return "", fmt.Errorf("read machine-id: %w", err)
	}
	if machineID == "" {
		return "", fmt.Errorf("empty /etc/machine-id")
	}

	boardSerial, _ := readTrimmedFile("/sys/class/dmi/id/board_serial")

	h := sha256.New()
	h.Write([]byte(machineID))
	h.Write([]byte{0}) // domain separator
	h.Write([]byte(boardSerial))
	sum := h.Sum(nil)

	return fmt.Sprintf("%x", sum[:nodeIDLength/2]), nil
}

func readTrimmedFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
