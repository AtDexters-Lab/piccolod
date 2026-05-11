# Async Recovery-Key Keyslot Provisioning

## Scope

**Problem:** `/api/v1/crypto/recovery-key/generate` performs LUKS keyslot 2 provisioning across every device-managed LUKS volume synchronously inside the request handler. Each per-volume `cryptsetup luksAddKey` call is Argon2id-bound (multi-second) — `--master-key-file` bypasses Argon2id only when *unlocking* the existing slot; the **newly-added slot's per-keyslot Argon2id KDF still runs** on the supplied passphrase. On a fast x86 with ~5 volumes the request takes ~50 seconds; on an RPi 400 with ~19 volumes (3 user data + 7 golden snapshots + 8 service rootfs + 1 workspace) it exceeds the 120 s frontend timeout and the request fails outright. The same N×Argon2id shape applies to keyslot 1 (admin password) — `handleAuthPassword` runs slot-1 provisioning under a 30 s `opContext` and `handleCryptoResetPassword` under a 2 min `opContext`; both are guaranteed to time out on devices with enough volumes (≥6 volumes for slot-1 password change on x86). Compounded by a separate bug — commit `10daa79` added the `recovery_ack_at` column with `TEXT NOT NULL DEFAULT ''` and no backfill — every pre-Apr-28-2026 device fires the gate (`computeRecoveryKeyPending` returns true) on every successful login, which calls `/generate`, which rotates the recovery key, which resets `RecoveryAckAt` again, which keeps the gate true. Net effect: pre-existing devices silently rotate their recovery key on every login (invalidating the operator's saved paper words) AND on slow / many-volume hardware they cannot escape the loop because `/generate` itself fails before the operator can reach the ack screen. The user's RPi 400 is in this state today.

The deeper architectural issue: keyslot provisioning across LUKS volumes is a **lifecycle operation**, not a setup operation. The recovery-key mnemonic (slot 2) is regenerated after each password reset via recovery key (`handleCryptoResetPassword`), on operator-initiated rotation, and after first setup. The admin password (slot 1) is provisioned at setup, after recovery-key-driven password reset, and on every admin password change. Both slots' provisioning paths share the same `cryptsetup luksAddKey` primitive and scale identically. Volume count grows monotonically over a device's life (every app install adds 1+ rootfs + 1 data + 1 workspace volume). A synchronous-O(N-volumes) handler — for *either* slot — is on a collision course with timeout windows the moment a device gets non-trivial.

**In scope:**

- Restructure `handleCryptoRecoveryGenerate` so the response returns within sub-second after `cryptoManager.GenerateRecoveryKey` and `applyStalenessUpdate` complete. The recovery-key mnemonic + `keyset.json` rewrap (the **online recovery path**) is the synchronous deliverable.
- Restructure `handleAuthPassword` (`gin_auth_handlers.go:360`, slot-1 provisioning at line 457) and `handleCryptoResetPassword` (`gin_crypto_handlers.go:663`, slot-1 provisioning at line 763) so their slot-1 keyslot work moves out of the request critical path. Both are guaranteed to time out today on devices with enough volumes; this is a live bug surfaced by the F1 review.
- Move LUKS keyslot provisioning (both slot 1 admin-password and slot 2 recovery-mnemonic — the **offline recovery path** — `cryptsetup luksOpen` directly against the LUKS volume) to a background reconciler that walks volumes asynchronously after the triggering handler returns. Reconciler primitive takes the slot number as a parameter so the same code path serves both slots. Reconciler is idempotent, restart-safe across process restart AND system reboot (encrypted-at-rest passphrase persistence on the encrypted control plane for the reconciliation window), and observable.
- Add per-volume `recovery_keyslot_key_id` and `password_keyslot_key_id` metadata recording which generation's passphrase is currently provisioned in each slot. Typed sentinel: `""` = pre-RFC-existing volume (state unknown, first reconcile must kill+re-add); `"unprovisioned"` = volume created in steady-state with no in-flight provisioning (slot is empty); non-empty fingerprint = up-to-date. Distinguishing the two empty-state kinds is the load-bearing primitive that makes status surface and reconciler decisions correct.
- Surface reconciler progress on `/api/v1/crypto/status` as `keyslot_provisioning: { slot1: { current_key_id, total, done, pending, in_progress }, slot2: { ... } }` so the operator can observe offline-recovery-readiness for each slot independently. Counts of "pending" treat sentinel-typed empty values per F6 resolution.
- Volume creation (new app install, new service rootfs, new workspace) provisions both keyslot 1 and keyslot 2 with the current passphrases when in-flight passphrase material is available; otherwise the volume metadata is stamped with the `"unprovisioned"` sentinel and the next operator-initiated provisioning round catches it.
- One-time migration backfill in `ensureAuthStateColumns`: when adding `recovery_ack_at`, if `auth_state.password_hash` is non-empty AND `recovery_ack_at` is empty AND `keyset.json` exists, parses, and has `SDEKRK` set, seed `recovery_ack_at` to NOW. Read-error handling: distinguish "file does not exist" (skip backfill cleanly — first login hits ack flow normally) from "file exists but unreadable / parse-fails / SDEKRK empty" (abort migration with explicit error so the operator attends; do not silently put the device into the regenerate-loop on degraded storage).
- Fix the two fire-and-forget `ackRecoveryKey` call sites (`setup_router.dart:468` in `_handleRecoveryKeyThenPasskey`, `first_run_controller.dart:416` in `proceedAfterRecovery`) to await — symmetric with the awaited site at `setup_router.dart:521` whose comment explicitly warns about the navigation race.
- Tests: regression on the regenerate-loop; reconciler convergence after slot-1 and slot-2 rotation; reboot mid-reconcile resumes via encrypted-at-rest passphrase; kill+add-failure does not leave slot empty; volume-creation atomicity (in-flight and steady-state cases); backfill idempotency under each read-error branch; frontend ack-await on both fixed sites; status surface correctness with sentinel-typed metadata.

**Out of scope:**

- Replacing the BIP39 mnemonic format, the SDEKRK wrap scheme, or `keyset.json` shape. The "online path" remains as today.
- Replacing keyslot 2 with keyslot 3+ rotation (i.e., add new key as keyslot 3, kill keyslot 2 atomically). The B2 fix in this RFC (atomic kill→add boundary via `context.WithoutCancel` plus `luksDump` probe) defends the "slot non-empty across boundary" invariant without keyslot-numbering churn.
- A general "background work scheduler" framework. The reconciler is purpose-built for these two slot-provisioning jobs and lives next to `internal/persistence`; if a third or fourth async job appears later, factoring out is cheap.
- A `POST /api/v1/crypto/recovery-key/refresh-keyslots` endpoint (the user-decided rotate-to-recover trade-off). When a rare gap forms (e.g., backup-restored-from-snapshot mid-reconcile, simultaneous tmpfs corruption AND encrypted-control-plane corruption), the recovery path is operator-initiated `/generate` (rotation), accepting that the just-saved paper words are invalidated. Captured as deferred follow-up if operational data later shows the gap is more frequent than expected.
- Frontend UX redesign of the recovery-key step (the "preparing recovery key…" intermediate state, the rotation-warning banner, etc.). Today's UI flow remains; with the async restructure it just doesn't hang. UX polish noted in deferred follow-ups.
- The 49.72 s LUKS provisioning behavior on the *first device* (boris) in this session — that's the same root cause; this RFC fixes it as a side effect of the async restructure. Not a separate work item.
- `internal/autounlock` integration: spot-check verified null — no references to recovery-key state, `GenerateRecoveryKey`, `recovery-key/generate`, `RecoveryAckAt`, or `computeRecoveryKeyPending` across `internal/autounlock/{blob,scheduler,pickup,orchestrator,ceremony,state,update}`.
- v2 control-plane LUKS volume keyslot management. The existing `provisionKeyslotOnAllVolumes` iterator already skips non-v3 metadata; the reconciler inherits that scope. Offline recovery for the control plane proceeds via SDEKRK + control-plane master-key wrap (online path) since the control plane is the source of `keyset.json`; if the control plane is unrecoverable, neither slot 2 nor SDEKRK helps. Captured as deferred-investigation if a v2 control-plane device requires offline recovery in practice.

---

## Background

### Two paths the recovery key serves

The 24-word BIP39 mnemonic is the operator's only escape if they forget their admin password OR the system disk dies. It functions through two parallel paths:

| Path | Mechanism | Used by | Critical-path role |
|---|---|---|---|
| **Online (SDEKRK in `keyset.json`)** | `mnemonic → derive RK → AES-GCM-unwrap SDEKRK → SDEK → derive per-volume LUKS key` | `/api/v1/crypto/reset-password` while piccolod is running | Primary. Sub-second, scales O(1) regardless of volume count. Only writes one file (`keyset.json`). |
| **Offline (LUKS keyslot 2)** | `cryptsetup luksOpen --key-file <mnemonic> /dev/<volume>` directly | Disaster recovery — pull drives, mount externally, no piccolod running | Defense-in-depth. Scales O(N volumes) because each volume's LUKS metadata is independent. |

Async-with-eventual-consistency is the structurally correct shape: the operator's mnemonic is live the moment `keyset.json` is rewritten; offline-recovery readiness for the new mnemonic catches up in the background.

### Lifecycle of the recovery key

`/generate` is invoked at first setup, post-recovery-key password reset (`handleCryptoResetPassword` clears `RecoveryAckAt`), on operator rotation (`force_rotate=true`), and — the bug — every login on pre-Apr-28 devices. Volume count grows monotonically over a device's life (every app install adds 1+ rootfs + 1 data + 1 workspace volume + service-rootfs subvolumes per app); the RPi 400 case in the scope block (~19 volumes) is the cases-2/3 lifecycle, not first setup.

Step 4 of `handleCryptoRecoveryGenerate` (the per-volume `ProvisionLUKSKeyslot` iteration) is the entire scaling problem; the rest of the handler is O(1).

---

## Architecture

### Decomposition

The same shape applies to both slot 1 (admin password) and slot 2 (recovery mnemonic). For brevity the diagram shows slot 2 (`/generate` trigger); slot 1 is identical with `handleAuthPassword` / `handleCryptoResetPassword` as the synchronous handler instead.

**Step ordering (B3 fix):** the blob is written *before* `keyset.json` is committed for slot 2, and *before* `Rewrap` + `userManager.ChangePassword` for slot 1. If blob write fails, the persistent state-changing op is not attempted; the operator gets a synchronous 5xx and their paper words / current password remain authoritative. If keyset.json commit / Rewrap fails after blob is written, the blob exists but no live key_id references it (live id is unchanged); reconciler treats it as an orphan and deletes at next startup or end-of-pass-success.

**Durability primitive (F-iter3-B1 fix):** the blob write at step 2 uses `fsutil.AtomicWriteFile` — the same primitive `keyset.json` uses at step 3 (write → fsync(file) → rename → fsync(parent dir)). Both files have their data and directory entries durably persisted before the next step proceeds. Without this, a power loss between an `os.WriteFile`-style step 2 returning nil (data still in OS page cache) and step 3's durable `keyset.json` commit could leave the device with `keyset.json` reflecting key_id NEW but the blob's data block zero/truncated/absent — at which point the operator's just-saved paper words match the live `RecoveryKeyID()` (online path works) but offline-recovery cannot converge, identical in operator-impact to the iter-2 B3 hazard the new ordering exists to eliminate. The `<core>/mounts/control-plane/recovery/` directory and `<core>/crypto/` directory are independent durability domains; the keyset write's parent-dir fsync does not cover the blob's directory entry.

```
                          ┌───────────────────────────┐
                          │  /generate (synchronous)  │
                          ├───────────────────────────┤
                          │ 1. compute candidate      │  ~50 ms
                          │    (mnemonic, SDEKRK,     │
                          │     key_id) in-memory     │
                          │ 2. write encrypted blob   │  ~10 ms
                          │    keyed by candidate     │
                          │    key_id to encrypted    │
                          │    control plane          │
                          │ 3. atomic-write           │  ~5 ms
                          │    keyset.json (commits)  │
                          │ 4. applyStalenessUpdate   │  ~10 ms
                          │ 5. nudge reconciler       │  non-blocking
                          │ 6. respond                │  total <100 ms
                          └────────────┬──────────────┘
                                       │ (returns to UI; operator sees words)
                                       │
                                       │ async, decoupled from request lifetime
                                       ▼
                          ┌───────────────────────────┐
                          │ Keyslot reconciler        │
                          ├───────────────────────────┤
                          │ - per slot {1, 2}:        │
                          │   - read pending blobs    │
                          │   - capture (key_id, pp)  │
                          │     pair atomically       │
                          │   - walk LUKS volumes     │
                          │   - per-volume:           │
                          │     if v.kskey_id[N] !=   │
                          │       captured.key_id:    │
                          │         atomic kill+add   │
                          │         set v.kskey_id[N] │
                          │   - delete blob on        │
                          │     full convergence      │
                          │ - converges, idempotent   │
                          └───────────────────────────┘
```

### Data shape additions

Per-volume metadata gains two fields (one per slot). Volume metadata is the v3 `volumeMetaV3` struct in `internal/persistence/luks_volume_manager.go`. New fields:

```
PasswordKeyslotKeyID string  // generation fingerprint currently in slot 1
RecoveryKeyslotKeyID string  // generation fingerprint currently in slot 2
                             // typed sentinel:
                             //   "" = pre-RFC-existing volume (state unknown,
                             //         first reconcile must kill+re-add)
                             //   "unprovisioned" = volume created in steady-state
                             //         with no in-flight provisioning (slot empty)
                             //   non-empty fingerprint = up-to-date
```

The fingerprint format for slot 2 already exists: `fingerprintSDEKRK` (manager.go:442) computes `sha256(SDEKRK base64)` first 16 hex chars and is what `RecoveryKeyID()` returns. For slot 1, we introduce a parallel `fingerprintPasswordHash` (`sha256(PasswordHash)` first 16 hex chars) computed at password-set time and exposed via `cryptoManager.PasswordKeyslotID()` or equivalent. Reusing the fingerprint vocabulary across slots keeps "current key_id" comparisons uniform.

The two empty-state sentinels (`""` vs `"unprovisioned"`) carry distinct reconciler semantics: `""` means "kill+re-add unconditionally" (we don't know what's in the slot — pre-RFC could have any old generation); `"unprovisioned"` means "add only" (we created the volume; slot is known empty; skip the kill).

### Reconciler shape

Lives next to `internal/persistence/luks_volume_manager.go` (extends the existing volume reconciliation surface; no new package). Single goroutine, signaled by:

- Process startup (catches up volumes that were dirty when the previous process died OR when the system rebooted mid-pass)
- `/generate` completion (slot-2 trigger)
- `handleAuthPassword` / `handleCryptoResetPassword` completion (slot-1 trigger)
- Volume creation completion (new volume needs initial provisioning if blobs are present)

Convergence model: each pass enumerates the slots with pending blobs (slot 1 and/or slot 2), and for each, captures the `(key_id, passphrase)` pair atomically by reading the blob ONCE per pass under a lock. Per-volume decision: if `volume.kskey_id[N] == captured.key_id` skip; if `volume.kskey_id[N] == "unprovisioned"` add-only (no kill); otherwise (pre-RFC `""` or stale fingerprint) atomic kill+add. On per-volume success, set `volume.kskey_id[N] = captured.key_id`. Per-volume failures leave the field unchanged so the next pass retries; whole-pass failures (SDEK not loaded, no blob, ctx cancelled at the pass boundary) abort cleanly without setting any volume's field. Idempotent: repeated invocations are no-ops once converged.

**Atomic kill→add boundary (B2 fix):** the kill→add pair within `luksSetKeyslot` (currently `luks_volume_manager.go:1399-1415`) is wrapped in `context.WithoutCancel` so a reconciler nudge or process-exit signal mid-pair does not split the operation and leave the slot empty. Additionally, before the kill, a `cryptsetup luksDump` probe checks whether the target slot is occupied — if not (sub-case ii / `"unprovisioned"` sentinel), the kill is skipped entirely. Net invariant: the slot transitions atomically from old-passphrase to new-passphrase OR stays at old-passphrase; it never transitions through empty.

**Lock-state-aware reconciliation (S4 fix):** the reconciler subscribes to (or polls) `cryptoManager.SDEKLoaded()`. On `SDEKLoaded()` flipping false mid-pass (operator-initiated lock, idle-lock policy, etc.), abort the current pass cleanly at the next per-volume boundary — do not start a new per-volume cryptsetup operation while SDEK is unavailable, and do not stamp any volume's `kskey_id` after the lock event. Resume on next nudge after Unlock. The captured passphrase blob remains on the encrypted control plane (which itself was just locked, but the blob persists — see passphrase-transport below).

**Passphrase-transport (D6, B1 fix):** for each pending slot, `/generate` (or the slot-1 handlers) writes a passphrase blob to `<core>/mounts/control-plane/recovery/keyslot-pending-<slot>-<key_id>.blob`, encrypted under SDEK using the same AEAD primitive as keyset.json. Filename includes both slot and key_id so concurrent rotations on different slots cannot collide and a force_rotate on slot 2 cannot overwrite a still-pending slot 2 blob (concurrent slot-2 force_rotate within a window where the prior pass hasn't completed: the latest blob's key_id wins; reconciler reads the highest-key_id blob for the slot, drains, deletes it AND any older blobs for the same slot). Blob is deleted on full per-slot convergence (every volume's `kskey_id[N]` matches that blob's key_id). Restart-safe across system reboot: blobs live on the encrypted control plane (LUKS-encrypted at rest), so reboot wipes only the SDEK in RAM, not the blobs themselves; on next unlock + reconciler startup, blobs are re-read and reconciliation resumes.

**Per-pass `(key_id, passphrase)` pair-read semantics (F3, S1 fix):** within a single pass, the reconciler captures key_id and passphrase from the blob's filename and contents respectively in a single read; if a `/generate` fires mid-pass and writes a newer blob, the reconciler does NOT pick up the new blob during the current pass — it finishes against the captured pair, then re-enters on the next nudge and picks up the latest. Status surface counts (`done`, `pending`) are reported against the captured `key_id`, not the current `cryptoManager.RecoveryKeyID()`, so concurrent rotations don't make the surface lie.

**Blob enumeration rules (F-A2 fix):** sha256-prefix fingerprints have no temporal ordering; "highest" / "latest" are not properties the implementer can compute from the blob name alone. Two precise rules instead:

- *Capture rule:* at pass start, the reconciler reads the blob whose filename `key_id` matches the live `cryptoManager.RecoveryKeyID()` (slot 2) / `cryptoManager.PasswordKeyslotID()` (slot 1) for the slot. Any blob whose `key_id` does not match the live id is an orphan.
- *Cleanup rule:* at process startup AND at end-of-pass-success, delete every `keyslot-pending-<slot>-*.blob` whose `key_id` does not match the live manager's id for that slot.

A pass with no matching blob aborts cleanly (logs the slot and the live id) and waits for the next nudge — typical post-restart state, the next `/generate` or password change writes a fresh matching blob.

**Per-volume capture-stickiness (F-A1 fix):** before each per-volume kill+add, the reconciler re-reads `cryptoManager.RecoveryKeyID()` (slot 2) / `cryptoManager.PasswordKeyslotID()` (slot 1) and compares to the captured `key_id` from the blob open. If the live id has advanced past captured (a `/generate` or password change fired mid-walk), abort the pass cleanly without writing the volume — pass-B will pick up the newer blob and converge. This prevents the regression where pass-A reverts a volume from NEW (stamped via sub-case-i hook) back to OLD because the per-volume decision was rule "kskey_id != captured ⇒ overwrite" without a freshness check on captured itself.

This same check covers the case where a volume's `kskey_id[N]` was `"unprovisioned"` and a sub-case-i hook upgraded it to NEW between pass-A's capture and pass-A reaching that volume — the freshness check makes pass-A defer to pass-B rather than reverting.

### Status surface

Add to `/api/v1/crypto/status` (site list below):

```
{
  ...,
  "keyslot_provisioning": {
    "slot1": {
      "captured_key_id": "abc123def",  // what the current/last reconciler pass is converging to
      "target_key_id":   "abc123def",  // live cryptoManager.PasswordKeyslotID() (target state)
      "total_volumes": 19,
      "done": 12,                      // volumes with kskey_id == captured_key_id
      "pending": 7,                    // volumes with kskey_id != captured_key_id
                                       // (excludes "unprovisioned" sub-case ii volumes
                                       //  per F6 sentinel resolution)
      "unprovisioned_steady_state": 0, // sub-case ii count, surfaced separately
      "rotation_storm_pending": 0,     // count of newer blobs queued behind the
                                       // current capture (target_key_id != captured_key_id)
      "in_progress": true,
      "last_error": ""
    },
    "slot2": { /* same shape */ }
  }
}
```

**Captured vs target distinction (S5 fix):** during a rotation storm (operator triggers `/generate` or password change repeatedly), the reconciler is honestly walking against `captured_key_id` while the live `target_key_id` may have advanced. Surfacing both makes the divergence operator-comprehensible: `captured == target` means "no rotation queued, reconciler is fully converging on the latest"; `captured != target` means "rotation storm in progress, reconciler will pick up the latest after the current pass completes." `rotation_storm_pending` is a count of blobs newer than captured that are queued behind it.

`/api/v1/system/boot` surfaces:

- `recovery_key_pending` (the *ack* gate — unchanged)
- `keyslot_unprovisioned_count` (S7 fix): sum of `unprovisioned_steady_state` across slots. When non-zero, the UI surfaces a hint on the home/login surface ("your recovery key doesn't cover N newly-installed volumes — rotate to update") so operators who don't visit the crypto status panel still see the gap. Without this, sub-case-ii volumes silently accumulate over the device's life and the rotate-to-recover policy becomes a recovery path requiring foreknowledge of the gap (which most operators won't have).

Status is informational for `keyslot_provisioning.in_progress` and `pending`; the operator can ack words and proceed to desktop while keyslot reconciliation is still draining. The `keyslot_unprovisioned_count` boot surface is *also* informational (does not block boot), but is more visibly placed than `/crypto/status` so it actually gets seen.

`unprovisioned_steady_state` is intentionally separated from `pending` because the recovery story differs: `pending` volumes will converge automatically as soon as the reconciler reaches them; `unprovisioned_steady_state` volumes will stay unprovisioned until the operator initiates a new rotation (per the user-decided rotate-to-recover policy).

### Volume-creation atomicity

When `EnsureVolume` (or whichever create path lands a new LUKS volume — site list below) finishes, it provisions slots 1 and 2 with available passphrase blobs before returning:

- **Sub-case i**: a passphrase blob for slot N exists on the encrypted control plane (a `/generate` or password change is in flight, OR a previous unfinished pass left a blob). Volume creation reads it via the same path the reconciler uses, provisions slot N, sets `volume.kskey_id[N] = blob.key_id`. Atomic-with-creation, reusing the existing `luksSetKeyslot` primitive (no new tmpfs primitives — F9 resolution).
- **Sub-case ii**: no passphrase blob for slot N. Volume is stamped with `volume.kskey_id[N] = "unprovisioned"`. Status surface counts it in `unprovisioned_steady_state`. The volume's slot N is genuinely empty — no kill needed at next provisioning round; the reconciler will add-only (per the sentinel-typed semantics in §Data shape additions).

Sub-case ii's recovery is operator-initiated rotation per the user-decided rotate-to-recover policy. Until then, the operator's paper-saved words for the current generation work for `/reset-password` (online path, all volumes) but do NOT work for `cryptsetup luksOpen` directly against this newly-created volume (offline path). The status surface's `unprovisioned_steady_state` count is the operator's signal.

### Backfill migration

In `ensureAuthStateColumns` (sqlite_control_store.go:307), after `addColumn("recovery_ack_at", ...)` succeeds, run a one-shot backfill UPDATE keyed on three conditions:

1. `auth_state.password_hash` is non-empty (rules out fresh `INSERT ... ON CONFLICT DO NOTHING` rows that haven't been set up yet).
2. `auth_state.recovery_ack_at` is empty (the column was just added or this is a pre-existing migrated row; either way no ack on record).
3. `keyset.json` evaluation at `<core>/crypto/keyset.json` (read once per migration to avoid TOCTOU between this check and the UPDATE):

| `keyset.json` state | Action | Rationale |
|---|---|---|
| File does not exist | Skip backfill, continue migration | Device genuinely never generated a recovery key. First login hits the ack flow normally. |
| File exists, parses, `SDEKRK` non-empty | Backfill UPDATE | Pre-Apr-28 device with an ack-worthy key; this is the target population. |
| File exists, parses, `SDEKRK` empty | Skip backfill, continue migration, log WARN | Partial setup state — keyset.json was rewritten by a prior `/setup` retry with an interruption before the SDEK wrap. Operator should hit normal recovery-key flow. |
| File exists but unreadable, unparseable, or corrupt (I/O error, invalid JSON, invalid base64 in SDEKRK) | **Abort migration with explicit error** | Storage or state corrupted; silent skip would put the device into the regenerate-loop the RFC exists to prevent. Forcing the operator to attend at this point is the right venue — the device is showing degradation symptoms regardless. |

When the conditions hold (case 2 above), set `recovery_ack_at` to NOW. The filesystem read at migration time lives outside the encrypted control-plane mount and is accessible from `applyMigrations`, so no layering inversion.

**Migration site coupling:** `ensureAuthStateColumns` runs inside the SQLite migration; `keyset.json` is at `<core>/crypto/keyset.json` — outside the encrypted control-plane mount, accessible during migration.

**Operator recovery surface for migration abort (F-A4 / S6 / S9 / F-iter3-S1 fix):** the abort posture is correct (silent skip on degraded storage would put the device into the regenerate-loop the RFC exists to prevent), but the operator's path to attend it must be defined:

1. **Bounded retry before declaring abort:** the keyset.json read attempts up to 3× with exponential backoff (250 ms / 1 s / 4 s) to absorb transient I/O glitches. Persistent failure across the retries is the actual abort trigger. Single I/O blip on a marginal SD card does not brick the device.
2. **Degraded-startup, not fail-to-start:** when abort triggers, `applyMigrations` returns a typed `ErrAuthMigrationDegradedStorage` rather than a generic error. The persistence layer surfaces this via `health.Tracker` as a `LevelError` event (`auth.migration_aborted_storage_health`). piccolod completes startup in a degraded-admin-only state — the web UI renders, the operator can read the health event, and ssh / serial console access remains available for diagnostic. The login surface shows a clear copy: "Storage health prevented automatic upgrade. Saved recovery words remain valid; please contact support or attend the device console." This avoids the "device unbootable" footgun on remote-installed appliances while preserving the abort posture's correctness for the auth-migration step.
3. **Audit event:** `auth.migration_keyset_unreadable` (path, errno, retry_count) for forensics.

**Architectural locus (Lens-5 reconciliation):** `applyMigrations` runs inside `sqliteControlStore.Unlock(ctx)` at `internal/persistence/sqlite_control_store.go:429-466` — *not* inside `NewService`. The HTTP listener is up *before* `Unlock` fires (the operator is on the locked screen pre-unlock). Degraded-startup is therefore architecturally natural: the locked-screen UI is the rendering surface, and the `Unlock` error response is the wire from `ErrAuthMigrationDegradedStorage` to the operator banner. No new degraded-mode persistence layer or special handler-tolerance is required — the existing pre-unlock UI surface already runs without requiring `auth_state`, and the migration abort surfaces through the same return path that today's "wrong password" / "ErrLocked" responses use.

**Wiring sites (default-by-omission closure for S9 / F-iter3-S1):**
- `internal/persistence/sqlite_control_store.go:Unlock` — propagates the typed `ErrAuthMigrationDegradedStorage` back through the unlock RPC; existing callers that handle `Unlock` errors gain a typed-error case alongside `ErrInvalidPassword` etc.
- `internal/health/tracker` (or wherever `health.Tracker` registration lives today) — registers the `auth.migration_aborted_storage_health` topic.
- Frontend locked-screen banner — reads the health event via `/api/v1/system/boot` (extended to surface the typed degraded-storage condition) and renders the bridging copy.

The recovery is "operator boots, sees the warning on the locked-screen UI, attends storage health (replace SD, restore from backup, etc.), restarts piccolod after fixing." The migration retries on next start; if the underlying storage issue is resolved, it succeeds and clears the health event.

The backfill timestamp = now-at-migration-time. Semantically: "we noticed this device pre-dated the ack mechanism and has a usable recovery key on disk; we're recording 'ack'd at upgrade'." Operator may or may not actually have saved the words from initial setup — this RFC doesn't and can't know that. The trade-off: silently treat them as ack'd (avoid the hostile silent rotation) vs. force them into the ack flow on next login (reset gate, run /generate, rotate, invalidate paper words). Of those two: the silent-treat-as-ack'd is strictly safer for the operator's actual paper words. If they don't have paper, the password-stale gate (separately) will eventually prompt them to rotate intentionally; that's the right venue for "set up your recovery key now."

### Frontend ack-await fix

Two sites awaited inconsistently with the third (which has the explicit warning comment):

```
ui/lib/shells/desktop/features/setup/setup_router.dart:468  unawaited(ackRecoveryKey(keyId));
ui/lib/shells/desktop/features/setup/setup_router.dart:521  await ackRecoveryKey(keyId);  // ← correct
ui/lib/shells/desktop/features/setup/controllers/auth_controller.dart:428  await ackRecoveryKey(keyId);  // ← correct
ui/lib/shells/desktop/features/setup/controllers/first_run_controller.dart:416  unawaited(ackRecoveryKey(...));
```

Fix: same `unawaited(() async { await ackRecoveryKey(keyId); ... }())` shape used in `auth_controller.dart:419-431`. The post-ack action (route transition / onComplete) goes inside the closure, after the await.

Reproducer for the race: OIDC remote setup flow. Operator hits passkey-required after recovery-key step → `_handleRecoveryKeyThenPasskey` fires unawaited ack → operator registers passkey within ~5 s → `_completeAuthAndRedirect` assigns `window.location.href = redirectUrl` → in-flight ack POST is browser-aborted → `RecoveryAckAt` stays zero → next login regenerates. Independently of the migration bug, this race exists.

---

## Decisions

### D2. Reconciler extends `internal/persistence`

Rationale: every LUKS-volume operation already lives there (`luksVolumeManager`). The reconciler reads volume metadata, walks volume handles, calls `luksSetKeyslot`. No new package; no new abstraction. Slot number is a parameter so the same reconciler serves both keyslot 1 (admin password) and keyslot 2 (recovery mnemonic).

### D3. Backfill IS required even with /generate now fast

Rationale: even with /generate fast, pending=true on a pre-Apr-28 device would still trigger a rotation that invalidates the operator's saved paper words. Backfill seeds RecoveryAckAt so we don't auto-rotate devices that already had ack'd-during-setup-but-pre-this-mechanism words. Async + backfill are independent fixes for independent bugs.

### D4. Existing-volume migration: kill+re-add at first reconcile, with atomic boundary

Rationale: `cryptsetup luksDump` can probe whether a slot is occupied but cannot tell us *which* generation is in it (LUKS metadata stores PBKDF parameters and a digest, not a fingerprint we can compare against `cryptoManager.RecoveryKeyID()` / `PasswordKeyslotID()`). At first reconcile after this RFC ships, `volume.kskey_id[N] = ""` for every existing volume; reconciler unconditionally kill+re-adds.

The kill→add pair is wrapped in `context.WithoutCancel` so a reconciler nudge or process-exit signal mid-pair cannot tear the pair apart and leave the slot empty (B2 fix). For sub-case ii (`"unprovisioned"` sentinel), the kill is skipped entirely via a pre-kill `luksDump` probe — add-only, no risk of teardown-empty.

### D5. New volumes provision both keyslots atomically with creation if blob available

Captured in §Volume-creation atomicity. The two sentinel-typed empty values (`""` for pre-RFC-existing volumes, `"unprovisioned"` for sub-case-ii steady-state new volumes) make the reconciler's kill-vs-add-only decision deterministic per F6 resolution. Sub-case ii's recovery is operator-initiated rotation per the user-decided rotate-to-recover policy; counted in `unprovisioned_steady_state` on the status surface to make the gap operator-visible.

### D6. Passphrase transport: encrypted blob on encrypted control plane (B1 fix)

For each pending slot, the synchronous handler writes `<core>/mounts/control-plane/recovery/keyslot-pending-<slot>-<key_id>.blob`, encrypted under SDEK using the same AEAD primitive that protects `keyset.json`. Restart-safe across system reboot (the encrypted control plane survives reboot; only the SDEK in RAM is wiped). On next unlock + reconciler startup, blobs are re-read and reconciliation resumes.

Rejected alternatives:
- *tmpfs file* — original D6 in iter 1; rejected because `/run/piccolo` is RAM-backed and does not survive system reboot. A reboot mid-reconcile would lose the passphrase, and per the rotate-to-recover policy the operator's only recourse would be a forced rotation (invalidating just-saved paper words). Encrypted-on-disk closes this gap without a new endpoint.
- *Keep mnemonic in process memory only* — defeats restart-safety entirely.
- *Persist plaintext on encrypted control plane* — the at-rest encryption is sufficient (the disk is LUKS-encrypted, the control plane is doubly encrypted via SDEK), but symmetry with how `keyset.json` itself is treated (plaintext-on-encrypted-mount) would be acceptable. Choosing AEAD-under-SDEK adds belt-and-suspenders against backup-restore exposure of the control-plane volume in unencrypted form.

The blob's lifetime is bounded: written by the synchronous handler, deleted on full per-slot convergence. Concurrent rotation collisions handled per the §Reconciler shape "Concurrent-rotation cleanup" rule (key_id-named blobs, latest wins, orphan cleanup at process startup).

### D7. Atomic kill→add boundary defended via `context.WithoutCancel` + pre-kill probe

(B2 fix.) The kill→add pair within `luksSetKeyslot` is the critical span where the slot is transiently empty. Two defenses combine:

1. **`context.WithoutCancel` around the pair**: the kill and the subsequent re-add cannot be interrupted by reconciler nudges, process-exit signals, or per-pass timeout cancellation. A SIGKILL is still possible (out-of-band-fatal) but the explicit-cancellation case — by far the more common failure mode — is closed.
2. **Pre-kill `luksDump` probe**: if the slot is already empty (sub-case ii / `"unprovisioned"` sentinel), skip the kill entirely. Net invariant: the slot transitions atomically from old-passphrase to new-passphrase OR stays at old-passphrase; it never transitions through empty *under the explicit-cancellation failure modes the reconciler is responsible for*. SIGKILL during the Argon2id window remains the residual hazard; rotate-to-recover policy is the documented recourse.

### D8. Lock-state-aware reconciliation (S4 fix)

The reconciler aborts cleanly at the next per-volume boundary if `cryptoManager.SDEKLoaded()` flips false mid-pass. It does not start new per-volume cryptsetup operations under a locked SDEK, and does not stamp any volume's `kskey_id` after the lock event. Resumes on next nudge after Unlock. The encrypted blob persists across the lock-unlock cycle (per D6).

The reconciler subscribes to `cryptoManager` lock-state changes via the existing event channel (or polls per-volume if no channel exists today; either is acceptable, per-volume polling adds a single SDEKLoaded() check per cryptsetup operation). Captured passphrase from the blob is dropped from process memory at pass-abort to bound exposure.

### D9. Per-pass `(key_id, passphrase)` pair-read is atomic at blob open (F3, S1 fix)

Within a single reconciler pass, the reconciler captures `key_id` (from the blob filename) and `passphrase` (from the blob's decrypted contents) under a single read. If a `/generate` fires mid-pass and writes a newer blob, the reconciler does NOT pick up the new blob during the current pass — it finishes against the captured pair, then re-enters on the next nudge. Status surface counts (`done`, `pending`, `current_key_id`) are reported against the captured `key_id`, not the live `cryptoManager.RecoveryKeyID()`, so concurrent rotations don't make the surface lie.

### D10. Slot-1 reconciler input is the new admin password's keyslot-1 fingerprint

(Slot-1 expansion.) `handleAuthPassword` and `handleCryptoResetPassword` write a passphrase blob for slot 1 ahead of committing the password change (per D11), the same primitive `/generate` uses for slot 2. The blob's key_id is `fingerprintPasswordHash(new_hash)` (introduced in §Data shape additions). This makes slot-1 rotation observable, reconcilable, and restart-safe under the same primitives as slot-2.

Distinct from slot 2 in one respect: slot 1's passphrase is the literal admin password (not derived from a mnemonic), which we already hold in plaintext for the duration of the change request. The blob writeback uses the same SDEK-AEAD wrap as slot 2; the operator's plaintext password is never persisted unencrypted.

### D11. Blob-write-first ordering: blob write is the precondition, not the consequence (B3, F-A3 fix)

The synchronous handler writes the encrypted blob *before* committing the persistent state-changing operation. Specifically:

| Slot 2 (`/generate`) | Slot 1 (`handleAuthPassword`, `handleCryptoResetPassword`, `handleCryptoSetup`) |
|---|---|
| 1. Compute candidate `(mnemonic, SDEKRK, key_id)` in-memory (no disk write yet) | 1. Compute candidate `(new_password_hash, fingerprint)` in-memory |
| 2. Write blob `keyslot-pending-2-<key_id>.blob` | 2. Write blob `keyslot-pending-1-<fingerprint>.blob` |
| 3. AtomicWrite `keyset.json` with new SDEKRK | 3. Update `password_hash` in userManager + `Rewrap(old, new)` keyset.json |
| 4. `applyStalenessUpdate` | 4. (no staleness change for slot 1 password change; reset-password path is its own update) |
| 5. Nudge reconciler, return | 5. Nudge reconciler, return |

If step 2 (blob write) fails, the handler returns 5xx and **no state-changing operation has fired** — the operator's existing paper words / current password remain authoritative. If step 3 fails after step 2 succeeded, the blob exists but no live key_id references it (live id unchanged) — reconciler treats it as an orphan per F-A2's cleanup rule and deletes at next startup. State is operator-equivalent to "step 2 failed."

Step 2's durability primitive is `fsutil.AtomicWriteFile` (the same primitive used by `keyset.json` and the volume metadata files in `internal/persistence/luks_volume_manager.go`): write to temp + fsync(file) + rename + fsync(parent dir). The blob's data and directory entry are both durable before step 3 begins. Buffered writes (e.g., `os.WriteFile` returning nil while data is still in OS page cache) are not sufficient — a post-step-2-pre-step-3 power loss with buffered semantics can leave keyset.json=NEW with a blob whose data block is zero/truncated, defeating the iter-2 B3 fix. Test plan covers this via fault-injection (kill -9 between steps).

`cryptoManager.GenerateRecoveryKey` is refactored to expose two phases: `PrepareRecoveryKey()` returns `(mnemonic, sdekrk_candidate, key_id)` without writing keyset.json; `CommitRecoveryKey(sdekrk_candidate)` performs the atomic write. Single-mutex serialization is preserved across the pair so concurrent `/generate` calls cannot interleave their candidates.

For slot-1 password change, the equivalent split is `PreparePassword(new_password) → (new_hash, fingerprint)` followed by the existing `Rewrap(old, new)` + `userManager.ChangePassword` after blob write. (`PreparePassword` is a thin wrapper over the existing hash function; the split is purely for ordering symmetry.)

### D12. Per-volume capture-stickiness check (F-A1 fix)

Before each per-volume kill+add, the reconciler re-reads the live `cryptoManager.RecoveryKeyID()` (slot 2) / `cryptoManager.PasswordKeyslotID()` (slot 1) and compares to the captured `key_id` from the blob open. If the live id has advanced past captured, the pass aborts cleanly without writing the volume — pass-B will pick up the newer blob and converge.

This closes the F-A1 regression where pass-A could revert a volume from NEW (stamped via sub-case-i hook between pass-A's capture and reaching that volume) back to OLD because the per-volume rule was "kskey_id != captured ⇒ overwrite" without freshness on captured itself.

The check fires per volume, not per pass-start, because a pass on a 19-volume RPi 400 walks for ~90 s — enough time for several rotations to queue. Per-pass freshness alone would not catch a rotation that arrived 30 s into the walk.

### D13. Blob format includes a version byte (S8 fix)

Encrypted blob format: `version_byte || nonce || AEAD(plaintext, AAD = slot || key_id || version_byte)`. Version byte starts at `0x01`. Future format changes (additional metadata, different AEAD primitive, structured envelope) bump the version; reconcilers that don't recognize the version reject the blob (treated as orphan, log WARN). This is cheap insurance against in-flight blobs at upgrade time and aligns with the discipline already applied to `keyset.json` (which is JSON-versionable via Go's tolerant unmarshalling).

---

## Site list

Sites that read, write, or compose with the new behavior:

### Read sites (must observe new fields / new behavior)

- `internal/server/gin_crypto_handlers.go:handleCryptoRecoveryGenerate` (line 858) — restructured; no longer iterates volumes; writes encrypted slot-2 blob, nudges reconciler.
- `internal/server/gin_crypto_handlers.go:handleCryptoResetPassword` (line 663, slot-1 site at 763) — restructured; no longer iterates volumes for slot 1; writes encrypted slot-1 blob, nudges reconciler.
- `internal/server/gin_auth_handlers.go:handleAuthPassword` (line 360, slot-1 site at 457) — restructured; 30 s `opContext` no longer holds the request through N×Argon2id; writes encrypted slot-1 blob, nudges reconciler.
- `internal/server/gin_crypto_handlers.go:handleCryptoSetup` (line 314, slot-1 site at 500) — restructured to use the same blob+nudge path so first-setup keyslot 1 also goes through the reconciler. (At first setup there's typically only the control-plane volume, so the latency benefit is small, but uniformity is correct.)
- `internal/server/gin_crypto_handlers.go:handleCryptoStatus` (or wherever `crypto/status` is wired today) — emit `keyslot_provisioning` block (slot1 + slot2 sub-objects with sentinel-typed counts).
- `internal/server/gin_boot_handler.go:handleSystemBoot` — unchanged; `recovery_key_pending` semantics unchanged.
- `internal/server/staleness_helpers.go:computeRecoveryKeyPending` — unchanged.
- `internal/persistence/luks_volume_manager.go` (new reconciler) — reads volume metadata, captures `(slot, key_id, passphrase)` from blobs.

### Write sites (generators of new state)

- The four restructured handlers above — write encrypted slot-N blobs to `<core>/mounts/control-plane/recovery/keyslot-pending-<slot>-<key_id>.blob`, nudge reconciler, return.
- `internal/persistence/luks_volume_manager.go:luksSetKeyslot` (line 1372) — modified to wrap kill→add pair in `context.WithoutCancel` and pre-kill `luksDump` probe (D7).
- `internal/persistence/luks_volume_manager.go:EnsureVolume` (line 329) — sub-case i atomicity hook reads any pending blobs at creation time, provisions both slots, stamps `volume.kskey_id[1,2]` accordingly. Sub-case ii stamps `"unprovisioned"`.
- New reconciler — writes `volume.kskey_id[N]` after each per-volume success per slot; deletes the per-slot blob on full convergence; cleans up orphan blobs at process startup (per §Reconciler shape "Concurrent-rotation cleanup").
- `internal/persistence/sqlite_control_store.go:ensureAuthStateColumns` (line 307) — backfill block with the read-error-distinguishing logic per §Backfill migration.
- `internal/persistence/sqlite_control_store.go:Unlock` (line 429) — propagates the typed `ErrAuthMigrationDegradedStorage` when `applyMigrations` aborts after retry exhaustion, alongside existing `ErrInvalidPassword` / `ErrLocked` cases.
- `internal/health/tracker` (or current `health.Tracker` registration site) — register `auth.migration_aborted_storage_health` topic; `LevelError` event emitted on abort.
- `internal/server/gin_boot_handler.go:handleSystemBoot` — when the typed degraded-storage condition is set, surface it on the boot response so the locked-screen UI can render the bridging banner.
- `ui/lib/shells/desktop/features/setup/setup_router.dart` (or the locked-screen step) — render a status banner when the boot response signals degraded-storage; copy: "Storage health prevented automatic upgrade. Saved recovery words remain valid; please contact support or attend the device console."
- `internal/persistence/luks_volume_manager.go:writeVolumeMetaV3` (line 1617) — extended to include `password_keyslot_key_id` and `recovery_keyslot_key_id`.
- `internal/crypt/manager.go` — new `PasswordKeyslotID()` method paralleling `RecoveryKeyID()` (line 538), backed by `fingerprintPasswordHash`. Also `PrepareRecoveryKey` / `CommitRecoveryKey` two-phase split per D11.
- All synchronous-handler blob writes use `fsutil.AtomicWriteFile` (file-fsync + rename + parent-dir-fsync) — same primitive as `keyset.json` and volume metadata files.

### Composing sites (depend on the new behavior being correct)

- `internal/autounlock/*` — verified null: grep across `internal/autounlock/{blob,scheduler,pickup,orchestrator,ceremony,state,update}` returns no references to recovery-key state, `GenerateRecoveryKey`, `recovery-key/generate`, `RecoveryAckAt`, or `computeRecoveryKeyPending` (F8 resolution).
- Frontend `ackRecoveryKey` consumers (3 sites) — 2 fix-ups (`setup_router.dart:468`, `first_run_controller.dart:416` to `await` per the awaited-site pattern at `setup_router.dart:521`).

### Deferred / explicit non-sites

- `internal/server/gin_passkey_handlers.go:handlePasskeyLoginFinish` — surfaces `recovery_key_pending` from `computeRecoveryKeyPending`; downstream behavior unchanged. Not modified.
- `handleAuthLogin` — same as above. Not modified.
- v2 control-plane volume reconciliation — out of scope per §Out-of-scope (existing iterator skip preserved); operator's offline-recovery story for v2 control-plane is via SDEKRK/keyset.json (online path) not LUKS keyslot 2.

---

## Migration plan

1. Ship the SQL backfill in the same release as the async restructure and slot-1 expansion. Pre-Apr-28 devices that upgrade get backfilled on first boot under the new code; gate stops firing.
2. First operator-initiated provisioning after upgrade (case 2 lifecycle: post-password-reset for slot 2; password change for slot 1) walks all existing volumes via the reconciler. One-time cost, ~50 s on x86 / ~90 s on RPi 400 — but in the **background**, not blocking the user. Operator sees words / completes password change within sub-second, lands on desktop.
3. From upgrade onward, every keyslot rotation is fast at the request layer; reconciler picks up the delta.
4. Pre-existing operators on pre-Apr-28 devices whose words were already silently rotated multiple times have the *current* on-disk mnemonic from the most recent rotation. They won't know it's been rotated; their paper words are stale. There is no programmatic recovery for that data loss — it has already happened. The forward fix prevents future occurrences.

### Test plan

- **Backfill idempotency:** unit test `ensureAuthStateColumns` against the matrix in §Backfill migration: (i) password_hash + empty recovery_ack_at + valid keyset.json → backfilled to NOW; (ii) password_hash + non-empty recovery_ack_at → unchanged; (iii) empty password_hash → unchanged; (iv) password_hash + empty recovery_ack_at + no keyset.json → unchanged (genuinely no key to ack); (v) password_hash + empty recovery_ack_at + keyset.json with empty SDEKRK → unchanged + WARN log; (vi) password_hash + empty recovery_ack_at + unreadable keyset.json → migration aborts with explicit error.
- **Regenerate-loop regression:** integration test simulating a pre-Apr-28 device (auth_state with empty recovery_ack_at, valid keyset.json), mock login, assert `/generate` is NOT called by the controller after backfill.
- **Slot-2 reconciler convergence:** unit test reconciler against a fixture with N volumes at mixed `recovery_keyslot_key_id` values (mix of `""`, `"unprovisioned"`, stale fingerprint, current fingerprint) → reconciler runs → all volumes converge to current key_id with correct kill-vs-add-only treatment per sentinel; calling reconciler again is a no-op.
- **Slot-1 reconciler convergence:** parallel test for `password_keyslot_key_id`.
- **Both slots concurrently:** test where slot-1 and slot-2 blobs both exist; reconciler drains both per-pass.
- **Reconciler restart-safety across system reboot:** simulate reboot mid-reconciliation (encrypted blob present on encrypted control plane, some volumes done some not). After unlock, reconciler resumes and converges; blob deleted on full per-slot convergence.
- **B2 atomic boundary:** simulate ctx-cancel between kill and add inside `luksSetKeyslot` (e.g., reconciler nudge mid-pair); assert the slot is left intact (either old passphrase or new — never empty), and `volume.kskey_id` reflects truth.
- **Concurrent rotation:** call `/generate` twice in quick succession; assert reconciler converges all volumes to the *latest* key_id, orphan blob from the older rotation is cleaned up, status surface never lies (counts always against captured key_id).
- **Sub-case ii (steady-state new volume):** create a volume when no provisioning is in flight; assert `volume.kskey_id[1,2] == "unprovisioned"`, `unprovisioned_steady_state` count incremented, slot is empty in `luksDump`.
- **Sub-case i (in-flight new volume):** create a volume while `/generate` is in flight; assert `volume.kskey_id[2]` is set to the current key_id at creation time, slot 2 is occupied in `luksDump`.
- **Reconciler abort on lock:** while reconciler is mid-pass, fire `cryptoManager.Lock()`; assert pass aborts at next per-volume boundary, no `kskey_id` stamped after lock event, blob persists; on Unlock + nudge, reconciler resumes and converges.
- **/generate and slot-1 handlers response time under load:** benchmark request-side latency with N volumes ∈ {1, 5, 19} → all stay sub-second regardless.
- **Frontend ack-await:** widget test on `_handleRecoveryKeyThenPasskey` and `proceedAfterRecovery` paths — assert ack POST completes before `_createAuthController` / step transition fires.
- **B3 blob-write-first ordering:** simulate blob write failure (mock `fsutil.AtomicWriteFile` returning ENOSPC / fsync failure) inside the synchronous handler; assert handler returns 5xx, `keyset.json` is unchanged, no audit event fires for the prospective rotation, operator-visible state is identical to pre-call. Symmetric test for slot 1 (`handleAuthPassword`).
- **F-iter3-B1 power-loss-between-steps:** fault-injection test simulating power loss between step 2 (blob write) and step 3 (keyset.json commit); assert that on restart either both files are present (atomic happy path) or neither (early failure), never keyset=NEW with blob absent. Use a fault-injecting filesystem layer or kill -9 between commands to validate the durability primitive holds across the step boundary.
- **Step-3-fails-after-blob-write recovery:** simulate `keyset.json` atomic write failure after blob write succeeds; assert blob exists with prospective key_id, live `RecoveryKeyID()` unchanged, and reconciler at next startup deletes the orphan blob per F-A2 cleanup rule.
- **F-A1 capture-stickiness:** simulate concurrent rotation: pass-A starts, pass-A reaches volume V, between capture and reaching V a sub-case-i hook stamps V's `kskey_id[2] = NEW`. Assert pass-A re-reads live id, sees advance past captured, aborts cleanly without reverting V. Pass-B runs against the new captured key_id and converges V correctly.
- **F-A2 capture rule:** fixture with multiple `keyslot-pending-2-*.blob` files (only one matching live key_id); reconciler reads only the matching blob, others are orphans deleted at end-of-pass-success.
- **F-A4 / S6 backfill abort recovery surface:** simulate transient EIO on first read attempt then success on retry → migration succeeds without abort. Persistent EIO across retries → migration aborts with typed error, health.Tracker emits `LevelError` event, piccolod completes degraded startup, web UI renders banner.
- **S5 status surface during rotation storm:** trigger 4 rapid `/generate` calls; assert `captured_key_id != target_key_id` while reconciler walks against `captured_key_id`; `rotation_storm_pending` reflects the queue depth; eventually converges to latest after passes drain.
- **S7 sub-case ii visibility:** create N volumes in steady state (no in-flight rotation); assert `/system/boot` returns `keyslot_unprovisioned_count = N`. Then operator-initiated rotation; assert reconciler converges all N (add-only path); `keyslot_unprovisioned_count` returns to 0.
- **S8 blob format versioning:** version-byte mismatch test — write a blob with version `0x02` (unknown), assert reconciler rejects, logs WARN, treats as orphan, cleanup deletes it.

---

## Failure modes / observability

| Failure | Detection | Recovery |
|---|---|---|
| Reconciler crashes mid-pass (process exit, panic) | `keyslot_provisioning.slot{1,2}.last_error` populated; encrypted blobs present on encrypted control plane | Auto-resume on process restart (blobs decrypted under SDEK, reconciler re-enters per slot) |
| **System reboot mid-reconcile** | Encrypted blobs survive reboot (D6); on next unlock, reconciler resumes | Auto-resume; no operator action required |
| **Synchronous handler blob write fails** (degraded `/piccolo-core` storage at handler time, ENOSPC, EROFS, EIO, fsync failure) | Handler returns 5xx synchronously per D11 step ordering; the `fsutil.AtomicWriteFile` primitive surfaces fsync errors as part of the write (not silently swallowed); no state advanced; operator's paper words / current password unchanged | Operator retries after attending storage health. No silent divergence between `keyset.json` and blob — both use the same atomic-write-with-fsync primitive, so power loss between step 2 and step 3 leaves either both committed or neither, never `keyset=NEW` + blob-absent. |
| **Synchronous handler step-3 fails after blob write succeeded** (atomic keyset.json write fails / Rewrap fails) | Blob exists with prospective key_id; live `cryptoManager.RecoveryKeyID()`/`PasswordKeyslotID()` unchanged | Reconciler treats blob as orphan (live id doesn't match blob's key_id), deletes at next startup or end-of-pass-success per F-A2 cleanup rule. Operator-equivalent to "blob write failed" — no state advance. |
| Per-volume keyslot add fails (e.g., volume detached, transient cryptsetup error) | `last_error` populated; volume's `kskey_id[N]` stays at old value | Next reconciler tick retries; if volume permanently gone, operator removes app → volume metadata gone → not retried |
| **Atomic kill→add interrupted by SIGKILL during Argon2id window** (residual after D7 defense) | `volume.kskey_id[N]` stays at old value (stamped only after `add` success); slot may be empty in `luksDump` | Next reconciler tick re-attempts: pre-kill probe sees slot empty → add-only path → restored. Self-healing as long as the blob still exists. **Only the SIGKILL ∧ blob-loss intersection is unrecoverable** (bounded by rotate-to-recover policy). |
| **Encrypted blob unavailable** (control-plane LUKS corruption, manual deletion, unauthorized tamper) | Reconciler enumerates pending blobs at pass start, finds none matching live key_id; pass aborts cleanly with `last_error = "no matching blob for live key_id"` | Operator must rotate (`/generate` for slot 2, password change for slot 1) per the user-decided rotate-to-recover policy. Status surface flags the inconsistency via `pending > 0 ∧ in_progress = false`. |
| Concurrent rotation storm (operator triggers `/generate` repeatedly) | Multiple blobs written; reconciler captures the one matching live id per F-A2 capture rule; older blobs are orphans | Per F-A2 cleanup rule, orphans deleted at end-of-pass-success or process startup. Status surface shows `captured_key_id != target_key_id` and `rotation_storm_pending > 0` so the operator-comprehension gap is closed (S5 fix). Per F-A1 capture-stickiness, a rotation arriving mid-walk causes pass-B to converge cleanly without reverting volumes. |
| Backfill skips a device that should have been backfilled (case ii: file does not exist) | Operator hits regenerate-loop on next login (current pre-RFC behavior) | Same as today's manual-fix path — `/generate` is now fast, ack, done |
| Backfill aborts migration on unreadable / corrupt keyset.json | After 3× retry with backoff (per F-A4 fix), persistent failure surfaces as `health.Tracker` `LevelError` event `auth.migration_aborted_storage_health`; piccolod completes startup in degraded-admin-only state; web UI renders with a status banner | Operator attends storage health (replace SD, restore from backup, etc.) via web UI / serial console; restart piccolod after fixing. Migration retries on next start. |
| Backfill includes a device that should not have been (e.g., never-acked-during-setup edge case) | Operator does not see recovery-key step on first login post-upgrade | Operator can manually rotate via Settings → see new words → ack. Strictly less hostile than silent rotation. |
| Reconciler running while `cryptoManager` locked (idle-lock policy) | Pass aborts cleanly at next per-volume boundary (D8); `last_error = "SDEK locked mid-pass; will resume on unlock"` | Resumes on next unlock + nudge; no operator action |
| **Sub-case ii steady-state accumulation** (new volumes created during steady state, slot is empty until next rotation) | Surfaced on `/api/v1/system/boot` as `keyslot_unprovisioned_count` (per S7 fix); UI renders a hint on home/login surface when non-zero | Operator-initiated rotation per rotate-to-recover policy; convergence picks up `"unprovisioned"` volumes via add-only path |
| **Pre-RFC blob format encountered during version-bump upgrade** (S8 hazard) | Version byte read at blob open; reconciler rejects unknown versions, logs WARN, treats as orphan | Operator-initiated rotation; new blob written in current version |

Reconciler progress on `/api/v1/crypto/status` (`keyslot_provisioning.slot{1,2}`) is the primary observability surface. Audit events:

- `auth.keyslot_reconcile_started` (slot, key_id, volume_count)
- `auth.keyslot_reconcile_completed` (slot, key_id, duration_ms, volume_count_done, volume_count_skipped_unprovisioned)
- `auth.keyslot_reconcile_failed` (slot, key_id, volume, err) — per-volume failures, not the whole pass
- `auth.keyslot_reconcile_aborted_locked` (slot, captured_key_id) — pass aborted because SDEK was locked mid-pass

---

## Out of scope / deferred

- **`/refresh-keyslots` endpoint** (user decision: rotate-to-recover accepted). When a rare gap forms (e.g., simultaneous SDEK loss AND blob corruption, or backup-restored-from-snapshot mid-reconcile), the recovery path is operator-initiated rotation, accepting paper-words invalidation. Captured here in case operational data later shows the gap is more frequent than expected.
- **Backup/restore semantics for the encrypted control plane** (Adj1 from iter-2 red-team review). Restoring a control-plane snapshot that contains a blob from a different generation than the live `keyset.json` / `password_hash` produces a state where the reconciler's capture rule rejects the blob as orphan (correct behavior: it doesn't match live id). Restoring auth_state with a different password_hash than what's actually in slot 1 of volumes is a broader backup/restore consistency issue not specific to this RFC. Defer to a future control-plane backup/restore consistency RFC.
- **Frontend "preparing recovery key…" intermediate state** — UX polish to bridge the (now sub-second, but non-zero) gap between click and words rendering. Not load-bearing now that the gap is sub-second.
- **Audit pass on other column-add sites** for similar missing-backfill bugs. Captured as deferred process-smell follow-up.
- **Reconciler back-pressure / scheduling / parallelization**: the current shape is single-goroutine sequential. A future optimization could parallelize across volumes (each LUKS device is independent). Out of scope; sequential is fine because the UI no longer waits.
- **Migration of v2 volume metadata** (if any v2 volumes still exist on legacy devices): the per-volume `kskey_id` fields need to land on the metadata struct that v2 volumes use. Defer until a v2 device is encountered in the field.
- **v2 control-plane LUKS volume keyslot management** (F7 resolution: existing iterator already skips non-v3 metadata; reconciler inherits that scope). Offline-recovery story for the v2 control plane is via SDEKRK + control-plane master-key wrap (online path); if the control plane is unrecoverable, neither slot 2 nor SDEKRK helps. Deferred-investigation if a v2 control-plane device requires offline recovery in practice.

---

## Risks

- **Operator ack vs reconciler completion window:** operator ack's words → status shows reconciler still running. If they immediately unplug for a disaster-recovery drill with the just-ack'd words, the *new* words don't yet work via `cryptsetup luksOpen` directly on every volume. They still work via `/reset-password` (online path) if the device boots normally. Mitigation: status surface, plus the offline-recovery scenario is rare enough that this window is acceptable.
- **Backfill false positive** (treating a device-that-never-ack'd as ack'd): operator's "pre-existing ack'd words" don't actually exist; they have no recovery options if password is forgotten. Mitigation: this is the *current* state of those devices today (they have a recovery key in `keyset.json` but no UI told them to save it pre-Apr-28), so backfill doesn't make their position worse — it just stops the silent rotation hostile path.
- **Encrypted-blob exposure on control-plane backup**: blobs live on the encrypted control plane during reconciliation; an attacker who gains access to a control-plane snapshot during a reconcile window AND the SDEK could decrypt the blob. Mitigation: SDEK-AEAD wrap (D6); blob lifetime bounded to the reconciliation window (sub-minutes typical, never more than a single reboot cycle); the SDEK itself is the more valuable target and protects it under the operator's password.
- **Reconciler livelock under repeated lock-unlock churn**: each lock-unlock cycle triggers a pass restart. Mitigation: passes are idempotent and bounded (per-volume work is bounded by volume count); the work that *did* complete pre-lock is preserved (D8). Worst case is no progress until the device stays unlocked long enough; UI status surface flags this.
- **SIGKILL ∧ blob-loss intersection** (residual after D7 defense): SIGKILL alone during the Argon2id window leaves the slot empty, but the next reconciler tick auto-recovers via the pre-kill probe + add-only path *as long as the blob still exists*. Only the simultaneous SIGKILL-during-Argon2id AND blob-unavailability case is unrecoverable; rotate-to-recover policy is the bound.
- **Concurrent slot-1 + slot-2 rotations**: two blobs present, reconciler walks both per-pass. Mitigation: per-pass walks are independent per slot (reconciler reads `(slot, key_id, passphrase)` once per slot per pass); volumes have independent `kskey_id[1]` and `kskey_id[2]` so progress on one slot does not invalidate the other.
