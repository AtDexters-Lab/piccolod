package resources

import "piccolod/internal/api"

// UsableRAMRatio is the fraction of system RAM considered usable by apps.
// The remainder (20%) is reserved for kernel, piccolod, systemd, and OS
// overhead. See plans/resource-stewardship.md D-1.
const UsableRAMRatio = 0.8

// SliceCeilingHeadroom is the byte reserve below usable_RAM that bounds
// any slice's MemoryMax. Keeps a minimum breathing room for the host
// between the slice ceiling and the physically-available usable RAM.
const SliceCeilingHeadroom int64 = 256 * 1024 * 1024 // 256 MiB

// MemoryKnobs is the derived memory policy for an app slice. Zero values
// mean "no drop-in" (kernel defaults apply).
type MemoryKnobs struct {
	HighBytes int64
	MaxBytes  int64
}

// DeriveMemoryKnobs computes MemoryHigh and MemoryMax for an app slice.
// The derivation honors D-1 in plans/resource-stewardship.md:
//
//   - `bounded`: MemoryHigh = min_required × 1.25, MemoryMax = min_required × 2,
//     both clamped to slice_ceiling_cap.
//   - `elastic`: MemoryHigh = max(min_required × 1.25, usable_RAM × 0.5 / N),
//     MemoryMax = max(min_required × 1.5, usable_RAM × 0.7),
//     both clamped to slice_ceiling_cap.
//
// slice_ceiling_cap = usable_RAM − 256 MiB. MemoryMax cannot exceed it,
// regardless of min_required — this is the load-bearing safety invariant
// that keeps the kernel's slice-scoped reclaim the first line of defence.
//
// activeElasticCount is the number of running elastic apps *including* the
// one being provisioned (so the formula is stable across "about-to-start"
// and "now-running" transitions). Values < 1 are floored to 1.
//
// Returns zero MemoryKnobs if min_required ≤ 0, system_RAM ≤ 0, or the
// derived slice_ceiling_cap is degenerate (< 0). Callers interpret zero
// as "do not write a drop-in; kernel defaults apply."
func DeriveMemoryKnobs(minRequiredBytes int64, profile api.ResourceProfile, systemRAMBytes int64, activeElasticCount int) MemoryKnobs {
	if minRequiredBytes <= 0 || systemRAMBytes <= 0 {
		return MemoryKnobs{}
	}

	usableRAM := int64(float64(systemRAMBytes) * UsableRAMRatio)
	sliceCeilingCap := usableRAM - SliceCeilingHeadroom
	if sliceCeilingCap <= 0 {
		return MemoryKnobs{}
	}

	if activeElasticCount < 1 {
		activeElasticCount = 1
	}

	var high, max int64
	switch profile {
	case api.ProfileElastic:
		elasticShare := usableRAM / 2 / int64(activeElasticCount)
		high = maxInt64(minRequiredBytes*125/100, elasticShare)
		max = maxInt64(minRequiredBytes*3/2, usableRAM*7/10)
	default: // ProfileBounded and zero-value
		high = minRequiredBytes * 125 / 100
		max = minRequiredBytes * 2
	}

	// Hard clamp: MemoryMax ≤ slice_ceiling_cap regardless of min_required.
	// See D-1 invariant and D-11 supply-chain note.
	if max > sliceCeilingCap {
		max = sliceCeilingCap
	}
	// MemoryHigh stays strictly below MemoryMax so reclaim has room.
	highCap := sliceCeilingCap * 9 / 10
	if high > highCap {
		high = highCap
	}
	if high > max {
		high = max
	}

	return MemoryKnobs{HighBytes: high, MaxBytes: max}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
