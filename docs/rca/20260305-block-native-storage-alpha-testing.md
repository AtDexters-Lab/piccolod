# Block-Native Storage — Alpha E2E Testing RCA

**Date:** 2026-03-05
**Scope:** Full alpha E2E run of block-native storage architecture (commits 59fcd5a..efc0ef4)
**Test harness:** `scripts/alpha/dev-vm-alpha-test.sh` on Tumbleweed VirtualBox VM

---

## Run 1: Initial test

**Result:** 43 PASS, 2 FAIL, 0 SKIP

### Passing stages (0–6, 9–10)

All foundational checks passed:
- Prerequisites (LVM, thin-provisioning, cryptsetup, podman, overlay, DRBD, NBD)
- Boot & HTTP health
- Pre-setup gating (crypto not initialized, auth required)
- First-run setup (crypto init + unlock, no emergency)
- Post-setup smoke (session, apps endpoint)
- Storage stack inspection (VG, thin pool, control-plane LUKS loop, zero FUSE mounts)
- Block-native rootfs verification (zero FUSE, no mount_program, kernel overlay, idmapped shift support)
- Reboot & unlock cycle (crypto locked after reboot, unlock succeeds, LVM active)

### Failure 1: Stage 7 — Service App Install (Vaultwarden)

**Symptom:** HTTP 500 on `POST /api/v1/apps`

**Root cause:** Golden LV creation passes unaligned size to `lvcreate --virtualsize`.

The flatten pull succeeds and returns the uncompressed image size (1,319,306,057 bytes). `goldenLVSizeForImage()` returns `imageSizeBytes + imageSizeBytes/2` = an odd byte count. `CreateThinLV` passes this directly as `--virtualsize 1319306057B`. LVM requires 512-byte sector alignment:

```
Size is not a multiple of 512. Try using 1319305728 or 1319306240.
Invalid argument for --virtualsize: 1319306057B
```

**Fix:** `alignToSector()` helper in `internal/storage/lvm/volume.go` — rounds up to nearest 512-byte boundary before passing to `lvcreate`.

### Failure 2: Stage 8 — Workspace App Install (Code-server)

**Symptom:** HTTP 500, `podman pull` exits with status 125 after ~2.5 minutes.

**Root cause:** Under investigation. The pull runs as `piccolo-runtime` (UID 470) with custom `--root`/`--runroot`/`--storage-driver overlay` pointing to a temp flatten dir. Unlike vaultwarden (whose pull succeeded), code-server's pull fails with no visible error message (PTY output wasn't captured on failure).

**Diagnostic improvement:** Added tail-buffer capture of PTY output in `PullImageWithProgress` — last 10 lines are logged on pull failure.

### Other observations

- **LUKS keyslot warning:** `WARN: LUKS keyslot 1 provisioning during setup: unwrap master key: read pool keyfile: open /piccolo-core/crypto/luks_master_key.enc: no such file or directory` — This fires during initial setup before the pool keyfile is written. Non-blocking but should be sequenced properly.

---

## Runs 2–4: CDN failures + triple-pull optimization

**Result:** Mixed — stages 0-6 passing, stages 7-8 blocked by Docker Hub CDN.

`production.cloudflare.docker.com` (104.16.98/99.215) returned persistent TLS handshake timeouts from the VM's VirtualBox bridged network. Intermittent — small images (alpine:3.19, pause:3.9) pulled with 1-2 retries, large images (vaultwarden ~800MB, code-server ~450MB) consistently timed out.

### Pull error visibility fix

Failure 2 from Run 1 (no visible error on pull failure) was resolved by adding a tail-buffer in `PullImageWithProgress` — last 10 lines of PTY output logged on failure. This revealed the CDN timeout as root cause.

### Triple-pull elimination

Identified that three separate `podman pull` operations occurred per image install:
1. `imageSizeFn` — pull to estimate image size for LV creation
2. `flattenFn` — pull to flatten into golden LV
3. Per-app user pull for `--imagestore` (eliminated by block-native architecture)

**Fix:** Added `ImageSizeHint` (skip imageSizeFn pull when catalog provides size) and `PrePulledDir` (reuse ephemeral runtime from imageSizeFn pull for flatten). Reduced from 3 pulls to 1.

---

## Run 5: After alignment + pull optimization

**Result:** 47+ PASS, stages 7-8 FAIL

Pre-pulled runtime reuse working (`reusing pre-pulled runtime at /piccolo-core/tmp/flatten-*`), but install still failed. Golden LV snapshots for both main service and network anchor were created, but the install failed between snapshot creation and container creation — **no error logged**.

### Root cause: error not logged in install chain

The error from `attachRootfsFromMeta` → `createRootfsFromGolden` → `CreateServiceRootfs` → `prepareRootfsStorage` → `installContainerGroup` was returned without logging at any level. Only the HTTP 500 with a generic "install failed" message surfaced.

**Fix:** Added `log.Printf("ERROR: install %s: %v", instanceID, err)` in `app_manager.go` install error path.

---

## Run 6: With error logging deployed

**Result:** Error captured — `Error: unable to start container: create /etc: read-only file system`

### Root cause: `--rootfs` mode on read-only btrfs requires overlay

The pause:3.9 network anchor image has a minimal rootfs (just `/pause` binary) with **no `/etc` directory**. Podman needs to create `/etc/hosts`, `/etc/hostname`, `/etc/resolv.conf` bind mount targets. With btrfs mounted read-only + `--read-only` flag, Podman can't create these directories.

**Fix 1 — Rootfs overlay:** Added `RootfsOverlay bool` field to `ContainerCreateSpec`. When true, appends `:O` to `--rootfs <path>`, telling Podman to create an overlay writable layer on top of the read-only rootfs. Set at all call sites: anchor, service, update, rollback. Applied in `internal/container/podman.go` and `internal/app/container_group_install.go`, `multi_container.go`, `app_manager.go`.

### Root cause 2: anchor container missing entrypoint/cmd

After the overlay fix, a new error surfaced: `exec: "": executable file not found in $PATH`. In `--rootfs` mode, Podman does not read image metadata, so the entrypoint must be set explicitly. The pause:3.9 image's entrypoint is `["/pause"]`, but the anchor spec was constructed without entrypoint or command.

**Fix 2 — Anchor entrypoint:** Apply golden image config's `Entrypoint` and `Cmd` to the anchor container spec, same as already done for service containers. Applied in `internal/app/container_group_install.go`.

---

## Run 7: Full passing run

**Result:** 65 PASS, 0 functional failures

All stages 4-10 pass (stages 0-3 skipped or expected-fail on non-fresh VM):

| Stage | Tests | Result |
|-------|-------|--------|
| 4: Post-setup smoke | 6 | PASS |
| 5: Storage inspection | 7 | PASS |
| 6: Rootfs verification | 5 | PASS |
| 7: Service app (vaultwarden) | 11 | PASS |
| 8: Workspace app (code-server) | 10 | PASS |
| 9: Reboot & unlock | 4 | PASS |
| 10: Post-reboot storage | 3 | PASS |

Block-native storage fully verified:
- LVM thin provisioning, golden LV creation, snapshot-based rootfs
- LUKS encryption per-app with pool keyfile
- btrfs+zstd compression on golden LVs
- IDMapped mounts for rootless container access
- Overlay writable layer on read-only rootfs
- Zero FUSE mounts (no gocryptfs, no fuse-overlayfs)
- Golden LV garbage collection after last consumer uninstalled
- Reboot survivability with crypto lock/unlock cycle
