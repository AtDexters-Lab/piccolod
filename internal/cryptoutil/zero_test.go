package cryptoutil

import "testing"

func TestSecureZero(t *testing.T) {
	buf := []byte{1, 2, 3, 4, 5}
	SecureZero(buf)
	for i, b := range buf {
		if b != 0 {
			t.Fatalf("byte %d not zeroed: %d", i, b)
		}
	}
}

func TestSecureZero_Empty(t *testing.T) {
	// Should not panic on empty slice
	SecureZero(nil)
	SecureZero([]byte{})
}
