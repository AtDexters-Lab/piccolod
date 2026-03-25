package cryptoutil

import (
	"encoding/base64"
	"testing"
)

func TestGenerateSecureToken(t *testing.T) {
	t.Run("correct_byte_length", func(t *testing.T) {
		for _, length := range []int{16, 32, 64} {
			tok, err := GenerateSecureToken(length)
			if err != nil {
				t.Fatalf("length=%d: %v", length, err)
			}
			raw, err := base64.RawURLEncoding.DecodeString(tok)
			if err != nil {
				t.Fatalf("length=%d: decode error: %v", length, err)
			}
			if len(raw) != length {
				t.Errorf("length=%d: got %d bytes, want %d", length, len(raw), length)
			}
		}
	})

	t.Run("url_safe_encoding", func(t *testing.T) {
		tok, _ := GenerateSecureToken(32)
		// RawURLEncoding uses - and _ instead of + and /, no padding
		for _, c := range tok {
			if c == '+' || c == '/' || c == '=' {
				t.Errorf("token contains non-URL-safe character: %c", c)
			}
		}
	})

	t.Run("uniqueness", func(t *testing.T) {
		a, _ := GenerateSecureToken(32)
		b, _ := GenerateSecureToken(32)
		if a == b {
			t.Error("two tokens should not be equal")
		}
	})
}
