package cryptoutil

import "syscall"

const (
	minKDFMemoryKiB = 128 * 1024 // 128 MiB floor
	maxKDFMemoryKiB = 512 * 1024 // 512 MiB ceiling
)

// SelectKDFMemoryKiB returns the Argon2id memory parameter (in KiB)
// capped at totalRAM/8, with a floor of 128 MiB and ceiling of 512 MiB.
// This prevents OOM kills on memory-constrained devices while maintaining
// strong KDF parameters on well-provisioned systems.
func SelectKDFMemoryKiB() uint32 {
	return selectKDFMemoryForRAM(totalRAMKiB())
}

// selectKDFMemoryForRAM is the pure, testable core.
func selectKDFMemoryForRAM(totalKiB uint64) uint32 {
	cap := uint32(totalKiB / 8)
	if cap < minKDFMemoryKiB {
		cap = minKDFMemoryKiB
	}
	if cap > maxKDFMemoryKiB {
		cap = maxKDFMemoryKiB
	}
	return cap
}

func totalRAMKiB() uint64 {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err != nil {
		return 4 * 1024 * 1024 // assume 4 GiB → returns 512 MiB ceiling
	}
	return uint64(info.Totalram) * uint64(info.Unit) / 1024
}
