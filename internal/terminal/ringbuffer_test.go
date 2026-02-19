package terminal

import (
	"bytes"
	"testing"
)

func TestRingBuffer_BasicWrite(t *testing.T) {
	rb := NewRingBuffer(10)
	rb.Write([]byte("hello"))
	snap := rb.Snapshot()
	if !bytes.Equal(snap, []byte("hello")) {
		t.Fatalf("expected %q, got %q", "hello", snap)
	}
}

func TestRingBuffer_ExactFill(t *testing.T) {
	rb := NewRingBuffer(5)
	rb.Write([]byte("abcde"))
	snap := rb.Snapshot()
	if !bytes.Equal(snap, []byte("abcde")) {
		t.Fatalf("expected %q, got %q", "abcde", snap)
	}
}

func TestRingBuffer_Wrap(t *testing.T) {
	rb := NewRingBuffer(5)
	rb.Write([]byte("abc"))
	rb.Write([]byte("defg"))
	// Buffer should contain "cdefg" (oldest 'a','b' overwritten)
	snap := rb.Snapshot()
	if !bytes.Equal(snap, []byte("cdefg")) {
		t.Fatalf("expected %q, got %q", "cdefg", snap)
	}
}

func TestRingBuffer_OverwriteMultipleWraps(t *testing.T) {
	rb := NewRingBuffer(4)
	rb.Write([]byte("ab"))
	rb.Write([]byte("cd"))
	rb.Write([]byte("ef"))
	// Buffer should contain "cdef"
	snap := rb.Snapshot()
	if !bytes.Equal(snap, []byte("cdef")) {
		t.Fatalf("expected %q, got %q", "cdef", snap)
	}
}

func TestRingBuffer_WriteLargerThanBuffer(t *testing.T) {
	rb := NewRingBuffer(4)
	rb.Write([]byte("abcdefgh"))
	snap := rb.Snapshot()
	if !bytes.Equal(snap, []byte("efgh")) {
		t.Fatalf("expected %q, got %q", "efgh", snap)
	}
}

func TestRingBuffer_Reset(t *testing.T) {
	rb := NewRingBuffer(10)
	rb.Write([]byte("hello"))
	rb.Reset()
	snap := rb.Snapshot()
	if len(snap) != 0 {
		t.Fatalf("expected empty snapshot after reset, got %q", snap)
	}
}

func TestRingBuffer_Empty(t *testing.T) {
	rb := NewRingBuffer(10)
	snap := rb.Snapshot()
	if len(snap) != 0 {
		t.Fatalf("expected empty snapshot, got %q", snap)
	}
}

func TestRingBuffer_SingleByte(t *testing.T) {
	rb := NewRingBuffer(1)
	rb.Write([]byte("a"))
	rb.Write([]byte("b"))
	snap := rb.Snapshot()
	if !bytes.Equal(snap, []byte("b")) {
		t.Fatalf("expected %q, got %q", "b", snap)
	}
}
