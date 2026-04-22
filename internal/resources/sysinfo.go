package resources

import "golang.org/x/sys/unix"

// SystemRAMBytes returns the total physical system RAM in bytes, derived
// from sysinfo(2). Returns 0 on error; callers treat zero as "unknown" and
// typically skip derivation (no drop-in written, kernel defaults apply).
func SystemRAMBytes() int64 {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0
	}
	// Totalram is in sysinfo's Unit (bytes on current Linux, but checked
	// defensively per the syscall contract).
	unit := int64(info.Unit)
	if unit <= 0 {
		unit = 1
	}
	return int64(info.Totalram) * unit
}
