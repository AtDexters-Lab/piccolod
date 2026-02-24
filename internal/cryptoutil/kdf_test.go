package cryptoutil

import "testing"

func TestSelectKDFMemoryForRAM(t *testing.T) {
	tests := []struct {
		name     string
		totalKiB uint64
		want     uint32
	}{
		{"512MB system → floor 128 MiB", 512 * 1024, 128 * 1024},
		{"1 GiB system → floor 128 MiB", 1024 * 1024, 128 * 1024},
		{"2 GiB system → 256 MiB", 2 * 1024 * 1024, 256 * 1024},
		{"4 GiB system → ceiling 512 MiB", 4 * 1024 * 1024, 512 * 1024},
		{"8 GiB system → ceiling 512 MiB", 8 * 1024 * 1024, 512 * 1024},
		{"16 GiB system → ceiling 512 MiB", 16 * 1024 * 1024, 512 * 1024},
		{"tiny system → floor 128 MiB", 256 * 1024, 128 * 1024},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectKDFMemoryForRAM(tt.totalKiB)
			if got != tt.want {
				t.Errorf("selectKDFMemoryForRAM(%d) = %d, want %d", tt.totalKiB, got, tt.want)
			}
		})
	}
}

func TestSelectKDFMemoryKiB_Live(t *testing.T) {
	// Smoke test: just ensure it returns a value within bounds.
	got := SelectKDFMemoryKiB()
	if got < minKDFMemoryKiB || got > maxKDFMemoryKiB {
		t.Errorf("SelectKDFMemoryKiB() = %d, want [%d, %d]", got, minKDFMemoryKiB, maxKDFMemoryKiB)
	}
}
