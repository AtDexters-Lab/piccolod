package persistence

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"

	"piccolod/internal/cryptoutil"
	"piccolod/internal/resources/pressure"
)

// KeyslotReconciler converges LUKS keyslot 1 (admin password) and keyslot 2
// (recovery mnemonic) across all v3 volumes after the synchronous handlers
// return. RFC 20260510 §Reconciler shape.
//
// The synchronous /generate, /password, /reset-password, /setup handlers
// write an encrypted passphrase blob to <core>/mounts/control-plane/recovery/
// and nudge this reconciler. The handler returns sub-second; the reconciler
// drains the blob across volumes asynchronously and is restart-safe across
// process exit AND system reboot (blobs survive on the encrypted control
// plane; only the SDEK in RAM is wiped on reboot).
//
// Convergence model — per slot per pass:
//   1. Read the live key_id for the slot (RecoveryKeyID for slot 2,
//      injected fingerprint provider for slot 1).
//   2. Read the matching blob from disk — capture (key_id, passphrase) as
//      one atomic unit (D9). Blobs whose key_id != live are orphans.
//   3. Walk v3 volumes. For each volume:
//      a. Abort the pass if SDEK locks mid-pass (D8).
//      b. Re-read the live key_id and compare to captured; if advanced,
//         abort the pass — a /generate fired mid-walk and we let pass-B
//         pick up the newer blob (D12, F-A1).
//      c. Skip volumes whose kskey_id already matches captured.
//      d. Otherwise call luksSetKeyslot — pre-kill probe + WithoutCancel
//         kill+add (D7) makes the slot transition atomic.
//      e. On success, stamp volume's kskey_id = captured.
//   4. On full per-slot convergence, delete the live blob.
//   5. Clean up orphan blobs for the slot (F-A2 cleanup rule).
//
// The reconciler is also responsible for orphan cleanup at process start
// (Start() runs cleanup before the first pass), so blobs left over from a
// crashed process don't accumulate.

// keyslotReconcilerVM is the subset of luksVolumeManager the reconciler
// needs. Defined as an interface so unit tests can drive convergence
// against a synthetic volume set without going through the full Module.
type keyslotReconcilerVM interface {
	listKeyslotVolumes() ([]keyslotVolume, error)
	readKeyslotMeta(volID string) (*volumeMetaV3, error)
	writeKeyslotMeta(volID string, meta *volumeMetaV3) error
	provisionKeyslotOnVolumeByID(ctx context.Context, volID string, meta *volumeMetaV3, slot int, passphrase, masterKey []byte) error
	unwrapMasterKey() ([]byte, error)
}

type keyslotVolume struct {
	ID   string
	Meta *volumeMetaV3
}

// keyslotLiveKeyID returns the current generation fingerprint for a slot
// ("" when no key is set). Slot 2 reads cryptoManager.RecoveryKeyID();
// slot 1 reads from the userManager / authManager via injection (the
// password hash is owned outside the persistence layer).
type keyslotLiveKeyID func() string

// keyslotSDEKLoaded reports whether the SDEK is currently loaded — the
// reconciler honors lock-state transitions (D8) by polling this between
// per-volume operations.
type keyslotSDEKLoaded func() bool

// KeyslotReconcilerStatus is the per-slot snapshot consumed by
// /api/v1/crypto/status. RFC §Status surface.
type KeyslotReconcilerStatus struct {
	CapturedKeyID            string `json:"captured_key_id"`
	TargetKeyID              string `json:"target_key_id"`
	TotalVolumes             int    `json:"total_volumes"`
	Done                     int    `json:"done"`
	Pending                  int    `json:"pending"`
	UnprovisionedSteadyState int    `json:"unprovisioned_steady_state"`
	RotationStormPending     int    `json:"rotation_storm_pending"`
	InProgress               bool   `json:"in_progress"`
	LastError                string `json:"last_error,omitempty"`
}

// KeyslotReconciler is the goroutine + state for converging both slots.
type KeyslotReconciler struct {
	vm                keyslotReconcilerVM
	crypto            keyslotCryptoProvider
	livePasswordKeyID keyslotLiveKeyID
	liveRecoveryKeyID keyslotLiveKeyID
	sdekLoaded        keyslotSDEKLoaded

	nudgeCh     chan struct{}
	stopCh      chan struct{}
	done        chan struct{}
	initialDone chan struct{}
	startOnce   sync.Once
	wg          sync.WaitGroup

	mu     sync.RWMutex
	status map[KeyslotSlot]KeyslotReconcilerStatus

	// auditFn is called for observable events; nil disables audit.
	auditFn func(event string, fields map[string]any)
}

// keyslotCryptoProvider is the subset of crypt.Manager the reconciler
// uses for blob encryption + AAD-bound decrypt. Decoupled as an interface
// so tests can substitute a stub. Identical surface to blobCrypto so the
// reconciler can pass its provider directly to the blob helpers.
type keyslotCryptoProvider = blobCrypto

// KeyslotReconcilerOptions wires the reconciler's dependencies.
type KeyslotReconcilerOptions struct {
	VolumeManager     keyslotReconcilerVM
	Crypto            keyslotCryptoProvider
	LiveRecoveryKeyID keyslotLiveKeyID
	LivePasswordKeyID keyslotLiveKeyID
	SDEKLoaded        keyslotSDEKLoaded
	AuditFn           func(event string, fields map[string]any)
}

// NewKeyslotReconciler constructs but does not start the reconciler.
// Caller invokes Start with a long-lived context (typically server
// background context).
func NewKeyslotReconciler(opts KeyslotReconcilerOptions) *KeyslotReconciler {
	r := &KeyslotReconciler{
		vm:                opts.VolumeManager,
		crypto:            opts.Crypto,
		livePasswordKeyID: opts.LivePasswordKeyID,
		liveRecoveryKeyID: opts.LiveRecoveryKeyID,
		sdekLoaded:        opts.SDEKLoaded,
		nudgeCh:           make(chan struct{}, 1),
		stopCh:            make(chan struct{}),
		done:              make(chan struct{}),
		initialDone:       make(chan struct{}),
		status:            make(map[KeyslotSlot]KeyslotReconcilerStatus),
		auditFn:           opts.AuditFn,
	}
	if r.sdekLoaded == nil {
		r.sdekLoaded = func() bool { return true }
	}
	if r.liveRecoveryKeyID == nil {
		r.liveRecoveryKeyID = func() string { return "" }
	}
	if r.livePasswordKeyID == nil {
		r.livePasswordKeyID = func() string { return "" }
	}
	return r
}

// Start launches the reconciler goroutine. Runs orphan cleanup once at
// startup (covers blobs from a crashed prior process) then enters the
// nudge loop. Start is idempotent; repeated calls leave the existing
// goroutine and initial-pass proof unchanged.
func (r *KeyslotReconciler) Start(ctx context.Context) {
	r.startOnce.Do(func() {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer close(r.done)
			// Cleanup and the initial pass drain whatever a prior process left
			// pending before recovery startup advances to another owner.
			r.runOwnedPass(ctx, true)
			close(r.initialDone)
			for {
				select {
				case <-ctx.Done():
					return
				case <-r.stopCh:
					return
				case <-r.nudgeCh:
					r.runOwnedPass(ctx, false)
				}
			}
		}()
	})
}

// InitialPassDone closes after startup orphan cleanup and the first bounded
// convergence pass have both returned.
func (r *KeyslotReconciler) InitialPassDone() <-chan struct{} { return r.initialDone }

func (r *KeyslotReconciler) runOwnedPass(ctx context.Context, startup bool) {
	releaseOwner := pressure.BeginLifecycleOwner("keyslot")
	defer releaseOwner()
	if startup {
		r.startupOrphanCleanup()
	}
	r.runOnePass(ctx)
}

// Stop signals the reconciler to exit and waits for the goroutine to
// drain. Safe to call once.
//
// Stop respects shutdown ctx via StopCtx. Plain Stop() blocks indefinitely
// — kept for back-compat callers, but production shutdown should use
// StopCtx with a bounded budget so a hung cryptsetup (LUKS device
// disappeared mid-op, marginal SD card with stuck I/O) cannot wedge the
// FENCE→DRAIN→CLEANUP sequence.
func (r *KeyslotReconciler) Stop() {
	close(r.stopCh)
	r.wg.Wait()
}

// StopCtx is Stop with a timeout. On ctx expiry, returns without waiting
// for the goroutine — it will exit on its own after the in-flight
// cryptsetup completes (the kill+add pair runs under context.WithoutCancel
// per D7 and cannot be cancelled mid-pair).
func (r *KeyslotReconciler) StopCtx(ctx context.Context) {
	close(r.stopCh)
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		// goroutine leaks until next in-flight cryptsetup completes;
		// SIGKILL at the end of shutdown will reap it.
	}
}

// Nudge wakes the reconciler. Non-blocking — if a nudge is already
// pending, this call coalesces with it (the next pass will see both
// triggers' state). Synchronous handlers call Nudge after writing their
// blob; volume creation calls Nudge after sub-case-i provisioning so the
// reconciler picks up newly-created volumes that might need add-only
// catchup.
func (r *KeyslotReconciler) Nudge() {
	select {
	case r.nudgeCh <- struct{}{}:
	default:
	}
}

// Status returns the per-slot reconciler status snapshot.
func (r *KeyslotReconciler) Status() map[KeyslotSlot]KeyslotReconcilerStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[KeyslotSlot]KeyslotReconcilerStatus, len(r.status))
	for k, v := range r.status {
		out[k] = v
	}
	return out
}

// startupOrphanCleanup deletes pending blobs whose key_id does not match
// the current live key_id for the slot — orphans left behind by a crash
// before they could be processed.
//
// Probe failure handling (codex P1): live*KeyID closures return "" for
// BOTH "no key configured" and "transient probe failure" (e.g., users DB
// momentarily locked during daemon restart). Cleanup must distinguish
// these — deleting on a transient probe failure would wipe a pending blob
// from a crashed-mid-rotation prior process and lose the only passphrase
// material for the resume. Conservative posture: skip cleanup entirely on
// empty probe; a real "no key" device has no blobs to clean anyway, and
// real orphans (post-rotation blobs whose live id is well-defined) get
// cleaned at end-of-pass.
func (r *KeyslotReconciler) startupOrphanCleanup() {
	probes := []struct {
		slot KeyslotSlot
		fn   keyslotLiveKeyID
	}{
		{KeyslotPassword, r.livePasswordKeyID},
		{KeyslotRecovery, r.liveRecoveryKeyID},
	}
	for _, p := range probes {
		live := p.fn()
		if live == "" {
			continue
		}
		if err := cleanupOrphanKeyslotBlobs(p.slot, live); err != nil {
			log.Printf("WARN: keyslot reconciler: startup orphan cleanup slot=%d: %v", p.slot, err)
		}
	}
}

func (r *KeyslotReconciler) runOnePass(ctx context.Context) {
	r.runSlotPass(ctx, KeyslotPassword, r.livePasswordKeyID)
	r.runSlotPass(ctx, KeyslotRecovery, r.liveRecoveryKeyID)
}

// runSlotPass reconciles a single slot. Capture rules per RFC §Reconciler
// shape: the (key_id, passphrase) pair is captured once at pass start; if
// /generate fires mid-pass, pass-B will pick it up. Per-volume
// capture-stickiness re-reads live id before each kill+add to avoid the
// F-A1 regression where pass-A would revert a volume from NEW to OLD.
func (r *KeyslotReconciler) runSlotPass(ctx context.Context, slot KeyslotSlot, liveProbe keyslotLiveKeyID) {
	r.setStatus(slot, func(s *KeyslotReconciler) KeyslotReconcilerStatus {
		st := s.status[slot]
		st.InProgress = true
		st.LastError = ""
		return st
	})
	defer r.setStatus(slot, func(s *KeyslotReconciler) KeyslotReconcilerStatus {
		st := s.status[slot]
		st.InProgress = false
		return st
	})

	liveID := liveProbe()
	r.setStatus(slot, func(s *KeyslotReconciler) KeyslotReconcilerStatus {
		st := s.status[slot]
		st.TargetKeyID = liveID
		return st
	})
	if liveID == "" {
		// No key for this slot OR live-id probe failed transiently.
		// Per codex P1: do NOT cleanup blobs here — deleting on a
		// transient probe failure would lose pending-blob material
		// from a crashed-mid-rotation prior process. Real orphans
		// get cleaned in a future pass when the live id is available.
		return
	}
	if !r.sdekLoaded() {
		r.audit("auth.keyslot_reconcile_aborted_locked", map[string]any{"slot": int(slot), "captured_key_id": liveID})
		r.setLastErr(slot, "SDEK locked at pass start")
		return
	}
	blob, err := readKeyslotBlob(r.crypto, slot, liveID)
	if err != nil {
		// No matching blob is the typical post-restart state and not an
		// error — the next /generate or password change writes a fresh
		// blob and re-nudges.
		//
		// Orphan cleanup using snapshot-before-probe: snapshot the dir
		// FIRST, then re-read liveID, then delete only listed entries
		// not matching liveID. Any blob written by a concurrent
		// /generate after the snapshot is invisible to this cleanup —
		// preserves the iter-6/iter-7 TOCTOU defense. Without this,
		// orphan blobs left over from a prior process/version
		// accumulate indefinitely when no subsequent rotation finds a
		// matching blob to trigger a full-pass cleanup.
		snapshot, _ := listKeyslotBlobs(slot)
		current := liveProbe()
		if current != "" {
			for _, id := range snapshot {
				if id != current {
					_ = deleteKeyslotBlob(slot, id)
				}
			}
		}
		if !os.IsNotExist(err) {
			r.setLastErr(slot, err.Error())
			r.audit("auth.keyslot_reconcile_failed", map[string]any{"slot": int(slot), "captured_key_id": liveID, "err": err.Error(), "phase": "blob_read"})
		}
		return
	}
	defer cryptoutil.SecureZero(blob.Passphrase)

	capturedID := liveID
	capturedPP := blob.Passphrase

	// Orphan cleanup runs on every exit path from here on (success,
	// errors, ctx-cancel, SDEK-lock, capture-stickiness abort).
	// Preserve set:
	//   - capturedID: this pass's blob, may still be load-bearing if
	//     pass aborted or if a newer concurrent rotation hasn't yet
	//     written its own blob.
	//   - liveProbe() at defer time: a concurrent /generate or password
	//     change may have advanced the live id and written a NEWER blob
	//     for that id mid-pass; that blob is the pass-B input and must
	//     not be deleted.
	//
	// Probe-failure handling (codex iter-3 P1 + iter-4 P2 #2): liveProbe
	// returning "" is ambiguous ("no key configured" OR "transient
	// probe failure" — slot-1's SQL probe returns "" on context-cancel
	// during shutdown). If a newer concurrent-rotation blob exists, we
	// can't distinguish it from an orphan without a working probe.
	// Skip cleanup entirely on empty probe; orphans survive one more
	// pass-cycle but no newer blob is lost.
	//
	// TOCTOU protection (codex iter-7 P1): snapshot the directory
	// BEFORE probing the live id. Any blob written by a concurrent
	// rotation AFTER the snapshot is invisible to this cleanup, so the
	// race-arriving new blob cannot be deleted. The probe runs against
	// the snapshot — pre-existing blobs not matching preserve get
	// deleted; post-snapshot blobs stay.
	defer func() {
		snapshot, lerr := listKeyslotBlobs(slot)
		if lerr != nil {
			return
		}
		newer := liveProbe()
		if newer == "" {
			return
		}
		preserve := map[string]struct{}{capturedID: {}}
		if newer != capturedID {
			preserve[newer] = struct{}{}
		}
		for _, id := range snapshot {
			if _, keep := preserve[id]; keep {
				continue
			}
			_ = deleteKeyslotBlob(slot, id)
		}
	}()

	r.audit("auth.keyslot_reconcile_started", map[string]any{"slot": int(slot), "captured_key_id": capturedID})

	vols, err := r.vm.listKeyslotVolumes()
	if err != nil {
		r.setLastErr(slot, "list volumes: "+err.Error())
		return
	}
	r.setStatus(slot, func(s *KeyslotReconciler) KeyslotReconcilerStatus {
		st := s.status[slot]
		st.CapturedKeyID = capturedID
		st.TotalVolumes = len(vols)
		st.Done = 0
		st.Pending = 0
		st.UnprovisionedSteadyState = 0
		return st
	})

	if len(vols) == 0 {
		// No v3 volumes — trivial convergence. Blob persists for any
		// volume-creation race (codex iter-4 P2 #1); next pass with a
		// post-rotation live id cleans it as orphan.
		r.audit("auth.keyslot_reconcile_completed", map[string]any{
			"slot": int(slot), "captured_key_id": capturedID,
			"volume_count_done": 0, "volume_count_skipped_unprovisioned": 0,
		})
		return
	}

	masterKey, err := r.vm.unwrapMasterKey()
	if err != nil {
		r.setLastErr(slot, "unwrap master key: "+err.Error())
		return
	}
	defer cryptoutil.SecureZero(masterKey)

	doneCount := 0
	skippedUnprov := 0
	var allConverged = true

	for _, v := range vols {
		// S5: observe shutdown / context cancellation between volumes.
		// The kill+add itself uses context.WithoutCancel by design (D7);
		// this check only shortcuts the iteration to the next volume on
		// shutdown, not the in-flight kill+add.
		select {
		case <-ctx.Done():
			r.setLastErr(slot, "context cancelled mid-pass")
			return
		default:
		}
		if !r.sdekLoaded() {
			r.audit("auth.keyslot_reconcile_aborted_locked", map[string]any{"slot": int(slot), "captured_key_id": capturedID, "completed_volumes": doneCount})
			r.setLastErr(slot, "SDEK locked mid-pass; will resume on unlock")
			return
		}
		// D12 / F-A1: capture-stickiness — re-read live id before each
		// kill+add. If a rotation arrived mid-walk, abort pass cleanly.
		if liveProbe() != capturedID {
			r.setLastErr(slot, "rotation queued mid-pass; deferring to next pass")
			// Surface the divergence for /crypto/status (rotation storm).
			r.setStatus(slot, func(s *KeyslotReconciler) KeyslotReconcilerStatus {
				st := s.status[slot]
				st.RotationStormPending = maxInt(st.RotationStormPending, 1)
				return st
			})
			r.Nudge() // ensure we re-enter promptly on the new id
			return
		}

		current := metaKeyslotKeyID(v.Meta, slot)
		switch current {
		case capturedID:
			doneCount++
			continue
		case KeyslotKeyIDUnprovisioned:
			skippedUnprov++
			// fall through and provision (add-only via D7 probe)
		case "":
			// pre-RFC unknown — kill+add via D4
		default:
			// stale fingerprint — kill+add
		}

		if err := r.vm.provisionKeyslotOnVolumeByID(ctx, v.ID, v.Meta, int(slot), capturedPP, masterKey); err != nil {
			allConverged = false
			r.setLastErr(slot, fmt.Sprintf("volume %s: %v", v.ID, err))
			r.audit("auth.keyslot_reconcile_failed", map[string]any{
				"slot": int(slot), "captured_key_id": capturedID,
				"volume": v.ID, "err": err.Error(),
			})
			continue
		}
		// Read-modify-write to avoid clobbering concurrent writes to
		// other fields (e.g., resize updating SizeBytes mid-pass —
		// codex iter-7 P2). Stale snapshot from listKeyslotVolumes()
		// is fine for the kskey_id field decision above, but the write
		// must merge with the latest on-disk state.
		latest, rerr := r.vm.readKeyslotMeta(v.ID)
		if rerr != nil {
			allConverged = false
			r.setLastErr(slot, fmt.Sprintf("re-read meta %s: %v", v.ID, rerr))
			continue
		}
		setMetaKeyslotKeyID(latest, slot, capturedID)
		if err := r.vm.writeKeyslotMeta(v.ID, latest); err != nil {
			allConverged = false
			r.setLastErr(slot, fmt.Sprintf("write meta %s: %v", v.ID, err))
			continue
		}
		doneCount++
		// Per-volume Done counter — frontend polls every 3s, so
		// sub-iteration status granularity isn't user-visible. End-of-
		// pass setStatus below captures the final count. Inline write
		// avoids one closure allocation per volume on hot RPi-400 paths.
		r.mu.Lock()
		st := r.status[slot]
		st.Done = doneCount
		r.status[slot] = st
		r.mu.Unlock()
	}

	pending := len(vols) - doneCount
	r.setStatus(slot, func(s *KeyslotReconciler) KeyslotReconcilerStatus {
		st := s.status[slot]
		st.Done = doneCount
		st.Pending = pending
		st.RotationStormPending = 0
		return st
	})

	// Do NOT auto-delete the captured blob on success. The rescan-then-
	// delete pattern is inherently racy: a volume creation between
	// "safeToDelete=true" and "delete" leaves the new volume stamped
	// "unprovisioned" with no blob for the queued follow-up pass to use
	// (codex iter-4 P2 #1). The blob persists at sub-1KB per slot and
	// is cleaned by the next pass's orphan-cleanup defer when a newer
	// live id arrives (this captured blob then becomes orphan against
	// it). Steady-state disk usage: one blob per active rotation.
	_ = allConverged
	_ = pending

	r.audit("auth.keyslot_reconcile_completed", map[string]any{
		"slot": int(slot), "captured_key_id": capturedID,
		"volume_count_done":                  doneCount,
		"volume_count_skipped_unprovisioned": skippedUnprov,
	})
}

func metaKeyslotKeyID(meta *volumeMetaV3, slot KeyslotSlot) string {
	if meta == nil {
		return ""
	}
	switch slot {
	case KeyslotPassword:
		return meta.PasswordKeyslotKeyID
	case KeyslotRecovery:
		return meta.RecoveryKeyslotKeyID
	}
	return ""
}

func setMetaKeyslotKeyID(meta *volumeMetaV3, slot KeyslotSlot, id string) {
	if meta == nil {
		return
	}
	switch slot {
	case KeyslotPassword:
		meta.PasswordKeyslotKeyID = id
	case KeyslotRecovery:
		meta.RecoveryKeyslotKeyID = id
	}
}

func (r *KeyslotReconciler) setStatus(slot KeyslotSlot, fn func(s *KeyslotReconciler) KeyslotReconcilerStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status[slot] = fn(r)
}

func (r *KeyslotReconciler) setLastErr(slot KeyslotSlot, msg string) {
	r.setStatus(slot, func(s *KeyslotReconciler) KeyslotReconcilerStatus {
		st := s.status[slot]
		st.LastError = msg
		return st
	})
}

func (r *KeyslotReconciler) audit(event string, fields map[string]any) {
	if r.auditFn != nil {
		r.auditFn(event, fields)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
