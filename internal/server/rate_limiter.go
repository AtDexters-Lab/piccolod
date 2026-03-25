package server

import (
	"sync"
	"time"
)

// ipRateLimiter tracks failures per IP and blocks IPs that exceed the budget.
// Only failures are counted — successful requests do not consume budget.
type ipRateLimiter struct {
	mu          sync.Mutex
	buckets     map[string]*rateBucket
	limit       int
	window      time.Duration
	lastCleanup time.Time
}

type rateBucket struct {
	failures int
	resetAt  time.Time
}

func newIPRateLimiter(limit int, window time.Duration) *ipRateLimiter {
	return &ipRateLimiter{
		buckets: make(map[string]*rateBucket),
		limit:   limit,
		window:  window,
	}
}

// Allow checks whether the IP is within the failure budget.
// Does NOT increment the counter — call RecordFailure after a failed operation.
func (r *ipRateLimiter) Allow(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()

	if len(r.buckets) > 100 && now.Sub(r.lastCleanup) > time.Minute {
		for k, v := range r.buckets {
			if now.After(v.resetAt) {
				delete(r.buckets, k)
			}
		}
		r.lastCleanup = now
	}

	bucket, ok := r.buckets[ip]
	if !ok || now.After(bucket.resetAt) {
		return true
	}

	return bucket.failures < r.limit
}

// RecordFailure increments the failure counter for the given IP.
func (r *ipRateLimiter) RecordFailure(ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	bucket, ok := r.buckets[ip]
	if !ok || now.After(bucket.resetAt) {
		r.buckets[ip] = &rateBucket{failures: 1, resetAt: now.Add(r.window)}
		return
	}
	bucket.failures++
}
