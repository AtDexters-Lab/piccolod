# Block-Native Storage Implementation Plan

**Date:** 2026-02-26
**Status:** Implementation
**Target RFC:** `org-context/03_engineering/storage_architecture_block_native.md`

## Summary

Replace FUSE-based storage (gocryptfs for encryption, fuse-overlayfs for container overlay) with a zero-FUSE block-native stack: per-volume LUKS2 on LVM thin LVs (ext4), kernel-native overlayfs, a LUKS loop file for the control plane, and DRBD+NBD as the cluster/tiering foundation.

## Design Decisions

1. **Strict no-FUSE**: Hard fail if native overlay unavailable. No `mount_program` fallback.
2. **No backward compatibility**: No migration code, no dual-path logic.
3. **No pool-level LUKS**: LVM VG on raw partition. Per-volume LUKS2 encryption.
4. **Remove Argon2id on recovery mnemonic**: Mnemonic key used directly as LUKS passphrase.
5. **Always full stack**: ALL app volumes use `thin LV → NBD → DRBD → LUKS → ext4`.

## Block Device Stack

```
dm-thin LV         ← thin provisioning (M1)
  ↑
NBD server/client  ← userspace block I/O, PSFN hooks paused (M2)
  ↑
DRBD               ← replication, standalone/paused in single-node (M2)
  ↑
LUKS2              ← per-volume encryption (M3)
  ↑
ext4               ← mounted filesystem, visible to containers (M3)
```

Control plane: `loop file → LUKS → ext4` (no thin LV, no NBD, no DRBD).

## Milestones

### M1: LVM Thin Pool
### M2: NBD + DRBD
### M3: Per-Volume LUKS2
### M4: Zero FUSE

See implementation plan for full details.
