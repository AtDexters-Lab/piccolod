package resources

import (
	"testing"

	"piccolod/internal/api"
)

func TestPriorityToCPUWeight(t *testing.T) {
	tests := []struct {
		priority api.ResourcePriority
		want     int
	}{
		{api.PriorityHigh, CPUWeightHigh},
		{api.PriorityNormal, CPUWeightNormal},
		{api.PriorityBackground, CPUWeightBackground},
		{"", CPUWeightNormal},      // unset → normal
		{"garbage", CPUWeightNormal}, // unknown → normal (defensive)
	}
	for _, tc := range tests {
		got := PriorityToCPUWeight(tc.priority)
		if got != tc.want {
			t.Errorf("PriorityToCPUWeight(%q) = %d, want %d", tc.priority, got, tc.want)
		}
	}

	// Sanity: high:background ratio is 16:1 (compressed), not 100:1
	ratio := CPUWeightHigh / CPUWeightBackground
	if ratio != 16 {
		t.Errorf("high:background ratio = %d, expected 16 (compressed for small-core hardware)", ratio)
	}
}
