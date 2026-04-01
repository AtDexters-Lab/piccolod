package captive

import (
	"sync"
	"time"
)

// rateLimiter tracks per-IP submission counts within a sliding window.
// Unlike the existing server rate limiter (failure-only), this counts all
// submissions (the captive portal rate-limits all attempts, not just failures).
type rateLimiter struct {
	mu       sync.Mutex
	window   time.Duration
	limit    int
	attempts map[string][]time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		window:   window,
		limit:    limit,
		attempts: make(map[string][]time.Time),
	}
}

// allow returns true if the IP has remaining budget.
func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.prune(ip)
	return len(rl.attempts[ip]) < rl.limit
}

// record adds a timestamp for the IP.
func (rl *rateLimiter) record(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.attempts[ip] = append(rl.attempts[ip], time.Now())
}

// prune removes expired entries for an IP. Caller must hold mu.
func (rl *rateLimiter) prune(ip string) {
	cutoff := time.Now().Add(-rl.window)
	entries := rl.attempts[ip]
	n := 0
	for _, t := range entries {
		if t.After(cutoff) {
			entries[n] = t
			n++
		}
	}
	if n == 0 {
		delete(rl.attempts, ip)
	} else {
		rl.attempts[ip] = entries[:n]
	}
}
