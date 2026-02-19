package terminal

import "sync"

// RingBuffer is a fixed-size circular byte buffer used to store PTY scrollback.
// It implements io.Writer and overwrites the oldest data when full.
type RingBuffer struct {
	mu   sync.Mutex
	buf  []byte
	size int
	pos  int
	full bool
}

// NewRingBuffer creates a ring buffer with the given capacity in bytes.
func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{
		buf:  make([]byte, size),
		size: size,
	}
}

// Write appends data to the buffer, overwriting oldest bytes on wrap.
// It is safe for concurrent use.
func (r *RingBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	if n >= r.size {
		// Data larger than buffer: keep only the last r.size bytes
		copy(r.buf, p[n-r.size:])
		r.pos = 0
		r.full = true
		return n, nil
	}
	// How much fits before wrapping?
	remaining := r.size - r.pos
	if n <= remaining {
		copy(r.buf[r.pos:], p)
		r.pos += n
		if r.pos == r.size {
			r.pos = 0
			r.full = true
		}
	} else {
		copy(r.buf[r.pos:], p[:remaining])
		copy(r.buf, p[remaining:])
		r.pos = n - remaining
		r.full = true
	}
	return n, nil
}

// Snapshot returns a chronological copy of all buffered data.
// The caller must hold no external locks that conflict with r.mu.
func (r *RingBuffer) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		out := make([]byte, r.pos)
		copy(out, r.buf[:r.pos])
		return out
	}
	out := make([]byte, r.size)
	// Oldest data starts at r.pos, wraps around
	n := copy(out, r.buf[r.pos:])
	copy(out[n:], r.buf[:r.pos])
	return out
}

// Reset clears the buffer.
func (r *RingBuffer) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pos = 0
	r.full = false
}
