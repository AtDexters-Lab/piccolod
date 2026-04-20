package server

import (
	"container/list"
	"log"
	"strings"
	"sync"
	"time"
)

// loginRateLimiter is an in-memory per-username failure counter (D-12).
//
// Design choices:
//   - Per-username, not per-IP: the threat model is a password-holder attacker
//     against a legacy-only victim. The process-global Retry-After counter at
//     handleAuthLogin is insufficient — one successful login anywhere resets it.
//   - Bounded LRU capacity prevents memory exhaustion from distinct-username
//     churn. Locked-and-unexpired buckets are PINNED against eviction so that
//     churn cannot reset the lockout on a targeted victim.
//   - In-memory only: does not persist across restarts. Acceptable trade-off
//     for the single-operator deployment model — restarts also reset legitimate
//     users' lockouts.
//   - Username matching is case-insensitive + trimmed so casing variants don't
//     bypass the lockout.
type loginRateLimiter struct {
	mu       sync.Mutex
	entries  map[string]*list.Element
	order    *list.List // front = most-recently-used
	cap      int
	maxFails int
	lockout  time.Duration
}

type loginBucket struct {
	username      string
	failures      int
	lockedAt      time.Time // set when failures crosses maxFails
	lastFailureAt time.Time // refreshed on every failure; drives partial-bucket pinning
}

func newLoginRateLimiter(maxFails int, lockout time.Duration, capacity int) *loginRateLimiter {
	return &loginRateLimiter{
		entries:  make(map[string]*list.Element),
		order:    list.New(),
		cap:      capacity,
		maxFails: maxFails,
		lockout:  lockout,
	}
}

func normalizeLoginKey(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

// Allow returns true when the username is not currently locked out.
// Does not increment failures — call RecordFailure after a verify failure.
//
// While a bucket is locked, Allow promotes it to the front of the LRU on
// every check so a flood of distinct attacker-chosen usernames cannot evict
// the victim's locked bucket and reset the failure counter.
func (r *loginRateLimiter) Allow(username string) bool {
	key := normalizeLoginKey(username)
	if key == "" {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	el, ok := r.entries[key]
	if !ok {
		return true
	}
	bucket := el.Value.(*loginBucket)
	if bucket.failures < r.maxFails {
		return true
	}
	if time.Since(bucket.lockedAt) >= r.lockout {
		r.removeLocked(el, key)
		return true
	}
	// Pin the locked bucket near the head so distinct-username churn cannot
	// evict it and reset the lockout.
	r.order.MoveToFront(el)
	return false
}

// RecordFailure increments the failure counter and records the lockout time
// when the threshold is crossed. When the limiter is already at 2× capacity
// with every bucket pinned (lockouts + recent partials), new failures for
// previously-unseen usernames are dropped — the alternative is unbounded
// memory growth under a distinct-username flood.
func (r *loginRateLimiter) RecordFailure(username string) {
	key := normalizeLoginKey(username)
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	el, ok := r.entries[key]
	if !ok {
		// Try to reclaim an expired bucket before falling back to the hard
		// ceiling. Without this, after a churn burst pushes us to 2×cap, new
		// users' failures stay unrecorded until an existing key happens to
		// revisit and drain expired neighbors.
		if r.order.Len() >= 2*r.cap {
			r.evictLocked()
		}
		if r.order.Len() >= 2*r.cap {
			log.Printf("WARN: login rate-limiter at 2x capacity; dropping new failure record (churn-attack mitigation) key=%s", key)
			return
		}
		bucket := &loginBucket{username: key, failures: 1, lastFailureAt: now}
		el = r.order.PushFront(bucket)
		r.entries[key] = el
		r.evictLocked()
		return
	}
	bucket := el.Value.(*loginBucket)
	// Age out a stale partial: if the bucket wasn't locked and the last
	// failure is beyond the lockout window, the user has had ample time to
	// recover — treat the next failure as the first of a new sequence.
	// Without this, 9 old failures + a single fresh typo would cross the
	// threshold and lock out a legitimate user.
	if bucket.failures < r.maxFails && now.Sub(bucket.lastFailureAt) >= r.lockout {
		bucket.failures = 0
	}
	bucket.failures++
	bucket.lastFailureAt = now
	if bucket.failures >= r.maxFails && bucket.lockedAt.IsZero() {
		bucket.lockedAt = now
	}
	r.order.MoveToFront(el)
}

// RecordSuccess clears the bucket for the username on successful login.
// Does not affect other usernames' counters.
func (r *loginRateLimiter) RecordSuccess(username string) {
	key := normalizeLoginKey(username)
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if el, ok := r.entries[key]; ok {
		r.removeLocked(el, key)
	}
}

func (r *loginRateLimiter) removeLocked(el *list.Element, key string) {
	r.order.Remove(el)
	delete(r.entries, key)
}

// evictLocked enforces the LRU cap, but preserves two classes of buckets:
//
//   (a) Fully-locked buckets whose lockout hasn't expired — evicting one
//       resets an in-flight 15-minute lockout the attacker already earned.
//   (b) Partial-failure buckets whose lastFailureAt is within the lockout
//       window — without this, an attacker makes maxFails-1 failures for a
//       victim, floods >cap distinct usernames to evict the victim's bucket
//       (partial bucket sits near the tail after each attacker-username
//       pushes to the front), then restarts the victim's counter. The
//       partial-bucket pin forces the attacker to wait out lockout before
//       the victim's bucket becomes evictable.
//
// When EVERY bucket is pinned, we let the map grow past cap but cap it at 2×
// via RecordFailure's absolute-ceiling check. A WARN surfaces the condition.
// Caller must hold r.mu.
func (r *loginRateLimiter) evictLocked() {
	now := time.Now()
	for r.order.Len() > r.cap {
		// Scan from the tail for the first non-pinned entry.
		var target *list.Element
		for el := r.order.Back(); el != nil; el = el.Prev() {
			b := el.Value.(*loginBucket)
			lockedAndFresh := b.failures >= r.maxFails && now.Sub(b.lockedAt) < r.lockout
			partialAndFresh := b.failures > 0 && b.failures < r.maxFails && now.Sub(b.lastFailureAt) < r.lockout
			if lockedAndFresh || partialAndFresh {
				continue
			}
			target = el
			break
		}
		if target == nil {
			// Every entry is a pinned lockout or recent-partial. Let the map
			// grow past cap (bounded at 2× by RecordFailure's ceiling). The
			// oldest pins will expire within r.lockout and be evicted later.
			log.Printf("WARN: login rate-limiter at capacity with all %d buckets pinned; skipping eviction", r.order.Len())
			return
		}
		bucket := target.Value.(*loginBucket)
		r.removeLocked(target, bucket.username)
	}
}
