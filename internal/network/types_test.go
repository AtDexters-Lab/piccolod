package network

import "testing"

func TestClassifySignal(t *testing.T) {
	tests := []struct {
		dbm  int
		want SignalTier
	}{
		{-30, SignalGood},
		{-59, SignalGood},
		{-60, SignalFair},
		{-65, SignalFair},
		{-70, SignalWeak},
		{-75, SignalWeak},
		{-80, SignalPoor},
		{-90, SignalPoor},
	}
	for _, tt := range tests {
		got := ClassifySignal(tt.dbm)
		if got != tt.want {
			t.Errorf("ClassifySignal(%d) = %s, want %s", tt.dbm, got, tt.want)
		}
	}
}
