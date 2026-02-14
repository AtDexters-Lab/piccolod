# RCA: gocryptfs password mismatch after reboot — data volume corruption

- **Status:** Open
- **Date:** 2026-02-12
- **Severity:** Critical (data loss — encrypted volume permanently inaccessible)
- **Related:**
  - `docs/rfc/20260201-storage-posture.md` (LUKS lifecycle)
  - `docs/rfc/20260202-storage-v2-foundation.md` (data root contract)

## 1. Summary

After a reboot cycle on a dev machine running Piccolo OS (Tumbleweed), a previously installed Debian workspace app (`d1`) failed to mount with repeated `Password incorrect` / `cipher: message authentication failed` errors. The root cause is a **race condition** in the crypto unlock handler: the persistence layer is notified of unlock *before* the LUKS data volume is mounted. The reconcile loop starts before `/piccolo-data` is available, causing a spurious `gocryptfs -init` on an already-initialized cipher directory. The init overwrote `gocryptfs.conf` before failing on the existing DirIV (exit code 7), creating a permanent key mismatch.

## 2. Timeline

All timestamps from `artifacts/logs/luks.log` (second boot cycle).

| Time (approx.) | Event |
|---|---|
| Boot start | `internal boot detected; starting disk preparation` |
| +14s | `storage phase 1 complete` |
| +14s | `POST /api/v1/crypto/unlock` — user enters admin password |
| +14s | `control lock state=unlocked` — `notifyPersistenceLockState(false)` fires |
| +14s | `app-manager observed control lock state=unlocked` — reconcile loop starts |
| +14s | `LUKS unlocked via pool keyfile (keyslot 0)` — data volume unlock begins |
| +14s | `data volume unlocked and mounted` — `/piccolo-data` now available |
| +15s | `WriteDirIV: Openat: file exists` (PID 1393) — `gocryptfs -init` finds existing DirIV |
| +15s | `WARN: reconcile app d1: init gocryptfs for /piccolo-data/ciphertext/app-d1: exit status 7` |
| +16s | `failed to unlock master key: cipher: message authentication failed` |
| +16s | `Password incorrect.` (PID 1435) |
| +16s–end | Repeated `Password incorrect` on every retry |

Filesystem evidence from the VM:

| File | Timestamp | Interpretation |
|---|---|---|
| `piccolo.volume.json` | 19:18 (first boot) | Metadata wraps passphrase₁ — never re-written |
| `gocryptfs.conf` | **20:10** (second boot) | Overwritten by rogue `gocryptfs -init` with passphrase₂ |
| `gocryptfs.diriv` | 19:18 (first boot) | Original DirIV preserved (init failed on this step) |

## 3. Root Cause

### 3.1 Race condition in `handleCryptoUnlock`

In `internal/server/gin_crypto_handlers.go`, `handleCryptoUnlock` (line 151) executes:

```
1. cryptoManager.Unlock(password)          // line 168 — unwrap SDEK in memory
2. notifyPersistenceLockState(ctx, false)   // line 172 — triggers app-manager reconcile
3. storageMgr.UnlockDataVolume(password)    // line 180 — LUKS unlock + mount /piccolo-data
```

Step 2 fires *before* step 3. The notification calls `setLockState` which publishes a lock-state event on the event bus. The app-manager's subscription handler launches `go m.ReconcileOnce(loopCtx)` (app_manager.go:410) — a goroutine that runs **concurrently** with the remainder of the handler. This is a **structural race**, not timing-dependent: even if LUKS unlock were instantaneous, the reconcile goroutine may have already called `ensureAppVolumeLayout` before `UnlockDataVolume` returns.

The full call chain from notification to corruption:
`notifyPersistenceLockState` → `setLockState` → event bus publish → app-manager `ReconcileOnce` (goroutine) → `reconcileApp` → `ensureAppVolumeLayout` → `EnsureVolume` → `ensureMetadata` → `gocryptfs -init`

### 3.1.1 All affected `notifyPersistenceLockState(false)` call sites

| File | Line | Handler | Affected? | Notes |
|---|---|---|---|---|
| `gin_crypto_handlers.go` | 172 | `handleCryptoUnlock` | **Yes** | Observed trigger of this incident |
| `gin_crypto_handlers.go` | 69 | `handleCryptoSetup` | **Yes** | Same ordering issue; less likely since no pre-existing volumes on first boot |
| `gin_auth_handlers.go` | 251 | `handleAuthLogin` | **Yes (worse)** | Calls `notifyPersistenceLockState(false)` but **never** calls `UnlockDataVolume` at all |
| `gin_crypto_handlers.go` | 273 | `handleCryptoResetPassword` | **Low risk** | Transient unlock with deferred relock at line 327; reconcile window is small but non-zero |
| `gin_crypto_handlers.go` | 281, 329 | `handleCryptoResetPassword` (relock) | No | These are lock=true notifications |
| `gin_crypto_handlers.go` | 367 | `handleCryptoLock` | No | Lock notification, not unlock |

The `handleAuthLogin` path is particularly dangerous: if a user's browser sends a login request (e.g., session restoration) before an explicit `/crypto/unlock` call after reboot, the reconcile fires with no LUKS unlock ever happening in that handler.

### 3.2 Rogue `gocryptfs -init` on an already-initialized cipher directory

When `ensureMetadata` (`internal/persistence/file_volume_manager.go:825`) finds no metadata file, it:

1. Generates a new random passphrase₂
2. Runs `gocryptfs -init` on the cipher directory
3. Writes `piccolo.volume.json` with passphrase₂ wrapped by SDEK (only if step 2 succeeds)

Due to the race, `ensureMetadata` reads metadata from the unmounted `/piccolo-data` (not found), then by the time `gocryptfs -init` executes the LUKS mount has completed and the cipher directory contains the original files. `gocryptfs -init` wrote `gocryptfs.conf` (new master key from passphrase₂) then failed when writing `gocryptfs.diriv` (`WriteDirIV: Openat: file exists`, exit code 7). This overwrites the config but not the DirIV, creating a permanent key mismatch.

The filesystem timestamps confirm this: `gocryptfs.conf` was overwritten (timestamp 20:10, second boot) while `piccolo.volume.json` was preserved (timestamp 19:18, first boot). Since `gocryptfs -init` failed, step 3 above never executed — the metadata still wraps passphrase₁ while the config now expects passphrase₂.

> **Peer review note:** gocryptfs 2.4.0 adds a non-empty directory check that exits with rc=6 before writing anything, which would prevent the config overwrite. However, the race condition itself (spurious init attempt on an already-initialized volume) is the bug regardless of whether a downstream tool happens to mask the data-loss symptom.

### 3.3 Race sequence

```
handleCryptoUnlock
  │
  ├── notifyPersistenceLockState(false)          [fires FIRST]
  │     └── app-manager reconcile starts (goroutine)
  │           └── ensureAppVolumeLayout("d1")
  │                 └── EnsureVolume
  │                       ├── ensureCipherDir: MkdirAll on unmounted /piccolo-data (creates empty dir)
  │                       └── ensureMetadata()
  │                             ├── os.ReadFile(stateDir/piccolo.volume.json) → ErrNotExist
  │                             ├── metadata not found → treats as new volume
  │                             ├── generates passphrase₂, seals with SDEK
  │                             └── gocryptfs -init on cipherDir
  │                                   │  (by now LUKS has mounted — dir contains original files)
  │                                   ├── writes gocryptfs.conf (passphrase₂) ← OVERWRITES
  │                                   └── fails on existing DirIV (exit 7)
  │
  └── UnlockDataVolume(password)                 [fires SECOND / concurrently]
        └── /piccolo-data mounted mid-race
```

**Result:** `gocryptfs.conf` is overwritten with passphrase₂, but `piccolo.volume.json` still wraps passphrase₁ (step 3 of `ensureMetadata` never executed because init failed). Every subsequent mount attempt unwraps passphrase₁ from the metadata and presents it to gocryptfs, which rejects it because the config now expects passphrase₂. The volume data is effectively bricked.

## 4. Impact

- **Blast radius:** Any app volume previously initialized on `/piccolo-data` is corrupted if the reconcile loop races ahead of LUKS unlock on reboot.
- **Data loss:** The encrypted data on the volume is inaccessible — the original `gocryptfs.conf` (keyed to passphrase₁) is overwritten and not recoverable without a backup.
- **User experience:** App appears stuck with cryptic "Password incorrect" errors. No self-service recovery path exists today.
- **Scope:** Affects `handleCryptoUnlock` (reboot path), `handleCryptoSetup` (first-boot path, lower risk), and `handleAuthLogin` (login-while-locked path — never calls `UnlockDataVolume` at all). `handleCryptoResetPassword` has a small transient window but relocks immediately.

## 5. Contributing Factors

1. **No ordering contract between persistence notification and data volume availability.** The code implicitly assumes `/piccolo-data` is mounted when the reconcile loop starts, but the notification fires before the mount. This is the root cause.
2. **No guard in `ensureMetadata` against existing gocryptfs state.** The function checks for `piccolo.volume.json` but does not check whether `gocryptfs.conf` already exists in the cipher directory before running `gocryptfs -init`.
3. **`gocryptfs -init` is not atomic.** It writes the config file before the DirIV, so a failure on the second write leaves the config in a corrupted-from-our-perspective state.

## 6. Remediation

### 6.1 Primary fix: reorder unlock handlers

Move `notifyPersistenceLockState` **after** the data volume mount attempt in all affected handlers. Step 3 must execute regardless of step 2's outcome (it gates gocryptfs-on-core volumes that don't depend on `/piccolo-data`), but must be sequenced **after** step 2 completes.

**`handleCryptoUnlock` — target order:**
```
1. cryptoManager.Unlock(password)
2. storageMgr.UnlockDataVolume(password)       // attempt mount first (capture error, don't abort)
3. notifyPersistenceLockState(ctx, false)       // then notify — fires regardless of step 2 outcome
```

**`handleCryptoSetup` — target order:**
```
1. cryptoManager.Setup(password) + Unlock
2. authManager.Setup + admin user creation
3. storageMgr.InitializeDataVolume(password)    // attempt mount first (capture error, don't abort)
4. notifyPersistenceLockState(ctx, false)        // then notify — fires regardless of step 3 outcome
```

**`handleAuthLogin` — add missing LUKS unlock:**
```
1. cryptoManager.Unlock(password)
2. storageMgr.UnlockDataVolume(password)       // currently missing entirely — must be added
3. notifyPersistenceLockState(ctx, false)       // then notify
```

Error handling note: the existing pattern captures `luksErr` but continues execution (lines 178-187 in `handleCryptoUnlock`). This behavior must be preserved — a LUKS failure should not prevent the persistence notification from firing, since core-volume gocryptfs mounts are independent of LUKS.

Both `handleCryptoSetup` (line 140) and `handleCryptoUnlock` (line 222) return `500 Internal Server Error` when LUKS fails, after creating a portal session so the user retains UI access for recovery.

### 6.2 Defense-in-depth: guard in `ensureMetadata`

Before running `gocryptfs -init`, check whether any gocryptfs artifacts (`gocryptfs.conf` or `gocryptfs.diriv`) already exist in the cipher directory. If either exists, the volume was previously initialized and re-initialization would corrupt it. Return a descriptive error instead:

```go
for _, artifact := range []string{"gocryptfs.conf", "gocryptfs.diriv"} {
    p := filepath.Join(entry.cipherDir, artifact)
    if _, err := os.Stat(p); err == nil {
        return fmt.Errorf("%s exists in %s but metadata is missing — refusing re-init to prevent data loss", artifact, entry.cipherDir)
    }
}
```

Checking both artifacts covers the case where `gocryptfs -init` wrote the DirIV before the conf (or vice versa in future gocryptfs versions). This guard prevents **re-initialization** of an existing volume even if the race occurs. It does not prevent two concurrent first-time initializations from racing (that scenario requires the primary fix in 6.1). This is defense-in-depth, not a standalone fix.

### 6.3 Future consideration: backup gocryptfs.conf

Store a copy of `gocryptfs.conf` alongside the volume metadata in the state directory. This would allow recovery if the cipher-directory copy is corrupted, but is a larger change with its own consistency concerns. Deferred.

## 7. Detection & Monitoring

- The current log output (`WARN: reconcile app d1: init gocryptfs ...`) does surface the symptom, but the root cause (race) is not obvious from logs alone.
- Consider emitting a structured event (`TopicVolumeCorruption`) when `ensureMetadata` detects an orphaned `gocryptfs.conf` without matching metadata.

## Remediation Status

- [x] Primary fix: reorder `handleCryptoUnlock` and `handleCryptoSetup` — LUKS before notify
- [x] Fix `handleAuthLogin`: add missing `UnlockDataVolume` call before notify
- [x] Defense-in-depth: gocryptfs artifact existence check in `ensureMetadata`
- [ ] Assess `handleCryptoResetPassword` transient unlock window — deferred (low risk, guarded by defense-in-depth)
- [ ] Test coverage: add test for race scenario
