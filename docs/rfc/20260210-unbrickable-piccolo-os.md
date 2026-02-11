# RFC: Make Piccolo OS Unbrickable

**Date:** 2026-02-10
**Status:** Draft
**Amends:** RFC 20251124-microos-transactional-update (Section 3 — this RFC masks the `transactional-update.timer` that the prior RFC relied on; piccolod's `systemd-run` invocation replaces the timer as the update trigger)

## 1. Problem Statement

A freshly imaged RPi 400 running Piccolo OS v0.1.19 suffered a silent self-destructive transactional update within minutes of first boot. The failure chain:

1. RPi has no battery-backed RTC — system booted with image-build-time clock (15 days stale)
2. chronyd stepped the clock forward 15 days after NTP sync (~70s post-boot)
3. `transactional-update.timer` (`Persistent=true`, `OnCalendar=daily`) evaluated 15 missed daily runs and fired immediately
4. `btrfs-scrub.timer`, `btrfs-balance.timer`, `btrfs-trim.timer`, and `btrfs-defrag.timer` also fired, creating an I/O storm on the SD card
5. The zypper solver cache for `repo-oss` was corrupted during compilation (97 bytes instead of ~50 MB) — likely due to I/O contention or a dual-process race (see FINDINGS.md for analysis)
6. `zypper dup` interpreted all 118 repo-oss packages as orphans and removed them — including `kernel-default`, `cryptsetup`, `iproute2`, `sudo`, `grub2`, `u-boot-rpiarm64`
7. Snapshot 3 (gutted) was set as the boot default
8. Only `REBOOT_METHOD=none` (configured by `piccolo-os-support`) prevented an immediate reboot into an unbootable snapshot

Full investigation: `artifacts/logs/rpi-first-boot-investigation/FINDINGS.md`

### The rollback defense is a paper tiger

Every supposed layer of protection failed or is non-functional:

| Defense layer | Status | Why it failed |
|---|---|---|
| health-checker plugin | **Broken** | Checks `/health/live` which always returns HTTP 200 regardless of system state |
| `/health/ready` endpoint | **Broken** | Always returns HTTP 200 — has a TODO comment acknowledging this (`gin_server.go:1879-1882`) |
| health-checker → snapper rollback | **Only works post-boot** | Cannot protect against missing kernel/bootloader — device never reaches userspace |
| GRUB `health_checker_flag` | **Broken on all platforms** | grubenv lives on read-only btrfs root on both x86 and RPi (`grub2-editenv list` returns empty on both). GRUB's btrfs driver cannot write (`save_env` is a no-op) and cannot read across subvolume boundaries (confirmed via GRUB shell testing). The mechanism is universally non-functional, not just an RPi/first-boot issue. |
| u-boot boot counting | **Not configured** | No `CONFIG_BOOTCOUNT_LIMIT`, no `altbootcmd`, no fallback snapshot |
| Pre-commit snapshot validation | **Doesn't exist** | Nothing checks whether a staged snapshot contains critical binaries before setting it as boot default |
| Timer hygiene | **Absent** | MicroOS server-oriented defaults (`transactional-update.timer`, `btrfs-scrub.timer`, etc.) left enabled on headless SD card devices |

Piccolo OS is a headless appliance with no SSH, no serial console, no physical recovery path. A bricked device is a paperweight. This threat model demands that brickability be treated as a first-class design concern.

## 2. Design Principle

**No single failure can brick the device.**

Every state transition that touches the boot chain must have a safety net. The defenses must be layered and independent — the failure of any one layer must not compromise the overall guarantee.

## 3. Goals

1. Eliminate the uncontrolled timer storm that triggered the investigated incident
2. Make the health-checker rollback mechanism actually functional (detect real failures, trigger real rollbacks)
3. Add pre-reboot validation that prevents gutted snapshots from becoming the boot default
4. Establish a defense-in-depth architecture where each layer catches a distinct failure class
5. Define a roadmap for bootloader-level protection (Phase 2)

## 4. Non-Goals

- Image-based A/B updates (replacing `transactional-update` entirely) — too expensive for pre-beta
- Custom u-boot builds for RPi (Phase 2 — documented here for design continuity, not implemented now)
- systemd-boot on RPi (u-boot's UEFI emulation is insufficient — see 9.1 for future path)
- Fixing the zypper solver corruption itself — that's an upstream zypper/libsolv issue; we defend against its consequences

## 5. Threat Model

| # | Brick vector | Likelihood | Current defense | Gap |
|---|---|---|---|---|
| T1 | Corrupted/gutted snapshot set as boot default | **Confirmed** (the incident) | None | No pre-commit validation |
| T2 | Missing kernel/initrd in boot snapshot | High (consequence of T1) | GRUB `health_checker_flag` + health-checker | **GRUB mechanism broken on all platforms** — grubenv is on read-only btrfs root on both x86 and aarch64 (confirmed empty via `grub2-editenv list` on VirtualBox MicroOS and RPi). GRUB's btrfs driver cannot write (`save_env` is a no-op), and cannot read across btrfs subvolume boundaries (confirmed by GRUB shell testing). health-checker runs post-boot; missing kernel = no boot = no health-checker |
| T3 | Uncontrolled timer storm on first boot | **Confirmed** (the incident) | None | All MicroOS default timers left enabled |
| T4 | In-snapshot software failure after successful boot | Medium | health-checker | Endpoint always returns 200; rollback never triggers |
| T5 | Kernel panic / boot hang | Medium | None | No boot counting configured |
| T6 | OOM / system hang | Medium | None | No hardware watchdog |
| T7 | Boot partition corruption (FAT ESP) | Low | None | No redundant bootloader env |
| T8 | SD card hardware failure | Low | None | No software defense possible |
| T9 | LUKS key loss / TPM seal failure | Low | Persistence module recovery flow | Existing — adequate for now |
| T10 | Power loss during btrfs write | Low | btrfs COW semantics | Adequate |

## 6. Defense Layer Architecture

### 6.1 Coverage matrix

| Defense | Phase | Catches | Does NOT catch |
|---|---|---|---|
| **Mask dangerous timers** | 1 | T1, T3 (eliminates trigger) | Post-trigger failures |
| **Health endpoint 503** | 1 | T4 (in-snapshot software failures, currently limited to LevelError paths — see 7.2a) | T2 (no boot = no endpoint) |
| **Pre-reboot snapshot validation** | 1 | T1, partial T2 (blocks API-triggered reboot if kernel or critical userspace binaries missing; does NOT check initrd or bootloader on ESP) | Uncontrolled reboots (power cycle, panic, manual `systemctl reboot`) |
| **health-checker explicit enable** | 1 | Belt-and-suspenders for rollback infra | — |
| **systemd-boot + bless-boot (x86/aarch64 PCs)** | 2 | T2, T5 (automatic boot assessment via BLS tries counter on ESP; works on first boot) | RPi (u-boot EBBR lacks EFI variable support needed by systemd-boot) |
| **U-Boot bootcount (RPi)** | 2 | T2, T5 (bootloader-level fallback including first boot) | T6 (hang without reboot); x86/aarch64 PCs (no u-boot) |
| **Pre-reboot static analysis** | 2 | T1, T2 (validates systemd unit graph in staged snapshot) | Runtime failures, service-level bugs |
| **Hardware watchdog** | 2 | T5, T6 (reboot on hang) | — |
| **systemd watchdog** | 2 | T6 (detect piccolod hang) | Kernel-level hangs |

**Honest assessment of Phase 1 coverage:** Phase 1 eliminates the specific trigger of the investigated incident (timer masking prevents the uncontrolled update storm) and adds two independent safety nets: file-existence validation for API-triggered reboots, and a functional health-checker rollback chain for post-boot failures. It materially reduces the likelihood and severity of bricking but does not make it impossible — file-existence checks cover a small set of critical binaries (not initrd, not ESP bootloader), and uncontrolled reboots bypass the API validation gate entirely. Full coverage requires Phase 2, which diverges by platform: systemd-boot with automatic boot assessment for x86/aarch64 PCs, u-boot bootcount for RPi.

### 6.2 Layered defense flow

```
[Phase 1: Prevent]
Timer masking ──→ Uncontrolled updates never start

[Phase 1: Validate before API-triggered reboot]
piccolod Reboot() ──→ validateStagedSnapshot() ──→ Block if critical binaries missing
                                                ──→ Revert default to active snapshot

[Phase 1: Detect after boot]
health-checker.service ──→ curl /health/ready ──→ 503 on LevelError ──→ snapper rollback

[Phase 2: Boot-level fallback — x86/aarch64 PCs]
Switch to systemd-boot ──→ BLS entries with tries counter on ESP (FAT32, writable)
Boot attempt ──→ systemd-boot decrements tries ──→ tries exhausted ──→ fallback to previous entry
Healthy boot ──→ systemd-bless-boot.service marks entry as good (removes counter)

[Phase 2: Boot-level fallback — RPi]
u-boot ──→ bootcount > bootlimit ──→ altbootcmd ──→ boot fallback snapshot

[Phase 2: Enhanced pre-reboot validation (all platforms)]
systemd-analyze verify ──→ unit graph validation
systemd-nspawn --boot  ──→ container boot validation

[Phase 2: Hang recovery]
systemd WatchdogSec + hardware watchdog ──→ hard reboot ──→ boot assessment catches it
```

## 7. Phase 1: Immediate Changes (Pre-Beta)

### 7.1 Mask dangerous timers

**Files:**
- `piccolo-os/packages/piccolo-os-support/piccolo-os-support.spec` — `%post` section (after line 106)
- `piccolo-os/kiwi/microos-ots/config.sh` — belt-and-suspenders (in case `%post` runs out of order)

**Change in spec `%post`:**

```bash
# Mask timers/paths that are dangerous on headless/SD-card devices.
# transactional-update.timer: Persistent=true causes immediate firing after NTP clock
# correction on devices with no battery-backed RTC. piccolod orchestrates updates
# via systemd-run (internal/update/manager.go:206), bypassing the timer entirely.
# btrfs maintenance: I/O contention on SD cards causes silent data corruption.
# Note: btrfsmaintenance-refresh is a .path unit (not a timer) on MicroOS.
/usr/bin/systemctl --root=/ --no-reload mask \
    transactional-update.timer \
    btrfs-scrub.timer \
    btrfs-balance.timer \
    btrfs-trim.timer \
    btrfs-defrag.timer \
    btrfsmaintenance-refresh.path
```

**Change in `config.sh`:**

Same `systemctl mask` commands added to the kiwi post-build script, ensuring timers are masked even if `piccolo-os-support` package installation fails or runs out of order during image build.

**Safety:** piccolod's update manager already invokes `transactional-update` directly via `systemd-run --unit piccolo-tu-<action>-<ts>` (`internal/update/manager.go:535-551`). The timer is not in the update path. btrfs maintenance is inappropriate for SD cards (the investigation showed scrub, balance, trim, and defrag all running simultaneously during the failure window). The `btrfsmaintenance-refresh.path` (not a `.timer` — confirmed on the RPi image) is the orchestrator that can re-enable individual btrfs maintenance services; masking it prevents re-activation.

**Idempotency:** `systemctl mask` is idempotent — masking an already-masked unit is a no-op (exit 0). Masking a non-existent unit also succeeds (creates a dangling symlink). The scriptlet is safe to run repeatedly and tolerant of unit name changes across MicroOS versions.

Note: snapper timers (`snapper-cleanup.timer`, `snapper-boot.timer`, `snapper-timeline.timer`) are NOT masked. snapper is needed for the health-checker rollback mechanism and snapshot management. `snapper-timeline.timer` is already effectively disabled via `TIMELINE_CREATE="no"` in the snapper config (`config.sh`).

### 7.2 Fix health endpoint to surface fatal states

**Failure class covered:** In-snapshot software failures after a successful boot (T4). Examples: storage corruption, LUKS unlock failure, container runtime broken. Does NOT cover the gutted-snapshot case (T1/T2) — that device never boots to userspace.

#### 7.2a Health tracker: no structural changes needed (known limitation)

The existing three-level model is sufficient for the 503 gate:

| Level | Meaning | During boot | On failure |
|---|---|---|---|
| `LevelOK` | Healthy | After unlock | — |
| `LevelWarn` | Expected transitional state | Pre-unlock, initializing | — |
| `LevelError` | Fatal failure | — | Storage corruption, unlock failure |

`Overall()` (`internal/health/tracker.go:81-91`) returns the worst level across all components. We gate 503 on `LevelError` only — `LevelWarn` during boot is normal and must not trigger rollback.

**Known limitation:** `LevelError` is currently set in only three code paths:
- `gin_crypto_handlers.go:109` — data volume initialization failed
- `gin_crypto_handlers.go:176` — data volume unlock failed
- `gin_emergency_handlers.go:148` — emergency event

Many degraded states remain at `LevelWarn` (persistence locked, storage awaiting prep, update initializing). This means the 503 rollback trigger has narrow coverage today. Expanding `LevelError` usage to additional fatal conditions (e.g., container runtime unrecoverable failure, persistent networking loss) is a follow-up task, not a Phase 1 blocker. The change from always-200 to 503-on-Error is still a strict improvement — it goes from detecting nothing to detecting the three most critical failure modes.

#### 7.2b Update `/health/ready` handler

**File:** `internal/server/gin_server.go` (lines 1867-1884)

Current code always returns HTTP 200 with a TODO comment acknowledging the gap. Replace with:

```go
func (s *GinServer) handleGinReadinessCheck(c *gin.Context) {
	if s.healthTracker == nil {
		c.JSON(http.StatusOK, gin.H{"ready": true, "status": "unknown"})
		return
	}
	required := []string{"persistence", "app-manager", "service-manager"}
	ready, snapshot := s.healthTracker.Ready(required...)
	overall := s.healthTracker.Overall()
	payload := gin.H{
		"ready":      ready,
		"status":     overall.String(),
		"components": flattenHealth(snapshot),
	}
	// Surface fatal states (LevelError) as 503 so MicroOS health-checker
	// can trigger automatic rollback. LevelWarn is normal during boot
	// (pre-unlock, initializing) and must not trigger rollback.
	if overall == health.LevelError {
		c.JSON(http.StatusServiceUnavailable, payload)
		return
	}
	c.JSON(http.StatusOK, payload)
}
```

This resolves the TODO at lines 1879-1882.

#### 7.2c Update health-checker script

**File:** `piccolo-os/packages/piccolo-os-support/piccolo-health-check.sh` (line 18)

Change from `/health/live` to `/health/ready`:

```bash
if /usr/bin/curl --silent --fail --max-time 2 http://127.0.0.1:80/api/v1/health/ready >/dev/null; then
```

`curl --fail` exits non-zero for HTTP 4xx/5xx. So:
- 200 (OK or Warn) → curl succeeds → health check passes
- 503 (Error) → curl fails → health check fails → health-checker triggers rollback
- Connection refused (piccolod not running) → curl fails → health check fails → rollback

**Backward compatibility:** Older piccolod returns 200 from `/health/ready`, so the updated script works with both old and new daemon versions.

### 7.3 Pre-reboot snapshot validation

**Failure class covered:** Gutted or corrupted staged snapshots (T1, T2) — the exact incident we investigated.

**File:** `internal/update/manager.go`

#### 7.3a `validateStagedSnapshot()` method

```go
// validateStagedSnapshot checks that the staged snapshot (if any) contains critical
// system components. Returns nil if no staged snapshot exists or if all checks pass.
// Returns a descriptive error listing missing components if validation fails.
//
// Fail-closed: if we cannot determine the default or active snapshot, we return
// an error rather than silently skipping validation.
func (m *microOSBackend) validateStagedSnapshot(ctx context.Context) error {
	activeID, _ := m.activeSnapshot(ctx)
	defaultRaw := m.defaultSnapshot(ctx)
	if defaultRaw == "" {
		return fmt.Errorf("cannot determine default snapshot; refusing to reboot without validation")
	}
	// Note: snapperNumberFromID() falls back to returning the raw btrfs ID
	// if the mapping fails (manager.go:669,679). In that case, defaultID will
	// be e.g. "269" rather than the snapper number "2", and the constructed
	// path /.snapshots/269/snapshot won't exist. The os.Stat checks below
	// will all fail, blocking the reboot. This is fail-closed by design.
	defaultID := m.snapperNumberFromID(ctx, defaultRaw)
	if defaultID == activeID {
		return nil // rebooting into current snapshot, no validation needed
	}
	snapshotRoot := filepath.Join("/.snapshots", defaultID, "snapshot")

	// Critical binaries — device is unbootable/unmanageable without these.
	// Paths validated against Piccolo OS v0.1.19 RPi snapshot:
	//   usr/lib/systemd/systemd        ← confirmed in snap1
	//   usr/sbin/cryptsetup            ← confirmed in snap1
	//   usr/sbin/ip                    ← confirmed in snap1 (iproute2)
	//   usr/lib/modules/*/Image        ← ARM64 kernel (confirmed in snap1)
	//   usr/lib/modules/*/vmlinuz      ← x86 kernel (expected)
	// Bootloader components were also removed in the incident (grub2,
	// u-boot-rpiarm64). However, bootloader files live on the EFI System
	// Partition, not in the btrfs snapshot root, so they survive snapshot
	// changes. We validate the snapshot root only.
	criticalPaths := []string{
		"usr/lib/systemd/systemd",
		"usr/sbin/cryptsetup",
		"usr/sbin/ip",
	}
	// Kernel image — explicit patterns for x86 (vmlinuz) and ARM64 (Image).
	// Note: initrd lives on the EFI System Partition (/boot/efi), not in
	// the btrfs snapshot root, so it survives snapshot changes and is not
	// checked here.
	criticalGlobs := []string{
		"usr/lib/modules/*/vmlinuz",
		"usr/lib/modules/*/Image",
	}

	var missing []string
	for _, rel := range criticalPaths {
		full := filepath.Join(snapshotRoot, rel)
		if _, err := os.Stat(full); err != nil {
			missing = append(missing, rel)
		}
	}
	// At least one kernel glob must match (ARM64 has Image, x86 has vmlinuz).
	kernelFound := false
	for _, pattern := range criticalGlobs {
		matches, _ := filepath.Glob(filepath.Join(snapshotRoot, pattern))
		if len(matches) > 0 {
			kernelFound = true
			break
		}
	}
	if !kernelFound {
		missing = append(missing, "usr/lib/modules/*/vmlinuz or */Image (kernel)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("staged snapshot %s is missing critical components: %v", defaultID, missing)
	}
	return nil
}
```

The snapshot path `/.snapshots/<N>/snapshot` is already used by `readStatus` (`manager.go:435`) for querying staged RPM versions — this pattern is proven.

Kernel path validated empirically against a real Piccolo OS v0.1.19 RPi snapshot:
- ARM64 kernel at `usr/lib/modules/6.18.8-1-default/Image` — matched by `*/Image` glob
- x86 images use `usr/lib/modules/*/vmlinuz` — matched by `*/vmlinuz` glob
- Using separate explicit patterns avoids false positives from other files starting with `v` or `I`
- Gutted snapshot 3 has no `/usr/lib/modules/` contents at all — both globs would correctly fail

Note: The `internal/update` package does not import any logging library — all error reporting uses `fmt.Errorf` returned to callers. The caller (`Reboot`) will surface the error through the API response. Phase 2 adds `systemd-analyze verify` for unit graph validation on top of these file-existence checks.

#### 7.3b `revertDefaultSnapshot()` helper

When validation fails, revert the boot default to the current active snapshot:

```go
// revertDefaultSnapshot sets the current active snapshot as the boot default,
// undoing a bad snapshot's promotion. Uses snapper modify --default rather than
// transactional-update rollback (which without arguments creates a new read-write
// snapshot copy — not what we want).
func (m *microOSBackend) revertDefaultSnapshot(ctx context.Context) error {
	activeID, _ := m.activeSnapshot(ctx)
	if activeID == "" {
		return fmt.Errorf("cannot determine active snapshot for revert")
	}
	_, _, _, err := m.runner.Run(ctx, "snapper", "modify", "--default", activeID)
	return err
}
```

#### 7.3c Hook into `Reboot()`

**Current code** (`manager.go:246-254`):

```go
func (m *microOSBackend) Reboot(ctx context.Context) error {
	if !m.supported {
		return ErrUnsupported
	}
	_, _, _, err := m.runner.Run(ctx, "systemctl", "reboot")
	return err
}
```

**Updated:**

```go
func (m *microOSBackend) Reboot(ctx context.Context) error {
	if !m.supported {
		return ErrUnsupported
	}
	if err := m.validateStagedSnapshot(ctx); err != nil {
		defaultRaw := m.defaultSnapshot(ctx)
		defaultID := m.snapperNumberFromID(ctx, defaultRaw)
		if revertErr := m.revertDefaultSnapshot(ctx); revertErr != nil {
			return fmt.Errorf("staged snapshot failed validation AND revert failed: %v; revert: %w", err, revertErr)
		}
		// Remove the corrupt snapshot so it cannot be targeted by future
		// Rollback() calls or set as boot default through any other path.
		// Best-effort: if deletion fails, the revert already protects the
		// next boot, and snapper-cleanup will eventually age it out.
		m.runner.Run(ctx, "snapper", "delete", defaultID)
		return fmt.Errorf("staged snapshot failed validation, reverted to active and deleted snapshot %s: %w", defaultID, err)
	}
	_, _, _, err := m.runner.Run(ctx, "systemctl", "reboot")
	return err
}
```

`Reboot()` is the primary enforcement point because:
- It catches snapshots staged by piccolod's own `Apply()` path
- It catches snapshots set externally (manual `transactional-update`, system timer if somehow unmasked)
- It runs synchronously, at the moment of truth
- On validation failure, it **deletes the bad snapshot** — this prevents `Rollback()` or any other path from targeting it. Deletion is best-effort (the revert already protects the next boot).

Note: `Apply()` runs `transactional-update` with `wait=false` (`manager.go:220`), meaning `runTransactionalUpdate` returns after `systemd-run` queues the unit, not after TU completes. Hooking validation in the `runTransactionalUpdate` success path would run before the snapshot exists. `Reboot()` is the correct and only reliable enforcement point.

**Acknowledged gap:** Uncontrolled reboots (power cycle, kernel panic, OOM kill) bypass `Reboot()`. Phase 2 bootloader-level protection addresses this — systemd-boot boot assessment on x86/aarch64 PCs, u-boot bootcount on RPi.

**Fail-closed deadlock risk:** If `defaultSnapshot()` or `snapperNumberFromID()` fails due to transient command parsing issues (e.g., `btrfs subvolume list` output changes across versions), the API will refuse all reboots. This is the safe direction (blocks reboot rather than allows bad one), but can strand an operator. Mitigation: the existing `handleOSUpdateReboot` handler should accept an optional `force=true` query parameter that bypasses validation and snapshot deletion. This override must be audit-logged via the Event Bus and should return HTTP 409 Conflict (not 500) when validation fails without force, with structured error details listing the missing components. The override should be documented as an emergency-only escape hatch accessible through the web portal.

### 7.4 Enable health-checker.service explicitly

**File:** `piccolo-os/packages/piccolo-os-support/piccolo-os-support.spec` — `%post` section

Add:

```bash
# Belt-and-suspenders: explicitly enable health-checker for boot-time rollback.
# MicroOS preset (87-default-MicroOS.preset) already enables this, but we make
# it explicit since this service is a critical safety net and preset ordering
# relative to package installation is not guaranteed.
/usr/bin/systemctl --root=/ --no-reload enable health-checker.service
```

Note: The MicroOS preset at `/usr/lib/systemd/system-preset/87-default-MicroOS.preset` already contains `enable health-checker.service`. This explicit enable is belt-and-suspenders — harmless if redundant, protective if preset processing fails or is reordered.

## 8. Phase 2: Short-Term (Post-Beta)

### 8.1 Switch to systemd-boot (x86/aarch64 PCs)

**Threat covered:** T2, T5 — automatic boot-level fallback including first boot.

**Platforms:** VirtualBox, SelfInstall (x86_64), and standard aarch64 PCs with native UEFI firmware. **Not RPi** — see 8.2.

#### Why GRUB grubenv is unfixable

The GRUB `health_checker_flag` mechanism is broken on **all** Piccolo OS platforms (not just RPi, as originally believed). Post-RFC investigation confirmed:

1. `/boot/grub2/grubenv` lives on the read-only btrfs root snapshot on both x86 and aarch64 — `grub2-editenv /boot/grub2/grubenv list` returns empty on both VirtualBox MicroOS and RPi
2. GRUB's btrfs driver is read-only — `save_env` is a silent no-op
3. GRUB's btrfs driver **cannot read across btrfs subvolume boundaries** — confirmed by GRUB shell testing (`load_env -f /boot/grub2/x86_64-efi/grubenv` returns "not found" despite the file existing). This rules out moving grubenv to the existing writable `boot/grub2/x86_64-efi` subvolume.
4. GRUB shell testing on the ESP also failed to read grubenv via relative paths — the GRUB-to-ESP path mapping is non-trivial and varies by firmware.
5. GRUB2-BLS does **not** implement BLS filename-based boot counting (the `+tries` decrement mechanism). GRUB2's `blscfg` module reads BLS entries but does not rename entry files to track boot attempts. This means even with BLS entries on the writable ESP, GRUB2 cannot provide automatic boot assessment.

The original RFC proposed moving grubenv to the ESP. Investigation showed this is both harder than expected (GRUB path resolution) and insufficient (no BLS boot counting). The correct fix is to replace GRUB with a bootloader that natively supports boot assessment.

#### systemd-boot + systemd-bless-boot

systemd-boot is a lightweight UEFI boot manager that natively implements the [Automatic Boot Assessment](https://systemd.io/AUTOMATIC_BOOT_ASSESSMENT/) protocol:

1. `sdbootutil` creates BLS entries with tries counters (e.g., `entry+3.conf` — 3 attempts allowed)
2. systemd-boot decrements the counter on each boot attempt (renames `+3.conf` → `+2-1.conf`)
3. If `boot-complete.target` is reached (all health checks pass), `systemd-bless-boot.service` removes the counter entirely ("blesses" the entry)
4. If tries reach 0, systemd-boot automatically boots the previous blessed entry

All state lives on the ESP (FAT32, writable). No grubenv. No btrfs driver limitations. Works on first boot.

#### Kiwi config changes

**`piccolo-os.kiwi`** — VirtualBox and SelfInstall profiles:
- Add `sdbootutil` and `systemd-boot` to `<packages type="image">`
- Increase ESP: add `efipartsize="512"` (kernel + initrd live on ESP with systemd-boot)
- Remove GRUB-specific btrfs volumes (`boot/grub2/i386-pc`, `boot/grub2/x86_64-efi`)

The existing `disk.sh` already handles the systemd-boot path — when `sdbootutil` is installed and `systemd-boot` is present, it sets `loader_type="systemd-boot"` and runs `sdbootutil install` + `sdbootutil add-all-kernels`. No changes needed to `disk.sh`.

`config.sh` also already handles the sdbootutil path — kernel cmdline goes to `/etc/kernel/cmdline` when sdbootutil is present. No changes needed.

#### Boot assessment integration with health-checker

Configure health-checker to gate `boot-complete.target`:
- Set `/etc/kernel/tries` to `3` (3 boot attempts before fallback)
- health-checker.service runs the piccolod health check plugin
- On success: health-checker triggers `boot-complete.target` → `systemd-bless-boot` blesses the entry
- On failure: health-checker triggers `snapper rollback` + reboot → tries decremented → after 3 failures, systemd-boot falls back

#### Evidence

- systemd-boot is proven on openSUSE x86_64 (MicroOS QEMU appliance images, Tumbleweed experimental)
- `sdbootutil` explicitly supports aarch64 (builds available, architecture-specific paths handled in source)
- openSUSE's `disk.sh` scaffolding for systemd-boot already exists in the Piccolo OS kiwi config
- GRUB2-BLS is now the default on Tumbleweed (Nov 2025) but does not implement boot counting — only systemd-boot does

### 8.2 U-Boot bootcount (RPi only)

**Threat covered:** T2, T5 — bootloader-level fallback including first boot on RPi.

**Platforms:** RaspberryPi, Rock64 — devices that use u-boot as the first-stage UEFI provider.

**Why RPi can't use systemd-boot:** RPi uses u-boot's EBBR (Embedded Base Boot Requirements) implementation, which is a subset of full UEFI. systemd-boot has historically crashed when chainloaded by u-boot ([systemd#7585](https://github.com/systemd/systemd/issues/7585)), and u-boot's EFI variable support (`LoaderBootCountPath` etc.) is unverified. There are zero confirmed deployments of systemd-boot on RPi with openSUSE. RPi remains on the GRUB2 boot chain; u-boot bootcount provides the boot-level protection that systemd-boot provides on x86/aarch64 PCs.

**Mechanism:** U-Boot's `CONFIG_BOOTCOUNT_LIMIT` feature increments a `bootcount` variable on each boot. When `bootcount > bootlimit`, U-Boot executes `altbootcmd` instead of `bootcmd`. This works even on first boot (the initial image snapshot serves as the fallback target).

**Implementation outline:**
1. Custom u-boot build for RPi4/5 with `CONFIG_BOOTCOUNT_LIMIT=y` and `CONFIG_BOOTCOUNT_ENV=y`
2. Configure u-boot environment with `bootlimit=3`, `bootcmd` for current snapshot, `altbootcmd` for fallback
3. After transactional-update, piccolod uses `fw_setenv` to update snapshot variables
4. On successful boot (after health-checker passes), clear bootcount via `fw_setenv bootcount 0`
5. On repeated boot failure (3x), u-boot executes `altbootcmd`

**Dependencies:** Custom u-boot build via OBS, `u-boot-tools` package for `fw_setenv`/`fw_printenv`.

### 8.3 Hardware and systemd watchdog

**Threat covered:** T5, T6 — system hang, OOM, kernel deadlock.

**Mechanism:** RPi4 has a hardware watchdog (`bcm2835_wdt`). systemd can manage it:

1. Add `WatchdogSec=60` to `piccolod.service` unit
2. piccolod periodically calls `sd_notify("WATCHDOG=1")` (e.g., every 30s from a health-tracker tick goroutine)
3. If piccolod stops notifying (hang, crash), systemd restarts it (`Restart=always`)
4. systemd also enables the hardware watchdog via `RuntimeWatchdogSec=` in `system.conf`
5. If the entire system hangs (kernel deadlock, OOM), the hardware watchdog triggers a hard reboot
6. Boot assessment catches the resulting reboot cycle (systemd-boot on x86/aarch64, u-boot bootcount on RPi)

**Integration with existing code:** piccolod already uses `daemon.SdNotify(false, daemon.SdNotifyReady)` at `gin_server.go:787`. Adding `WATCHDOG=1` notifications requires periodic calls from a goroutine — the health tracker's tick interval is a natural fit.

### 8.4 Pre-reboot static analysis (systemd-analyze verify)

**Threat covered:** T1, T2 — validates systemd unit graph in staged snapshot.

Add `systemd-analyze verify --no-pager --recursive-errors=no --root=<snapshotRoot> default.target` to `validateStagedSnapshot()`. This catches broken unit files, missing dependencies, and cycles that file-existence checks miss. Takes 3-7 seconds on ARM.

Note: `--recursive-errors=no` limits checking to `default.target`'s direct dependencies, avoiding false positives from units we don't control. Empirical testing against a known-good snapshot is needed before deployment.

### 8.5 Container boot validation (systemd-nspawn)

**Threat covered:** T1, T4 — catches service-level and runtime failures that static analysis and file-existence checks miss. Does NOT cover T2 (missing kernel/initrd) because nspawn shares the host kernel.

Boot the staged snapshot as a lightweight systemd-nspawn container before rebooting into it:

```bash
timeout 30s systemd-nspawn --boot --read-only \
    -D /.snapshots/<N>/snapshot \
    --bind-ro=/var/lib/misc \
    --register=no
```

15-30 seconds on RPi 4. Cannot validate hardware-dependent services or kernel binary (covered by file-existence checks in 7.3a). No production embedded OS does pre-reboot VM testing on-device — this is defense-in-depth, not a replacement for post-reboot rollback.

## 9. Phase 3: Future

### 9.1 systemd-boot on RPi (when u-boot UEFI matures)

If u-boot's UEFI implementation matures to support the EFI variables required by systemd-boot (`LoaderBootCountPath`, `LoaderEntrySelected`, etc.) and the u-boot → systemd-boot chain is proven on RPi, the RPi platform could migrate from GRUB2 + u-boot bootcount to systemd-boot + systemd-bless-boot — unifying the boot assessment mechanism across all platforms.

**Current blockers:**
- systemd-boot has historically crashed when chainloaded by u-boot's UEFI emulation ([systemd#7585](https://github.com/systemd/systemd/issues/7585))
- U-Boot implements EBBR (a UEFI subset); runtime `SetVariable` support for `Loader*` variables is unverified
- Zero confirmed deployments of systemd-boot on RPi with openSUSE
- openSUSE labels systemd-boot as experimental; only QEMU images are provided

**Migration path:** Once these blockers are resolved upstream, the RPi kiwi profile would follow the same pattern as x86/aarch64 PCs (8.1): add `sdbootutil` + `systemd-boot` packages, increase ESP size, remove GRUB-specific volumes. The `disk.sh` and `config.sh` scaffolding already handles this transition.

## 10. Testing Plan

### Unit tests

1. **Health endpoint** (`internal/server/`): Add test cases for:
   - 200 when `Overall() == LevelOK`
   - 200 when `Overall() == LevelWarn` (normal boot pre-unlock)
   - 503 when `Overall() == LevelError` (fatal state)

2. **Snapshot validation** (`internal/update/`): Add test cases for:
   - No staged snapshot (default == active) → validation passes (no-op)
   - Staged snapshot with all critical binaries → passes
   - Staged snapshot missing kernel → fails with descriptive error
   - Staged snapshot missing systemd → fails with descriptive error
   - `defaultSnapshot()` returns "" → fails closed (error, not silent pass)
   - `snapperNumberFromID()` returns "" → fails closed
   - Validation failure triggers `revertDefaultSnapshot`

### Integration tests

3. **Timer masking**: Build test image, verify `systemctl status transactional-update.timer` shows `masked`
4. **Health-checker rollback**: Set a component to `LevelError`, verify health-checker script detects 503, verify snapper rollback triggers
5. **Normal update flow regression**: piccolod Apply → transactional-update via systemd-run → Reboot validates snapshot → reboot → health-checker passes → system operational
### Manual verification

6. **RPi first-boot**: Deploy new image to RPi, verify transactional-update.timer does NOT fire after NTP clock correction
7. **Upgrade path**: Existing v0.1.19 device receives updated `piccolo-os-support` via piccolod's Apply → `%post` masks timers for subsequent boots

## 11. Operational Considerations

### Upgrade path for deployed devices

Existing devices running v0.1.19 have `transactional-update.timer` active. They receive updates through piccolod's update manager, which invokes `transactional-update` via `systemd-run` (bypassing the timer). When the new `piccolo-os-support` package is installed as part of an update, the `%post` scriptlet masks the timer. The timer masking takes effect on the next boot into the updated snapshot.

### zypper dup vs zypper update

The current `UPDATE_METHOD=dup` means `zypper dup` is used for updates, which removes packages not in any enabled repository (by design — SUSE KB 000020400). An alternative is `UPDATE_METHOD=up` (`zypper update`), which only updates existing packages without removing orphans. This would have prevented the mass removal even with a corrupted solver.

However, `zypper up` does not handle distribution-level upgrades (new packages replacing old ones, package renames). For Tumbleweed-based MicroOS, `zypper dup` is the recommended update method. The pre-reboot snapshot validation (7.3) is the appropriate defense rather than weakening the update semantics.

### btrfs maintenance on non-SD devices

Timer masking applies to all Piccolo OS images (including VirtualBox). For VirtualBox/SelfInstall targets on SSDs where btrfs maintenance is appropriate, consider future per-profile timer configuration. For pre-beta, the global mask is acceptable — btrfs maintenance can be re-enabled via future OTA updates if needed.

## Cross-Repo Dependencies

Phase 1 spans two repositories:

| Item | Repository | Files |
|---|---|---|
| 7.1 Timer masking | `piccolo-os` | `packages/piccolo-os-support/piccolo-os-support.spec`, `kiwi/microos-ots/config.sh` |
| 7.2b Health endpoint | `piccolod` | `internal/server/gin_server.go` |
| 7.2c Health-checker script | `piccolo-os` | `packages/piccolo-os-support/piccolo-health-check.sh` |
| 7.3 Snapshot validation | `piccolod` | `internal/update/manager.go` |
| 7.4 Enable health-checker | `piccolo-os` | `packages/piccolo-os-support/piccolo-os-support.spec` |

The `piccolod` changes (7.2b, 7.3) ship as a new daemon version. The `piccolo-os` changes (7.1, 7.2c, 7.4) ship as a new `piccolo-os-support` package version. Both should release together for full protection. The health-checker script change (7.2c) is backward-compatible with older piccolod (old endpoint returns 200), so staggered rollout is safe — just not fully protective until both land.

Phase 2 adds kiwi config changes for systemd-boot (x86/aarch64 PC profiles only):

| Item | Repository | Files |
|---|---|---|
| 8.1 systemd-boot migration | `piccolo-os` | `kiwi/microos-ots/piccolo-os.kiwi` (VirtualBox, SelfInstall profiles: add sdbootutil/systemd-boot packages, increase efipartsize, remove GRUB volumes) |

Note: `disk.sh` and `config.sh` already handle the systemd-boot path — no changes needed to these scripts.

## Implementation Notes & Status

| # | Item | Phase | Platform | Status |
|---|------|-------|----------|--------|
| 7.1 | Mask dangerous timers (spec + config.sh) | 1 | All | Pending |
| 7.2b | Health endpoint 503 on LevelError | 1 | All | Pending |
| 7.2c | Health-checker script → `/health/ready` | 1 | All | Pending |
| 7.3 | Pre-reboot snapshot validation (file-existence checks) | 1 | All | Pending |
| 7.4 | Enable health-checker.service | 1 | All | Pending |
| 7.3+ | Force-reboot override (`force=true` query param) | 1 | All | Pending |
| 8.1 | Switch to systemd-boot + systemd-bless-boot | 2 | x86/aarch64 PCs | Design only |
| 8.2 | U-Boot bootcount | 2 | RPi, Rock64 | Design only |
| 8.3 | Hardware/systemd watchdog | 2 | All | Design only |
| 8.4 | Pre-reboot static analysis (systemd-analyze verify) | 2 | All | Design only |
| 8.5 | Container boot validation (systemd-nspawn) | 2 | All | Design only |
| 9.1 | systemd-boot on RPi (when u-boot UEFI matures) | 3 | RPi | Future |
