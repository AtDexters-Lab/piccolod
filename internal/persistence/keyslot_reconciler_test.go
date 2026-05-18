package persistence

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"piccolod/internal/state/paths"
)

// stubBlobCrypto is a lightweight in-test AEAD using a fixed 32-byte key.
// Avoids the ~256 MiB-per-call Argon2id cost of constructing a real
// crypt.Manager for every reconciler unit test (Go's parallel test runner
// would exhaust memory otherwise). Production code paths use the real
// crypt.Manager (covered by keyslot_blob_test.go's round-trip tests).
type stubBlobCrypto struct {
	key []byte
}

func newStubBlobCrypto(t *testing.T) *stubBlobCrypto {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	return &stubBlobCrypto{key: k}
}

func (s *stubBlobCrypto) EncryptWithAAD(plaintext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, aad), nil
}

func (s *stubBlobCrypto) DecryptWithAAD(ct, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ct) < aead.NonceSize() {
		return nil, errors.New("stub: ct too short")
	}
	return aead.Open(nil, ct[:aead.NonceSize()], ct[aead.NonceSize():], aad)
}

// fakeReconcilerVM is a synthetic luksVolumeManager that records the
// reconciler's calls and stores metadata in memory so tests can drive
// convergence without going through real cryptsetup.
type fakeReconcilerVM struct {
	mu sync.Mutex

	volumes        []keyslotVolume
	unwrapErr      error
	provisionErr   map[string]error // volID → injected error
	provisionDelay time.Duration

	// onProvision, if non-nil, is called synchronously inside
	// provisionKeyslotOnVolumeByID with the per-call context, before the
	// injected error/delay is returned. Tests use this to flip external
	// state (SDEK lock, live id) deterministically between volumes.
	onProvision func(volID string)

	provisionCalls []provisionCall
}

type provisionCall struct {
	VolID      string
	Slot       int
	Passphrase string
	KeyID      string // captured kskey_id AFTER the call (for stickiness assertions)
}

func (f *fakeReconcilerVM) listKeyslotVolumes() ([]keyslotVolume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]keyslotVolume, len(f.volumes))
	for i, v := range f.volumes {
		metaCopy := *v.Meta
		out[i] = keyslotVolume{ID: v.ID, Meta: &metaCopy}
	}
	return out, nil
}

func (f *fakeReconcilerVM) readKeyslotMeta(volID string) (*volumeMetaV3, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.volumes {
		if v.ID == volID {
			metaCopy := *v.Meta
			return &metaCopy, nil
		}
	}
	return nil, fmt.Errorf("not found: %s", volID)
}

func (f *fakeReconcilerVM) writeKeyslotMeta(volID string, meta *volumeMetaV3) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, v := range f.volumes {
		if v.ID == volID {
			metaCopy := *meta
			f.volumes[i].Meta = &metaCopy
			return nil
		}
	}
	return fmt.Errorf("not found: %s", volID)
}

func (f *fakeReconcilerVM) provisionKeyslotOnVolumeByID(ctx context.Context, volID string, meta *volumeMetaV3, slot int, passphrase, masterKey []byte) error {
	_ = meta
	f.mu.Lock()
	delay := f.provisionDelay
	err := f.provisionErr[volID]
	hook := f.onProvision
	f.provisionCalls = append(f.provisionCalls, provisionCall{
		VolID:      volID,
		Slot:       slot,
		Passphrase: string(passphrase),
	})
	f.mu.Unlock()
	if hook != nil {
		hook(volID)
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (f *fakeReconcilerVM) unwrapMasterKey() ([]byte, error) {
	if f.unwrapErr != nil {
		return nil, f.unwrapErr
	}
	return []byte("master-key-padded-to-32-bytes-here!!"), nil
}

func makeFakeVolume(id, slot1ID, slot2ID string) keyslotVolume {
	return keyslotVolume{
		ID: id,
		Meta: &volumeMetaV3{
			Version:              metadataV3Version,
			Type:                 volumeTypeServiceData,
			LVName:               id,
			PasswordKeyslotKeyID: slot1ID,
			RecoveryKeyslotKeyID: slot2ID,
		},
	}
}

// Convergence on a clean run: 3 volumes at mixed states (pre-RFC empty,
// unprovisioned, stale fingerprint, current fingerprint) — all converge
// to live id with correct kill/add semantics surfaced via the call log.
func TestReconciler_SlotConvergence(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newStubBlobCrypto(t)

	const liveID = "feed1234feed1234"
	const oldID = "deadbeefdeadbeef"
	vm := &fakeReconcilerVM{
		volumes: []keyslotVolume{
			makeFakeVolume("v1", "", ""),                                     // pre-RFC
			makeFakeVolume("v2", "", KeyslotKeyIDUnprovisioned),              // unprovisioned slot 2
			makeFakeVolume("v3", "", oldID),                                  // stale fingerprint
			makeFakeVolume("v4", "", liveID),                                 // already converged — skip
		},
	}

	if err := writeKeyslotBlob(mgr, KeyslotRecovery, liveID, []byte("words")); err != nil {
		t.Fatalf("write blob: %v", err)
	}

	r := NewKeyslotReconciler(KeyslotReconcilerOptions{
		VolumeManager:     vm,
		Crypto:            mgr,
		LiveRecoveryKeyID: func() string { return liveID },
		LivePasswordKeyID: func() string { return "" },
		SDEKLoaded:        func() bool { return true },
	})

	r.runSlotPass(context.Background(), KeyslotRecovery, r.liveRecoveryKeyID)

	// v1 (pre-RFC), v2 (unprov), v3 (stale) should all have provision calls.
	// v4 (current) should not.
	if got := len(vm.provisionCalls); got != 3 {
		t.Fatalf("expected 3 provision calls, got %d: %+v", got, vm.provisionCalls)
	}
	for _, c := range vm.provisionCalls {
		if c.Slot != int(KeyslotRecovery) {
			t.Errorf("unexpected slot: %d", c.Slot)
		}
		if c.Passphrase != "words" {
			t.Errorf("unexpected passphrase: %q", c.Passphrase)
		}
	}
	// All affected volumes now stamped with liveID.
	for _, v := range vm.volumes {
		if v.ID == "v4" {
			continue // unchanged path
		}
		if v.Meta.RecoveryKeyslotKeyID != liveID {
			t.Errorf("vol %s: expected RecoveryKeyslotKeyID=%s, got %s", v.ID, liveID, v.Meta.RecoveryKeyslotKeyID)
		}
	}
	// Status reflects done count.
	st := r.Status()[KeyslotRecovery]
	if st.CapturedKeyID != liveID || st.TargetKeyID != liveID {
		t.Errorf("status ids: captured=%s target=%s", st.CapturedKeyID, st.TargetKeyID)
	}
	if st.Done != 4 {
		t.Errorf("expected done=4 (3 provisioned + 1 already), got %d", st.Done)
	}
	if st.Pending != 0 {
		t.Errorf("expected pending=0, got %d", st.Pending)
	}
	// Captured blob PERSISTS after convergence (codex iter-4 P2 #1):
	// auto-delete was removed to eliminate the volume-creation race
	// with rescan→delete. Next pass with a post-rotation live id
	// cleans this blob as orphan via the defer.
	if _, err := readKeyslotBlob(mgr, KeyslotRecovery, liveID); err != nil {
		t.Errorf("expected blob to persist post-convergence (deferred orphan cleanup): %v", err)
	}
}

// Idempotency: a second pass on the same fixture is a no-op (no provision
// calls).
func TestReconciler_IdempotentSecondPass(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newStubBlobCrypto(t)
	const liveID = "00112233aabbccdd"
	vm := &fakeReconcilerVM{
		volumes: []keyslotVolume{makeFakeVolume("v1", "", liveID)},
	}
	r := NewKeyslotReconciler(KeyslotReconcilerOptions{
		VolumeManager:     vm,
		Crypto:            mgr,
		LiveRecoveryKeyID: func() string { return liveID },
		SDEKLoaded:        func() bool { return true },
	})
	// First pass — no blob present, nothing to do.
	r.runSlotPass(context.Background(), KeyslotRecovery, r.liveRecoveryKeyID)
	if len(vm.provisionCalls) != 0 {
		t.Fatalf("expected 0 calls (no blob), got %d", len(vm.provisionCalls))
	}
	// Write blob, run pass — converges (v1 already at liveID so still no call).
	_ = writeKeyslotBlob(mgr, KeyslotRecovery, liveID, []byte("p"))
	r.runSlotPass(context.Background(), KeyslotRecovery, r.liveRecoveryKeyID)
	if len(vm.provisionCalls) != 0 {
		t.Fatalf("expected 0 calls (already converged), got %d", len(vm.provisionCalls))
	}
}

// D8: SDEK locks mid-pass → pass aborts at next per-volume boundary,
// captured passphrase blob persists, status reports SDEK locked.
func TestReconciler_AbortsCleanlyOnSDEKLock(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newStubBlobCrypto(t)
	const liveID = "abc123abc123abc1"
	vm := &fakeReconcilerVM{
		volumes: []keyslotVolume{
			makeFakeVolume("v1", "", ""),
			makeFakeVolume("v2", "", ""),
			makeFakeVolume("v3", "", ""),
		},
	}
	_ = writeKeyslotBlob(mgr, KeyslotRecovery, liveID, []byte("p"))

	var sdekUnlocked atomic.Bool
	sdekUnlocked.Store(true)
	// Flip the lock synchronously after v1's provision call returns,
	// so the reconciler observes SDEK locked at the top of the v2 loop
	// iteration — no timing race.
	vm.onProvision = func(volID string) {
		if volID == "v1" {
			sdekUnlocked.Store(false)
		}
	}
	r := NewKeyslotReconciler(KeyslotReconcilerOptions{
		VolumeManager:     vm,
		Crypto:            mgr,
		LiveRecoveryKeyID: func() string { return liveID },
		SDEKLoaded:        sdekUnlocked.Load,
	})

	r.runSlotPass(context.Background(), KeyslotRecovery, r.liveRecoveryKeyID)

	if got := len(vm.provisionCalls); got != 1 {
		t.Errorf("expected exactly 1 provision call before lock-abort, got %d", got)
	}
	// Blob must persist for next-unlock resumption.
	if _, err := readKeyslotBlob(mgr, KeyslotRecovery, liveID); err != nil {
		t.Errorf("expected blob to persist for resume, got %v", err)
	}
	st := r.Status()[KeyslotRecovery]
	if st.LastError == "" {
		t.Errorf("expected last_error set on lock abort")
	}
}

// D12 / F-A1: live id advances mid-pass → reconciler aborts cleanly,
// does NOT revert NEW-stamped volumes.
func TestReconciler_CaptureStickinessAbortsOnLiveIDAdvance(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newStubBlobCrypto(t)
	const capturedID = "cccccccccccccccc"
	const newerID = "ddddddddddddddd0"
	vm := &fakeReconcilerVM{
		volumes: []keyslotVolume{
			makeFakeVolume("v1", "", ""),
			makeFakeVolume("v2", "", ""),
			makeFakeVolume("v3", "", ""),
		},
	}
	_ = writeKeyslotBlob(mgr, KeyslotRecovery, capturedID, []byte("p"))

	live := atomic.Value{}
	live.Store(capturedID)
	// Synchronously flip live id after v1 lands, before the reconciler
	// checks capture stickiness for v2.
	vm.onProvision = func(volID string) {
		if volID == "v1" {
			live.Store(newerID)
		}
	}
	r := NewKeyslotReconciler(KeyslotReconcilerOptions{
		VolumeManager:     vm,
		Crypto:            mgr,
		LiveRecoveryKeyID: func() string { return live.Load().(string) },
		SDEKLoaded:        func() bool { return true },
	})

	r.runSlotPass(context.Background(), KeyslotRecovery, r.liveRecoveryKeyID)

	// Exactly v1 stamped with capturedID; remaining volumes unchanged.
	stamped := 0
	for _, v := range vm.volumes {
		if v.Meta.RecoveryKeyslotKeyID == capturedID {
			stamped++
		}
	}
	if stamped != 1 {
		t.Errorf("expected exactly 1 volume stamped on capture-stickiness abort, got %d", stamped)
	}
	// Capture blob persists on abort — cleanup-defer is anchored to
	// capturedID (not the advanced live id) so the captured blob isn't
	// self-deleted. Pass-B (triggered by the concurrent /generate's
	// nudge) will read the newer blob and clean this captured blob as
	// orphan at its own end-of-pass.
	if _, err := readKeyslotBlob(mgr, KeyslotRecovery, capturedID); err != nil {
		t.Errorf("capture blob should persist on stickiness abort: %v", err)
	}
}

// F-A2 cleanup rule: orphan blobs (key_id != live id) are deleted at
// end-of-pass-success.
func TestReconciler_OrphanCleanupAtEndOfPass(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newStubBlobCrypto(t)
	const liveID = "1111222233334444"
	const orphanID = "8888777766665555"
	vm := &fakeReconcilerVM{
		volumes: []keyslotVolume{makeFakeVolume("v1", "", liveID)},
	}
	_ = writeKeyslotBlob(mgr, KeyslotRecovery, liveID, []byte("live"))
	_ = writeKeyslotBlob(mgr, KeyslotRecovery, orphanID, []byte("orphan"))

	r := NewKeyslotReconciler(KeyslotReconcilerOptions{
		VolumeManager:     vm,
		Crypto:            mgr,
		LiveRecoveryKeyID: func() string { return liveID },
		SDEKLoaded:        func() bool { return true },
	})
	r.runSlotPass(context.Background(), KeyslotRecovery, r.liveRecoveryKeyID)

	if _, err := readKeyslotBlob(mgr, KeyslotRecovery, orphanID); err == nil {
		t.Errorf("expected orphan blob deleted at end-of-pass")
	}
}

// Per-volume failure: provision fails on v2 → metadata for v2 stays at
// the prior value (no over-stamping); v1, v3 still converge.
func TestReconciler_PerVolumeFailureLeavesMetaUnchanged(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newStubBlobCrypto(t)
	const liveID = "55556666aaaa1111"
	vm := &fakeReconcilerVM{
		volumes: []keyslotVolume{
			makeFakeVolume("v1", "", ""),
			makeFakeVolume("v2", "", "stalestalestale1"),
			makeFakeVolume("v3", "", ""),
		},
		provisionErr: map[string]error{"v2": errors.New("cryptsetup boom")},
	}
	_ = writeKeyslotBlob(mgr, KeyslotRecovery, liveID, []byte("p"))

	r := NewKeyslotReconciler(KeyslotReconcilerOptions{
		VolumeManager:     vm,
		Crypto:            mgr,
		LiveRecoveryKeyID: func() string { return liveID },
		SDEKLoaded:        func() bool { return true },
	})
	r.runSlotPass(context.Background(), KeyslotRecovery, r.liveRecoveryKeyID)

	for _, v := range vm.volumes {
		switch v.ID {
		case "v1", "v3":
			if v.Meta.RecoveryKeyslotKeyID != liveID {
				t.Errorf("vol %s: expected liveID, got %s", v.ID, v.Meta.RecoveryKeyslotKeyID)
			}
		case "v2":
			if v.Meta.RecoveryKeyslotKeyID != "stalestalestale1" {
				t.Errorf("vol v2: expected stale ID preserved, got %s", v.Meta.RecoveryKeyslotKeyID)
			}
		}
	}
	// On per-volume failure, blob must persist for next-pass retry.
	if _, err := readKeyslotBlob(mgr, KeyslotRecovery, liveID); err != nil {
		t.Errorf("expected blob to persist after partial failure: %v", err)
	}
	// Status reports last_error.
	st := r.Status()[KeyslotRecovery]
	if st.LastError == "" {
		t.Errorf("expected last_error set on partial failure")
	}
}

// Both slots in a single Start+Nudge cycle: writes a slot-1 and slot-2
// blob, nudges, asserts both converge.
func TestReconciler_DrainsBothSlotsPerPass(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newStubBlobCrypto(t)
	const slot1ID = "1111aaaa2222bbbb"
	const slot2ID = "3333cccc4444dddd"
	vm := &fakeReconcilerVM{
		volumes: []keyslotVolume{makeFakeVolume("v1", "", "")},
	}
	_ = writeKeyslotBlob(mgr, KeyslotPassword, slot1ID, []byte("pw"))
	_ = writeKeyslotBlob(mgr, KeyslotRecovery, slot2ID, []byte("words"))

	r := NewKeyslotReconciler(KeyslotReconcilerOptions{
		VolumeManager:     vm,
		Crypto:            mgr,
		LiveRecoveryKeyID: func() string { return slot2ID },
		LivePasswordKeyID: func() string { return slot1ID },
		SDEKLoaded:        func() bool { return true },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.Start(ctx)
	defer r.Stop()

	// Wait for convergence — the initial pass at Start should drain
	// both blobs against v1.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		vm.mu.Lock()
		n := len(vm.provisionCalls)
		vm.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(vm.provisionCalls) < 2 {
		t.Fatalf("expected ≥2 provision calls (slot 1 + slot 2), got %d", len(vm.provisionCalls))
	}
	slots := map[int]bool{}
	for _, c := range vm.provisionCalls {
		slots[c.Slot] = true
	}
	if !slots[1] || !slots[2] {
		t.Errorf("expected both slots provisioned, got %v", slots)
	}
}

// D11 blob-write-first ordering: when the prepare hook (blob write)
// fails, keyset.json is NOT written and the prior recovery key remains
// authoritative. RFC F-iter3-B1 also covers this — the AtomicWriteFile
// primitive handles the durability between blob + keyset commits; this
// test covers the higher-level ordering contract.
func TestPrepareHook_FailureBlocksKeysetCommit(t *testing.T) {
	paths.SetRootsForTest(t)
	core, _ := paths.SetRootsForTest(t)
	_ = core
	// Use the real crypt.Manager so the keyset.json on-disk shape is
	// authentic.
	mgr := newUnlockedCryptManager(t)

	// First /generate succeeds — establishes baseline keyset.json.
	wordsA, keyIDA, err := mgr.GenerateRecoveryKeyWithHook(false, nil)
	if err != nil {
		t.Fatalf("first generate: %v", err)
	}
	if len(wordsA) == 0 || keyIDA == "" {
		t.Fatalf("empty baseline")
	}

	// Second /generate with rotating=true, hook returns error → commit
	// must be skipped.
	hookErr := errors.New("synthetic blob write failure")
	_, _, err = mgr.GenerateRecoveryKeyWithHook(true, func(words []string, keyID string, sdek []byte) error {
		return hookErr
	})
	if err == nil {
		t.Fatalf("expected hook error to propagate")
	}
	if !errors.Is(err, hookErr) {
		// hook error is wrapped; just check the message is preserved
		if !contains(err.Error(), "synthetic blob write failure") {
			t.Errorf("expected hook error in message, got %v", err)
		}
	}

	// Verify keyset.json still reflects the first generate's key_id.
	if got := mgr.RecoveryKeyID(); got != keyIDA {
		t.Errorf("keyset.json drifted after failed hook: got %q want %q", got, keyIDA)
	}
}

// Startup cleanup runs before the first pass — orphan blobs left by a
// crashed process are wiped when the live key id is well-defined.
// Probe returning "" is conservative (could be "no key" OR transient
// failure) so cleanup is SKIPPED — the blob persists for a future
// pass to clean once the live id becomes available (codex iter-3 P1).
func TestReconciler_StartupOrphanCleanup(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newStubBlobCrypto(t)
	const orphan = "fefefefefefefefe"
	const live = "1111111111111111"
	_ = writeKeyslotBlob(mgr, KeyslotRecovery, orphan, []byte("p"))

	vm := &fakeReconcilerVM{}
	r := NewKeyslotReconciler(KeyslotReconcilerOptions{
		VolumeManager:     vm,
		Crypto:            mgr,
		LiveRecoveryKeyID: func() string { return live },
		SDEKLoaded:        func() bool { return true },
	})
	r.startupOrphanCleanup()
	if _, err := readKeyslotBlob(mgr, KeyslotRecovery, orphan); err == nil {
		t.Errorf("expected orphan deleted at startup")
	}
}

// Codex iter-3 P1: when live id probe returns "" (could be "no key" OR
// "transient probe failure during daemon restart"), startup cleanup must
// SKIP — deleting on a transient probe failure would lose a pending blob
// from a crashed-mid-rotation prior process.
func TestReconciler_StartupOrphanCleanup_SkipsOnEmptyProbe(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newStubBlobCrypto(t)
	const pendingFromPriorProcess = "deadbeefdeadbeef"
	_ = writeKeyslotBlob(mgr, KeyslotRecovery, pendingFromPriorProcess, []byte("p"))

	vm := &fakeReconcilerVM{}
	r := NewKeyslotReconciler(KeyslotReconcilerOptions{
		VolumeManager:     vm,
		Crypto:            mgr,
		LiveRecoveryKeyID: func() string { return "" }, // transient probe failure (or "no key")
		SDEKLoaded:        func() bool { return true },
	})
	r.startupOrphanCleanup()
	if _, err := readKeyslotBlob(mgr, KeyslotRecovery, pendingFromPriorProcess); err != nil {
		t.Errorf("expected pending blob to PERSIST on empty-probe startup, got %v", err)
	}
}
