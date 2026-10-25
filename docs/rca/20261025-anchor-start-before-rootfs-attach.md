# Graceful restart recreated every app's container group

## Observed incident

On 25 October 2026, while verifying the piccolospace ID map fix (PR
AtDexters-Lab/piccolod#47) on an alpha VM, every `systemctl restart piccolod`
logged this for each app:

```
INFO: attached anchor rootfs svc-rootfs-<id>--__netns__ (mount=/piccolo-core/mounts/svc-rootfs-<id>--__netns__-idmap)
INFO: app <id>: anchor start failed (podman start failed: ... runc create failed: unable to start container process: error during container init: exec: "/pause": stat /pause: no such file or directory ...), recreating container group
```

The recreate succeeded, so apps came back, but every graceful restart replaced
every app's containers. v0.2.47 and v0.2.48 behave the same.

## Cause

`reconcileContainerGroup` started an existing, stopped network anchor before
it attached the anchor's rootfs.

- The shutdown drain (`StopAllApps` → `quiesceContainerGroup` →
  `finalizeQuiescedContainerGroup`) stops each group and detaches its rootfs
  volumes: idmapped bind, raw btrfs mount, LUKS mapper and thin LV. It keeps
  the containers. Their metadata lives in `/run/piccolo/podman`, which
  survives a service restart.
- The anchor was created with
  `--rootfs /piccolo-core/mounts/svc-rootfs-<id>--__netns__-idmap:O`. After
  the detach, that path is an empty directory. `podman start` uses it as the
  overlay's lower layer, so the container has no `/pause`.
- Since `9a7bd3e` (21 May 2026, v0.2.22), reconcile attaches rootfs through a
  lazy, memoized accessor. Only the create and recreate branches called it:
  the plan classified the plain anchor and service starts as not needing
  rootfs handles. On the first reconcile after a restart, the anchor start
  therefore ran against the detached path. Only the recreate fallback then
  attached the volumes and replaced the group. That attach logs "attached
  anchor rootfs", which is why the line precedes the failure in the journal.

Manual Start was unaffected: `startContainerGroup` attaches rootfs and
artifacts before it starts anything. A reboot clears `/run/piccolo/podman`, so
reconcile finds no containers and takes the create path, which attaches first.

The suspected stale rootless mount namespace is ruled out. The app user's
Podman pause process (`catatonit -P`) survived every restart, and the bind
created after a restart appears in its mount namespace as a slave of the host
mount (`shared:343` on the host, `master:343` in the pause namespace). The
recreated anchor's overlay uses that bind as its `lowerdir`.

## Evidence

Alpha VM running v0.2.48 with a code-server workspace, a mount-event watcher
and a `podman` wrapper that logs each `start`, `create` and `rm`, on the first
reconcile after a drain:

```
15:50:07.177  mount  /piccolo-core/mounts/app-codeserver
15:50:07.494  podman start 0000afba...   (existing anchor)
15:50:07.815  mount  /piccolo-core/mounts/ws-codeserver
15:50:07.827  mount  /piccolo-core/mounts/ws-codeserver-idmap
15:50:08.137  mount  /piccolo-core/mounts/svc-rootfs-codeserver--__netns__
15:50:08.145  mount  /piccolo-core/mounts/svc-rootfs-codeserver--__netns__-idmap
15:50:08.139  "attached anchor rootfs", then "anchor start failed (... /pause ...)"
15:50:08.524  podman rm (workspace), 15:50:08.630 podman rm (anchor)
15:50:08.817  podman create --rootfs .../svc-rootfs-codeserver--__netns__-idmap:O
15:50:08.854  podman start b6edb688...   (new anchor, succeeds)
```

The anchor started about 650 ms before its rootfs was mounted. The mount
watcher polls, so its timestamps trail piccolod's by a few milliseconds.

## Impact

- Every graceful restart of piccolod removed and recreated every app's
  containers instead of starting them, and logged a start failure for each
  app.
- Recreating a container discards its writable layer, so files written
  outside volumes were lost at each restart. Volume data was not affected.
- Crashes and reboots were not affected: a crash skips the drain, so nothing
  is detached, and a reboot leaves no containers to start.

## Repair

Every reconcile branch that starts an existing container first attaches what
that container's recorded spec mounts:

- before starting the anchor, its rootfs;
- before starting a stopped service, its rootfs and, if it mounts any, the
  app's artifact references.

The drain also detaches artifact references and removes their projection
directories. Without the second rule, a service with an artifact mount would
fail its start and be recreated on its own.

The accessors stay lazy and memoized. A steady-state pass, in which every
container already runs, still attaches nothing, which was the point of
`9a7bd3e`. A start that fails anyway still falls back to the existing
recreate with the same handles.

## Verification

- `TestReconcileAttachesMountsBeforeStartingQuiescedGroup` models a drained
  group whose containers start only while their rootfs and artifact mounts
  are attached. It covers service mode, service mode with the anchor still
  running, and workspace mode. It requires the original containers to restart
  in place and a following steady-state pass to attach nothing. It fails on
  the old code, and removing any one of the three attach-before-start calls
  fails at least one case.
- Alpha stage `restart-in-place` (stage 25) installs a workspace and stops
  piccolod. It checks that the drain stopped the group and detached the anchor
  rootfs, then starts piccolod. Finally it checks that the same anchor and
  workspace containers run and that no anchor start failure is logged.
  v0.2.48 fails 25.5 and 25.6. The fixed binary passes all seven checks.
- On the VM with the fix, a drain-and-start and a `systemctl restart` brought
  back a workspace (code-server) and a service-mode app (Vaultwarden) on their
  original containers, both serving. Each `podman start` followed its
  attach. Stages service-app (11 checks), workspace-app (10),
  rootfs-idmap-drift (12) and oom-recovery (50) still pass. In oom-recovery,
  the workload-loss recoveries restarted their containers in place through
  the changed path.

## Remediation Status

- Fix, unit test and alpha stage: PR AtDexters-Lab/piccolod#49
  (`claude/anchor-pause-restart-110e20`).
- Follow-up, existed before this fix: when one volume failed to attach,
  `ensureAllServiceRootfsAttached` rolled back by detaching every volume it
  had collected. That included volumes that were already attached and may
  back running containers. The attach-before-start calls made this reachable
  from restarting one stopped container while its siblings run. There, a
  plain `podman start` used to need no attach. Fixed in PR
  AtDexters-Lab/piccolod#50: `AttachRootfs` marks a handle `AlreadyAttached`
  when it found the volume attached, fully or in part, and the rollback
  leaves such volumes attached.
- Not fixed, cosmetic: Podman's error output ends in a newline, so the
  "anchor start failed (...), recreating container group" line is split into
  two journal entries.
