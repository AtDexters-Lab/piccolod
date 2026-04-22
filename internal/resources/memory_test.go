package resources

import (
	"testing"

	"piccolod/internal/api"
)

func TestDeriveMemoryKnobs(t *testing.T) {
	const (
		mib = int64(1024 * 1024)
		gib = int64(1024 * 1024 * 1024)
	)

	tests := []struct {
		name             string
		minRequired      int64
		profile          api.ResourceProfile
		systemRAM        int64
		activeElasticCnt int
		wantHigh         int64 // 0 = skip assertion
		wantMax          int64
		wantHighLeMax    bool
	}{
		{
			name:        "zero min_required yields zero knobs",
			minRequired: 0, profile: api.ProfileBounded, systemRAM: 8 * gib,
			activeElasticCnt: 1,
			wantHigh:         0, wantMax: 0,
		},
		{
			name:        "zero systemRAM yields zero knobs",
			minRequired: 4 * gib, profile: api.ProfileElastic, systemRAM: 0,
			activeElasticCnt: 1,
			wantHigh:         0, wantMax: 0,
		},
		{
			name:        "bounded on 8GB: typical",
			minRequired: 512 * mib, profile: api.ProfileBounded, systemRAM: 8 * gib,
			activeElasticCnt: 0, // tests floor-to-1 for bounded (not used but shouldn't matter)
			wantHigh:         512 * mib * 125 / 100,
			wantMax:          1024 * mib,
			wantHighLeMax:    true,
		},
		{
			name:        "bounded clamp: min_required × 2 exceeds slice_ceiling_cap on 4 GB box",
			minRequired: 4 * gib, profile: api.ProfileBounded, systemRAM: 4 * gib,
			activeElasticCnt: 1,
			// usable_RAM = 3.2 GB, slice_ceiling_cap = 3.2 - 0.256 GB ≈ 2.94 GB
			// bounded would want high=5 GB, max=8 GB — both get clamped.
			wantHighLeMax: true,
		},
		{
			name:        "elastic on 8GB single app: generous headroom",
			minRequired: 6 * gib, profile: api.ProfileElastic, systemRAM: 8 * gib,
			activeElasticCnt: 1,
			// usable_RAM = 6.4 GB, slice_ceiling_cap ≈ 6.14 GB
			// Hard clamp: MemoryMax ≤ 6.14 GB even though min_required × 1.5 = 9 GB.
			wantHighLeMax: true,
		},
		{
			name:        "elastic with three active apps: share shrinks",
			minRequired: 1 * gib, profile: api.ProfileElastic, systemRAM: 8 * gib,
			activeElasticCnt: 3,
			// usable_RAM × 0.5 / 3 ≈ 1.07 GB; min × 1.25 = 1.25 GB wins.
			wantHighLeMax: true,
		},
		{
			name:        "divide-by-zero safety: activeElasticCnt = 0 floors to 1",
			minRequired: 512 * mib, profile: api.ProfileElastic, systemRAM: 8 * gib,
			activeElasticCnt: 0,
			wantHighLeMax:    true,
		},
		{
			name:        "degenerate systemRAM (below headroom): zero knobs",
			minRequired: 1 * gib, profile: api.ProfileBounded, systemRAM: 100 * mib,
			activeElasticCnt: 1,
			wantHigh:         0, wantMax: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveMemoryKnobs(tc.minRequired, tc.profile, tc.systemRAM, tc.activeElasticCnt)

			if tc.wantHigh != 0 && got.HighBytes != tc.wantHigh {
				t.Errorf("HighBytes: want %d, got %d", tc.wantHigh, got.HighBytes)
			}
			if tc.wantMax != 0 && got.MaxBytes != tc.wantMax {
				t.Errorf("MaxBytes: want %d, got %d", tc.wantMax, got.MaxBytes)
			}
			if tc.wantHigh == 0 && tc.wantMax == 0 && (got.HighBytes != 0 || got.MaxBytes != 0) {
				// expected zero knobs
				if tc.name == "bounded clamp: min_required × 2 exceeds slice_ceiling_cap on 4 GB box" ||
					tc.name == "elastic on 8GB single app: generous headroom" ||
					tc.name == "elastic with three active apps: share shrinks" ||
					tc.name == "divide-by-zero safety: activeElasticCnt = 0 floors to 1" ||
					tc.name == "bounded on 8GB: typical" {
					// these have wantHighLeMax only; skip zero-knobs check
				} else {
					t.Errorf("expected zero knobs, got HighBytes=%d MaxBytes=%d", got.HighBytes, got.MaxBytes)
				}
			}
			if tc.wantHighLeMax && got.HighBytes > got.MaxBytes {
				t.Errorf("High must be ≤ Max: HighBytes=%d, MaxBytes=%d", got.HighBytes, got.MaxBytes)
			}
		})
	}
}

func TestDeriveMemoryKnobs_ClampInvariant(t *testing.T) {
	// Invariant: MemoryMax ≤ usable_RAM − SliceCeilingHeadroom across all
	// realistic inputs. This is the load-bearing safety claim.
	gib := int64(1024 * 1024 * 1024)

	scenarios := []struct {
		minRequired int64
		profile     api.ResourceProfile
		systemRAM   int64
		active      int
	}{
		{4 * gib, api.ProfileBounded, 4 * gib, 1},
		{6 * gib, api.ProfileElastic, 4 * gib, 1},
		{6 * gib, api.ProfileElastic, 8 * gib, 1},
		{100 * gib, api.ProfileElastic, 4 * gib, 1}, // pathological over-declaration
		{10 * gib, api.ProfileBounded, 8 * gib, 3},
	}

	for _, s := range scenarios {
		knobs := DeriveMemoryKnobs(s.minRequired, s.profile, s.systemRAM, s.active)
		usable := int64(float64(s.systemRAM) * UsableRAMRatio)
		cap := usable - SliceCeilingHeadroom
		if cap <= 0 {
			continue
		}
		if knobs.MaxBytes > cap {
			t.Errorf("MaxBytes %d exceeds slice_ceiling_cap %d for scenario %+v",
				knobs.MaxBytes, cap, s)
		}
		if knobs.HighBytes > knobs.MaxBytes {
			t.Errorf("HighBytes %d > MaxBytes %d for scenario %+v",
				knobs.HighBytes, knobs.MaxBytes, s)
		}
	}
}
