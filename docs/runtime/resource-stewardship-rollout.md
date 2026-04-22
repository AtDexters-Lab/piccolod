# Resource Stewardship Rollout & Migration

Operational note for the resource-stewardship schema change
(see `.claude/plans/resource-stewardship.md` and L1-L3 design history).

## Background

The resource-stewardship schema replaces the pre-v2 `resources.limits`
manifest shape (per-service, numeric memory/CPU caps) with a new
app-level declaration of priority tier + memory profile + floor +
optional storage ceiling. See D-6 in the plan for schema details.

piccolod rejects the legacy shape at parse time with a hard error
directing the operator to run a catalog sync. There is **no in-process
backward-compatibility mapping** — the deployment footprint at the time
of rollout (N=1 user, 2 catalog-authored apps) made a coordinated
migration strictly simpler than maintaining a dual-schema parser, test
matrix, and persistence-migration code path.

## Rollout sequence

1. **Phase 0 (already shipped via catalog sync, no piccolod release):**
   `piccolo-store/apps/immich/app.yaml` machine-learning memory bumped
   from `2GB` to `6GB` in the legacy schema. This reaches the deployed
   user through the existing `classifyDiff → structural → recreate`
   flow and unblocks the reported OOM issue without any piccolod
   change.
2. **Phase 2 (this release):** piccolod parser switches to new-shape
   only. Catalog manifests (`immich/app.yaml`, `convertx/app.yaml`,
   `namek/app.yaml`) re-authored to new shape in the same release.
   Legacy-shape manifests produce a hard parse error; the deployed
   user's existing on-disk `app.yaml` files (still in legacy shape) will
   need to be resynced from the catalog before any piccolod restart
   into the new binary.

## Deployed-user coordination steps

1. **Announce.** Notify the user that a schema migration is landing.
   Give them the command(s) below and an expected window.
2. **Pre-upgrade catalog sync.** Have the user run `piccolod catalog
   sync` (or the UI equivalent) against the newly-authored catalog
   *before* upgrading piccolod. Each installed app's on-disk
   `app.yaml` gets overwritten with the new-shape manifest. Containers
   recreate per the sync diff.
3. **Piccolod upgrade.** Upgrade piccolod to the new release. On
   startup, `ReconcileAllSlicePolicies` writes per-app slice drop-ins
   under `/etc/systemd/system/user-<uid>.slice.d/piccolo-resources.conf`
   and applies them live via `systemctl set-property`. No container
   restart is needed for the stewardship transition.
4. **Verify.** After upgrade:
   - Check each app slice has the expected MemoryHigh/MemoryMax/CPUWeight:
     `systemctl show user-<uid>.slice | grep -E 'MemoryHigh|MemoryMax|CPUWeight'`.
   - Inspect `/etc/systemd/system/user-<uid>.slice.d/piccolo-resources.conf`.
   - Tail piccolod logs for `slice policy written for user-<uid>.slice`.

## What happens if the order is wrong

- Piccolod upgraded *before* catalog sync: existing installs have
  legacy-shape `app.yaml` on disk. Piccolod refuses to load them with a
  parse error instructing the operator to run a catalog sync. Fix: run
  `piccolod catalog sync` — manifests are rewritten in-place, piccolod
  picks them up on next sync cycle (or after a restart).
- Catalog synced but piccolod not yet upgraded: no harm. Legacy
  piccolod accepts new-shape manifests as unknown-but-permitted fields
  and silently ignores them (the old schema doesn't have new-shape
  fields, so the YAML decoder drops them with `strict: false` default).
  On upgrade, the new parser re-validates successfully.

## Rollback

If the Phase 2 release introduces a regression, rollback is:
- Revert piccolod binary to the previous release.
- Revert the catalog manifests (via `git revert` in `piccolo-store`),
  restoring the legacy shape.
- The deployed user's on-disk `app.yaml` will be overwritten back to
  legacy shape on the next catalog sync.
- Slice drop-ins left behind under
  `/etc/systemd/system/user-<uid>.slice.d/` are harmless — previous
  piccolod versions don't know about them and systemd continues to
  honor them until the file is deleted or the slice is torn down.
  Manual cleanup (`rm -f /etc/systemd/system/user-*.slice.d/piccolo-resources.conf &&
  systemctl daemon-reload`) is safe if desired.

## Future state

Once the footprint grows beyond the coordinated-migration-friendly
scale (say, >10 deployed nodes), a follow-up will introduce an
in-binary compatibility mode: the parser will accept both shapes for a
deprecation window, auto-translate legacy → new at parse time, and
persist the normalized form. Until then, hard-rejection-plus-catalog-sync
is the simpler and stricter correctness surface.
