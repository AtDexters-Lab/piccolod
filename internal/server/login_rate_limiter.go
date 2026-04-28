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
	inflight      int       // verifies admitted by Allow but not yet finished — reserves remaining lockout capacity so a concurrent burst can't race past maxFails
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

// noopRelease is the release callback returned when Allow does not
// reserve inflight capacity (unseen username, lockout in progress, or
// at capacity). Calling it is a no-op so handlers can defer the
// release unconditionally.
var noopRelease = func() {}

// Allow returns whether the username is admitted for a verify attempt
// AND a single-use release callback the caller MUST invoke once the
// verify completes (typically `defer release()` after the Allow returns
// true). Returning a callback rather than relying on RecordFailure /
// RecordSuccess closes the early-return paths in the auth handler
// (e.g. ErrLocked) where neither record-method runs.
//
// When a bucket exists for the username, Allow reserves capacity from
// the lockout budget by incrementing bucket.inflight. The combined
// `failures + inflight` is the count the lockout threshold compares
// against, so N concurrent attempts for the same username can no
// longer all pass while the threshold is below maxFails — at most
// `maxFails - failures` reservations are admitted before subsequent
// callers see false. This closes the codex-flagged TOCTOU where the
// stored failures counter only sees increments AFTER each verify
// completes, leaving a window where parallel Argon2id verifies could
// race past the lockout (PK-3 from the passkey-cohesive-fix deferred
// memo).
//
// Per-callback ownership of the inflight slot is critical: each Allow
// returns a sync.Once-guarded callback that decrements EXACTLY ONCE.
// RecordFailure / RecordSuccess do not touch inflight — they own the
// failures counter and bucket lifetime respectively, while inflight is
// owned exclusively by the Allow→release pair. Without this separation
// (a counter-based release that decrements "any inflight"), a deferred
// release after RecordFailure would silently consume a different
// goroutine's reservation under contention, partially restoring the
// pre-fix wall-clock parallelism.
//
// The reservation is bypassed when the bucket does not yet exist
// (unseen username, lockout-expired): creating a bucket on every Allow
// would re-introduce the unbounded-memory-growth attack the LRU cap is
// designed to prevent. The first burst on a fresh bucket therefore
// retains the pre-fix behavior; subsequent bursts within the lockout
// window are strictly serialized by the inflight reservation.
//
// While a bucket is locked, Allow promotes it to the front of the LRU
// on every check so a flood of distinct attacker-chosen usernames
// cannot evict the victim's locked bucket and reset the failure counter.
func (r *loginRateLimiter) Allow(username string) (allowed bool, release func()) {
	key := normalizeLoginKey(username)
	if key == "" {
		return true, noopRelease
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	el, ok := r.entries[key]
	if !ok {
		return true, noopRelease
	}
	bucket := el.Value.(*loginBucket)
	// Age out stale partials before admission: a bucket with K<maxFails
	// failures whose last failure is older than lockout has effectively
	// recovered, so a fresh sequence starts at 0. Without this, 9 stale
	// failures would let one concurrent Allow reserve and the next get
	// 429 (failures+inflight=10) even though the partial window expired.
	// RecordFailure already does the same age-out when it sees a fresh
	// failure; this just runs the check at the admission gate too.
	if bucket.failures < r.maxFails && time.Since(bucket.lastFailureAt) >= r.lockout {
		bucket.failures = 0
	}
	if bucket.failures+bucket.inflight < r.maxFails {
		bucket.inflight++
		return true, r.releaseFn(key, bucket)
	}
	if bucket.failures >= r.maxFails && time.Since(bucket.lockedAt) >= r.lockout {
		r.removeLocked(el, key)
		return true, noopRelease
	}
	// Locked or at capacity. Pin near the head so distinct-username churn
	// cannot evict the bucket and reset the lockout.
	r.order.MoveToFront(el)
	return false, noopRelease
}

// releaseFn returns a single-use callback that decrements
// bucket.inflight for the SPECIFIC bucket Allow incremented. The
// sync.Once guarantees the decrement runs exactly once even if the
// caller invokes the callback twice (so `defer release()` on a path
// that already called release earlier is safe).
//
// Bucket-pointer identity is critical: if the original bucket is
// removed (RecordSuccess, LRU eviction) and a new bucket is later
// created for the same username (next RecordFailure), this stale
// release callback must NOT decrement the new bucket's inflight —
// that reservation belongs to a different Allow caller. The closure
// captures the bucket pointer and decrements only when entries[key]
// still maps to that exact bucket. A removed-and-not-yet-recreated
// bucket lookup misses entirely and no-ops; a recreated bucket
// produces a different pointer and the decrement is skipped.
//
// One callback per Allow is the contract — RecordFailure must NOT
// decrement inflight, otherwise the per-callback ownership is lost
// and a deferred release silently consumes another goroutine's
// reservation under contention.
func (r *loginRateLimiter) releaseFn(key string, captured *loginBucket) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			el, ok := r.entries[key]
			if !ok {
				return
			}
			bucket := el.Value.(*loginBucket)
			if bucket != captured {
				// ABA: the original bucket was removed and a new bucket
				// was created for the same key. This callback's
				// reservation belonged to the original; the new bucket's
				// inflight is owned by the Allow that created it.
				return
			}
			if bucket.inflight > 0 {
				bucket.inflight--
			}
		})
	}
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
	// inflight is intentionally NOT decremented here — it is owned by
	// the Allow→release pair (sync.Once-guarded). RecordFailure owns
	// only the failures counter.
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

// evictLocked enforces the LRU cap, but preserves three classes of buckets:
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
//   (c) Buckets with active inflight reservations — evicting one drops the
//       reservation while the verify is still running; subsequent Allows
//       on a re-created bucket would re-enter the first-burst regime
//       and run another batch of parallel verifies on top. Today's
//       Argon2id verify always completes well within lockout (so this
//       case is structural defense-in-depth, not currently exploitable),
//       but the predicate must include inflight to remain fail-closed
//       if verify time ever grows.
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
			hasInflight := b.inflight > 0
			if lockedAndFresh || partialAndFresh || hasInflight {
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
