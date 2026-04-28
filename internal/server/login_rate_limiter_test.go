package server

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLoginRateLimiter_AllowReservesInflight pins the PK-3 contract: when
// a bucket already exists for the username, concurrent Allow callers see
// `failures + inflight` and cannot collectively pass more than maxFails
// reservations. Pre-fix, all N concurrent Allow calls returned true while
// `failures` was below the threshold, letting parallel Argon2id verifies
// race past the lockout.
func TestLoginRateLimiter_AllowReservesInflight(t *testing.T) {
	t.Parallel()
	const maxFails = 5
	r := newLoginRateLimiter(maxFails, 15*time.Minute, 16)

	// Seed a bucket. Without an existing bucket the first Allow does not
	// reserve inflight (preserves the no-bucket-on-Allow memory protection).
	r.RecordFailure("victim")

	// Spawn maxFails*4 concurrent Allows; expect at most maxFails-1 to be
	// admitted (one failure already on the bucket). The rest must see false.
	const N = maxFails * 4
	var allowed atomic.Int64
	var releases []func()
	var mu sync.Mutex

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ok, release := r.Allow("victim")
			if ok {
				allowed.Add(1)
				mu.Lock()
				releases = append(releases, release)
				mu.Unlock()
			} else {
				release() // no-op, but exercise the contract
			}
		}()
	}
	wg.Wait()

	wantMax := int64(maxFails - 1) // bucket already had 1 failure
	if got := allowed.Load(); got > wantMax {
		t.Fatalf("admitted %d concurrent verifies, want at most %d (failures+inflight must respect maxFails)", got, wantMax)
	}
	if got := allowed.Load(); got == 0 {
		t.Fatalf("admitted 0 verifies; expected up to %d under capacity", wantMax)
	}

	// Release every reservation.
	for _, rel := range releases {
		rel()
	}

	// After releases, inflight is back to zero so a fresh Allow sees the
	// stored-failures counter (still 1) and is admitted.
	ok, release := r.Allow("victim")
	if !ok {
		t.Fatalf("Allow after full release returned false; reservations leaked")
	}
	release()
}

// TestLoginRateLimiter_ReleaseUnknownUsername confirms the no-op release
// returned for unseen usernames is safe to invoke.
func TestLoginRateLimiter_ReleaseUnknownUsername(t *testing.T) {
	t.Parallel()
	r := newLoginRateLimiter(5, time.Minute, 8)
	ok, release := r.Allow("never-seen")
	if !ok {
		t.Fatalf("unseen username Allow should return true")
	}
	release() // must not panic, must not WARN
	release() // double-release of no-op is also safe
}

// TestLoginRateLimiter_ReleaseSurvivesRecordSuccess pins the idempotent
// behavior when RecordSuccess removes the bucket between Allow and
// release. The deferred release in the handler runs unconditionally, so
// it must no-op rather than panic or under-decrement to a phantom -1.
func TestLoginRateLimiter_ReleaseSurvivesRecordSuccess(t *testing.T) {
	t.Parallel()
	r := newLoginRateLimiter(5, time.Minute, 8)
	r.RecordFailure("user")

	ok, release := r.Allow("user")
	if !ok {
		t.Fatalf("Allow on partial bucket should reserve and admit")
	}
	r.RecordSuccess("user")
	release() // bucket is gone; must not panic

	// New Allow on now-cleared bucket: unseen → true with noop release.
	ok2, release2 := r.Allow("user")
	if !ok2 {
		t.Fatalf("Allow after RecordSuccess should admit")
	}
	release2()
}

// TestLoginRateLimiter_LockedRefuses confirms the existing lockout
// behavior is preserved by the inflight refactor: once failures reach
// maxFails, Allow returns false until lockout elapses.
func TestLoginRateLimiter_LockedRefuses(t *testing.T) {
	t.Parallel()
	const maxFails = 3
	r := newLoginRateLimiter(maxFails, time.Minute, 8)
	for i := 0; i < maxFails; i++ {
		r.RecordFailure("locked")
	}
	ok, release := r.Allow("locked")
	if ok {
		t.Fatalf("Allow on locked bucket must return false")
	}
	release() // no-op for locked branch
}

// TestLoginRateLimiter_HandlerContract simulates the production handler
// pattern under contention: each goroutine runs Allow → RecordFailure →
// release (mimicking `defer release()` after a verify-failed path). This
// is the path the counter-based release got wrong (Phase-1 finding B1):
// pre-fix, RecordFailure decremented inflight AND the deferred release
// also decremented inflight, so a single completed verify silently
// freed two reservation slots, allowing more concurrent admissions than
// the contract states.
//
// With per-callback ownership (sync.Once on releaseFn, RecordFailure
// untouching inflight), the total admissions across the burst respect
// the budget `maxFails - failures_at_burst_start`.
func TestLoginRateLimiter_HandlerContract(t *testing.T) {
	t.Parallel()
	const maxFails = 5
	r := newLoginRateLimiter(maxFails, 15*time.Minute, 16)

	// Seed with a single failure so a bucket exists; the budget is
	// maxFails-1 = 4 concurrent verify-failures before lockout.
	r.RecordFailure("victim")

	const N = maxFails * 4
	var allowed atomic.Int64
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ok, release := r.Allow("victim")
			defer release()
			if !ok {
				return
			}
			allowed.Add(1)
			// Mimic verify-failed → RecordFailure → defer release.
			r.RecordFailure("victim")
		}()
	}
	wg.Wait()

	// At most maxFails-1 admissions before failures+inflight crosses
	// the threshold. Pre-fix, the deferred release after RecordFailure
	// would over-decrement inflight under contention, allowing
	// roughly 2× admissions (each completed verify freed a slot AND
	// RecordFailure freed it again). The sync.Once-guarded release
	// + RecordFailure-no-touch-inflight discipline makes this hold.
	wantMax := int64(maxFails - 1)
	if got := allowed.Load(); got > wantMax {
		t.Fatalf("admitted %d concurrent verifies under handler-contract pattern, want at most %d (release double-decrement re-introduced?)", got, wantMax)
	}
}

// TestLoginRateLimiter_FillThenCompleteOne is the deterministic
// two-phase test the codex Phase-3 review pointed out the original
// concurrent test was missing: pre-fix, the deferred-release-after-
// RecordFailure double-decrement only manifested when a goroutine's
// release fired AFTER another goroutine's reservation existed. A
// concurrent test where each goroutine runs Allow → RecordFailure →
// release atomically (no overlap) admits exactly maxFails-1 even pre-
// fix, masking the bug. This test holds reservations open, completes
// one with RecordFailure, and asserts that one (not two) reservation
// is freed — i.e., a fresh Allow at that point is denied because
// failures+inflight == maxFails.
func TestLoginRateLimiter_FillThenCompleteOne(t *testing.T) {
	t.Parallel()
	const maxFails = 5
	r := newLoginRateLimiter(maxFails, 15*time.Minute, 16)
	r.RecordFailure("victim") // failures=1, inflight=0

	// Fill the remaining capacity. After this, failures=1, inflight=4.
	const N = maxFails - 1
	releases := make([]func(), 0, N)
	for i := 0; i < N; i++ {
		ok, release := r.Allow("victim")
		if !ok {
			t.Fatalf("expected Allow=true at i=%d (capacity=%d)", i, N)
		}
		releases = append(releases, release)
	}

	// At capacity: failures(1)+inflight(4)=5=maxFails. New Allow denied.
	if ok, _ := r.Allow("victim"); ok {
		t.Fatalf("Allow at capacity must be denied")
	}

	// Complete ONE attempt (failure path): RecordFailure, then release.
	// Pre-fix: RecordFailure also decremented inflight, then release
	// decremented again, freeing TWO slots from one completion. That
	// would let the next Allow admit (failures=2, inflight=2, 2+2<5).
	// Post-fix: RecordFailure increments only failures (failures=2);
	// release decrements only inflight (inflight=3). Net: failures+
	// inflight stays at 5 → denied.
	r.RecordFailure("victim")
	releases[0]()

	if ok, _ := r.Allow("victim"); ok {
		t.Fatalf("after one completion, failures+inflight should still equal maxFails (5); pre-fix double-decrement would admit here")
	}

	// Complete the remaining reservations (RecordFailure + release each).
	// After all N=4 completions: failures = 1 (seed) + 4 = 5, inflight=0,
	// bucket locked.
	for i := 1; i < N; i++ {
		r.RecordFailure("victim")
		releases[i]()
	}
	if ok, _ := r.Allow("victim"); ok {
		t.Fatalf("bucket should be locked at failures=%d", maxFails)
	}
}

// TestLoginRateLimiter_StaleReleaseDoesNotConsumeRecreatedBucket pins
// the ABA fix: an old release callback whose bucket was removed
// (RecordSuccess) and recreated (next RecordFailure for the same
// username) MUST NOT decrement the new bucket's inflight. Bucket-
// pointer identity in the closure is the guard.
//
// To detect a stale-release decrement, bucket B must be at the
// admission boundary: if inflight is silently reduced from N to N-1,
// a subsequent Allow that should be denied (failures+inflight ==
// maxFails) would instead be admitted. Filling B to capacity makes
// that assertion sharp.
func TestLoginRateLimiter_StaleReleaseDoesNotConsumeRecreatedBucket(t *testing.T) {
	t.Parallel()
	const maxFails = 3
	r := newLoginRateLimiter(maxFails, 15*time.Minute, 8)

	// Phase 1: seed bucket A, take a reservation, hold it.
	r.RecordFailure("user") // bucket A: failures=1, inflight=0
	ok, oldRelease := r.Allow("user")
	if !ok {
		t.Fatalf("phase 1 Allow should succeed")
	}
	// bucket A: failures=1, inflight=1

	// Phase 2: clear the bucket via RecordSuccess. Bucket A is gone.
	r.RecordSuccess("user")

	// Phase 3: a new attempt creates a fresh bucket B and fill it to
	// admission capacity (failures + inflight == maxFails). At this
	// point Allow MUST refuse — that's the boundary where a stale
	// decrement would silently re-admit.
	r.RecordFailure("user") // bucket B: failures=1, inflight=0
	heldReleases := make([]func(), 0, maxFails-1)
	for i := 0; i < maxFails-1; i++ {
		ok, release := r.Allow("user")
		if !ok {
			t.Fatalf("phase 3 fill: Allow %d/%d should succeed", i+1, maxFails-1)
		}
		heldReleases = append(heldReleases, release)
	}
	// bucket B: failures=1, inflight=2, at capacity.
	if ok, _ := r.Allow("user"); ok {
		t.Fatalf("phase 3 capacity check: Allow at failures+inflight=%d should refuse", maxFails)
	}

	// Phase 4: fire the stale release from phase 1. It MUST NOT
	// decrement bucket B's inflight. If the ABA guard is broken,
	// bucket B's inflight drops to 1, opening one capacity slot.
	oldRelease()

	// Phase 5: post-stale-release Allow MUST still refuse (capacity
	// unchanged at maxFails). Pre-fix key-based release would have
	// decremented bucket B and admitted here.
	if ok, _ := r.Allow("user"); ok {
		t.Fatalf("phase 5: stale release decremented bucket B's inflight; capacity bypassed")
	}

	// Cleanup.
	for _, rel := range heldReleases {
		rel()
	}
}

// TestLoginRateLimiter_AllowAgesOutStalePartial pins the F3 fix: a
// bucket with K<maxFails old failures has effectively recovered once
// `lockout` elapses; Allow now resets failures to 0 inline, matching
// RecordFailure's age-out. Without this, 9 stale failures would let
// one Allow reserve and the next concurrent Allow get falsely
// rejected at failures+inflight=10.
func TestLoginRateLimiter_AllowAgesOutStalePartial(t *testing.T) {
	t.Parallel()
	const maxFails = 10
	r := newLoginRateLimiter(maxFails, 50*time.Millisecond, 16)
	for i := 0; i < maxFails-1; i++ {
		r.RecordFailure("user")
	} // failures=9, lastFailureAt=now

	// Wait past the lockout window so the partial bucket is stale.
	time.Sleep(60 * time.Millisecond)

	// Allow must age out the stale failures and reserve in the fresh
	// sequence. A second concurrent Allow then sees failures(0)+
	// inflight(1)=1 < maxFails=10 and is admitted.
	ok1, release1 := r.Allow("user")
	if !ok1 {
		t.Fatalf("Allow on stale partial should age out and admit")
	}
	ok2, release2 := r.Allow("user")
	if !ok2 {
		t.Fatalf("second Allow after age-out should admit (failures reset to 0); pre-fix would see stale failures=9 and reject at inflight=1")
	}
	release1()
	release2()
}

// TestLoginRateLimiter_LockoutExpiry confirms a lockout that's older
// than the configured window is dropped on next Allow and admits a
// fresh attempt.
func TestLoginRateLimiter_LockoutExpiry(t *testing.T) {
	t.Parallel()
	const maxFails = 2
	// 1ns lockout — test sleeps long enough to expire.
	r := newLoginRateLimiter(maxFails, time.Nanosecond, 8)
	for i := 0; i < maxFails; i++ {
		r.RecordFailure("expired")
	}
	time.Sleep(time.Microsecond)

	ok, release := r.Allow("expired")
	if !ok {
		t.Fatalf("Allow after lockout window expired must admit (bucket dropped)")
	}
	release()
}
