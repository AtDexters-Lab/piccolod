# Workspace rootfs left on a deleted app user's ID map

## Observed incident

On 25 October 2026, `git pull` failed inside the `piccolospace` workspace on
the Linode node (v0.2.47). Inside the container every file showed as owned by
65534, creating files failed with `EOVERFLOW`, and nginx kept serving the
April files because reads still worked. Pulls had worked until about
19 April 2026.

On the node:

- `ws-piccolospace` and `svc-rootfs-piccolospace--__netns__` recorded
  `app_uid 474, app_gid 472`, subordinate range `327680+65536`, with a pinned
  `idmap_fingerprint`.
- `pa-piccolospace` was uid/gid 468/468 with the same subordinate range; the
  container's `uid_map` was `0 468 1` / `1 327680 65536`.
- On-disk owners were container-relative (the raw mount's `/root` was `0 0`).
- `/etc/shadow` dated `pa-piccolospace` to 12 June 2026. UID 468 follows
  `pa-git`'s 469 (19 May): shadow-utils gives a new system user one below the
  lowest one in use, so the account was deleted and re-created, not renumbered.
  Every other app's volumes matched its user.

## Cause

A rootfs volume's ID map was written once, at creation, and every attach built
the idmapped bind from that record. Since `389d7ca` (26 April) the record was
also fingerprint-locked. The container's user namespace, however, comes from
the app user's live IDs. Nothing compared the two, so after the user came back
as 468 the bind still presented root-owned files as host UID 474. That UID is
outside the container's map, which explains both symptoms: unmapped owners
show as the overflow UID, and the kernel cannot map the writer back, so
creates fail with `EOVERFLOW`.

Pinning was never needed for correctness. On-disk owners are
container-relative, so the right map is always a pure function of the app
user's live UID, GID and subordinate ranges. A pinned copy can only match that
or be wrong, and the lock guaranteed that a wrong copy survived. A stale copy
is also a latent exposure: if the old outer UID or a freed subordinate range is
later given to another app's user, the stale bind presents this app's files
as that app's. Artifact references kept the same pinned copy and failed with
"already names different durable content" after an ID change.

### Why the user was deleted

piccolospace's stored definition stopped parsing after an upgrade, and the
startup orphan cleanup of that era read "missing from the app list" as
"orphaned".

| When (IST) | What happened |
|---|---|
| 28 Mar 15:36–15:38 | piccolospace installed: data volume, workspace and anchor rootfs, app record (`created_at`). |
| 31 Mar 22:02 | `nginx` listener added on guest port 80; the service kept `bind_ports: []` (`app.prev.yaml` has no listeners). |
| ~19 Apr | Last working pull; `pa-piccolospace` still 474/472. |
| 1 Jun 18:43 | `4711e91` ("feat: add TLS mux tunnel mTLS auth") adds a parser rule: every listener's `guest_port` must be declared in exactly one service's `bind_ports`. Released in v0.2.24. |
| 2 Jun 00:11 | The node installs v0.2.24. On its next start, piccolod cannot parse piccolospace's `app.yaml`. `loadCache` prints a warning and leaves the app out of `ListApps()`. The first reconcile's orphan cleanup, whose known set was only `ListApps()`, kills `pa-piccolospace`'s processes and runs `userdel --remove`. The volumes survive; the app is down and absent from the app list. |
| 3–12 Jun | v0.2.25, v0.2.27 and v0.2.28 keep rejecting the definition. |
| 12 Jun 16:46:47 | `app.yaml_back` saved by hand. |
| 12 Jun 16:47:51 | `app.yaml` edited: `bind_ports: []` → `bind_ports: [80]`. |
| 12 Jun 16:48:34 | piccolod provisions a new `pa-piccolospace` as 468/468 with the same subordinate range; its home folder and linger file are born together. Both rootfs volumes still record 474/472, so the workspace comes back with unwritable root-owned files. |

Evidence, all read on the node on 25 October:

- zypper history gives the install times;
- `stat` birth times of `/home/pa-*`, the linger files, the app records and
  the volumes;
- the app folder holds `app.yaml_back` and `app.prev.yaml`;
- `transactional-update.log` records no rollbacks, and `pa-git` (19 May)
  kept its account, which rules out an `/etc` restore.

Parsing `app.yaml_back` with each release the node ran: v0.2.12–v0.2.23
accept it, while v0.2.24, v0.2.25, v0.2.27 and v0.2.28 fail with
`listener 'nginx' guest_port 80 must be declared in exactly one service
bind_ports`. The repaired `app.yaml` parses on all of them.

The exact minute of the deletion is unrecorded: the journal starts in
October and the oldest snapshot is from 29 June. It was the first unlocked
start of v0.2.24, on or after 2 June.

Since `91aaab2` (v0.2.41) orphan cleanup also counts app directories, and
since this fix it also counts surviving volumes, so the same parse failure
would no longer delete the user.

## Repair

The live app user is the only authority for an app's ID map:

- Every rootfs and artifact create, clone and attach receives the owning app
  user's live map (`AppManager.appIDMap`): UID/GID from the runtime
  credential, ranges from `/etc/subuid` and `/etc/subgid`. A missing
  credential or subuid range fails closed instead of producing a partial map;
  a missing subgid entry falls back to the subuid range, which provisioning
  allocates for both.
- Volume metadata records the map the bind was last built with. When an
  attach finds a different record and no bind built from it is mounted, it
  rewrites the record and fingerprint (`writeLiveIDMap`, logged old → new) and
  builds the bind from the live map. `writeVolumeMetaV3` still rejects map
  changes from every other writer.
- A bind still mounted with an older map may back running containers. Attach
  refuses it with `ErrIDMapStaleMount`, naming both maps, rather than
  returning it or rebuilding it underneath them. Stopping the app detaches
  its volumes, so the next start rebuilds the bind.
- Artifact reference records follow the live map the same way.
- Startup orphan cleanup treats any surviving app data volume, rootfs volume
  or artifact reference as an installed app, so a user is never deleted while
  its app's data exists.

No migration or chown is needed. A stale record is corrected on the next
attach. Records stay consistent with their fingerprints, so a rollback to an
older binary attaches the rewritten record unchanged.

piccolod's shutdown drain stops apps and detaches their volumes, so a
graceful restart or a reboot corrects a drifted volume on the next start. Only
an exit that skips the drain (a crash) can leave a stale bind mounted under
running containers; the next attach of that volume then fails loudly until the
app is stopped and started. For `piccolospace`, the release's reboot leaves
both volumes detached, so the first start after unlock records 468/468 and
mounts correct binds.

## Verification

Unit tests cover:

- the attach decision for every record/live/mount combination;
- refusal of a stale mounted bind (Attached, or a bind left without its raw
  mount) and of a stale mounted artifact projection;
- end-to-end attaches that build the rootfs bind and the artifact projection
  from the live map;
- the rewrite keeping fields stamped by unlocked writers;
- the immutability guard for other writers;
- clone and artifact-record follow;
- the strict live map and its subgid fallback;
- orphan cleanup keeping users whose apps have volumes, and waiting when
  volumes cannot be listed.

Mutation checks confirm that removing any of these guards fails its test.

Alpha stage `rootfs-idmap-drift` (stage 24) reproduces the field failure on a
VM: it installs a workspace, deletes its app user the way orphan cleanup does,
and lets piccolod re-create it with a new UID. It ran on the alpha template's
disk under QEMU/KVM with a throwaway qcow2 overlay, because VirtualBox cannot
start while `kvm_amd` holds AMD-V (`VERR_SVM_IN_USE`).

- v0.2.47: the user moved 465 → 464 and both records stayed at 465. As
  container root, `/root` showed `65534:65534` and creating a file there
  failed; on the host the bind showed `465:465` over an on-disk `0:0`.
  Checks 24.7–24.11 fail.
- Same drifted app, upgraded to the fix: the restart's drain detached the
  volumes, and the next start logged both records following `uid 465 → 464`.
  `/root` and the pre-drift file showed `0:0` and new files could be created.
- Fixed binary, fresh run: all 12 checks pass. The re-created user (465 →
  464) gets both records rewritten on the next start, the pre-drift file is
  still container-root owned, and new files can be created. The existing
  service-app (11 checks) and workspace-app (10 checks) stages also pass.

## Remediation Status

- Fix, tests and alpha stage: PR AtDexters-Lab/piccolod#47
  (`fix/rootfs-idmap-follows-app-user`).
- Deletion dated and explained: the first v0.2.24 start after 2 June 00:11
  (see "Why the user was deleted").
- Follow-up: `loadCache` still skips an app whose stored definition no longer
  parses, with only a printed warning. Its user and volumes now survive, but
  the app silently stops being reconciled and listed. A tightened validation
  rule needs a migration for stored definitions, and an unloadable record
  should be surfaced instead of dropped.
- Follow-up: uninstall and install cleanup destroyed the network anchor
  rootfs only when the definition had `Services`. The gate was latent: the
  parser has required `services` since January 2026, before anchors had their
  own rootfs, and the one nil-definition call runs after a call that covers
  the anchor. `destroyAllServiceRootfs` now destroys
  `svc-rootfs-<id>--__netns__` and its versioned variants regardless
  (PR AtDexters-Lab/piccolod#48). To spot a leaked anchor on a node, list
  anchor volumes with no app record:
  `comm -23 <(ls /piccolo-core/volumes | sed -n 's/^svc-rootfs-\(.*\)--__netns__.*/\1/p' | sort -u) <(ls /piccolo-core/mounts/control-plane/apps | sort)`.
  App records live in the control volume, so the second listing reads
  `/piccolo-core/mounts/control-plane/apps`.
