# MicroOS transactional-update integration in piccolod

## Problem
- piccolod currently stubs OS update status and apply/rollback APIs; UI and acceptance tests require real transactional-update behavior on MicroOS images.
- We must reuse the built-in MicroOS updater (transactional-update.service) rather than invent new helpers, while keeping piccolod installable on non-MicroOS hosts (graceful no-op there).

## Goals
- Surface accurate OS update state on MicroOS: active vs default (staged) snapshot, last run result, and optional available RPM counts.
- Allow “Update now” and rollback actions through the existing transactional-update tooling, with clear in-progress/timeout handling and reboot prompts.
- Gate mutating update actions on unlocked state; leave status read-only.
- Keep state lightweight and avoid new systemd units; persist intent/outcomes in the Piccolo state dir (`PICCOLO_STATE_DIR`, default `/var/lib/piccolod`) while using `/run` only for short-lived in-progress markers.

## Non-Goals
- Supporting non-MicroOS apply/rollback flows (status returns unsupported).
- Coordinating cluster-wide rolling updates; updates remain per-node.
- Persisting rich history beyond a small cache.

## Proposal
- Implement a real `internal/update.Manager` that:
  - Detects MicroOS by the presence of transactional-update/snapper/btrfs/findmnt.
  - Reads status via: `findmnt -no SOURCE /` (active snapshot), `btrfs subvolume get-default /` (default/next-boot), `snapper --json list` for IDs/descriptions/timestamps, and `systemctl show transactional-update.service -p Result -p ExecMainStatus -p ActiveEnterTimestamp --value` for the last run result/exit code/timestamp; optionally fetch recent logs with `journalctl -u transactional-update.service -n 10 --no-pager --output=cat` for human context. Optionally run `zypper --xmlout lu` for available RPM counts.
  - Derives staged flag when default snapshot ID != active snapshot ID; exposes both active and staged piccolod rpm versions (`rpm -q piccolod` and `rpm --root /.snapshots/<id>/snapshot -q piccolod`).
  - Runs apply via `systemd-run transactional-update dup` asynchronously (no `--wait`) with single-flight guard; maps “already running” to an in-progress error; enforces a hard timeout (default 45m, env `PICCOLO_UPDATE_TIMEOUT_S`) when explicitly waiting (tests/tools). Clients trigger and then poll `/updates/os` for progress.
  - Runs rollback via `systemd-run transactional-update rollback <id>` asynchronously after validating target snapshot from snapper JSON; warns/discards newer staged snapshot when applicable; auto-pick skips staged/default snapshot.
  - Persists the last requested action (apply/rollback), target snapshot, timestamps, and immediate execution result under the Piccolo state dir (`PICCOLO_STATE_DIR`/`/var/lib/piccolod/update/state.json`) so intent survives reboot. On status reads after reboot, piccolod derives the final outcome by comparing the active snapshot to the recorded target. Short-lived in-progress markers may still live in `/run`, but anything needed after reboot is stored in the persistent state dir.
- Wire Gin handlers:
  - Replace `/api/v1/updates/os` stub with real status; keep existing schema fields and tuck extras (snapshot IDs/descriptions, rpm counts, piccolod active/staged versions, last run summary) under a backward-compatible `meta` object.
  - Add POST `/api/v1/updates/os/apply` and `/rollback`, both behind `requireUnlocked`; return 429 on in-progress, 4xx on invalid snapshot, and 5xx on execution failure.

## API / UI Impact
- No schema break: `current_version`, `available_version`, `pending`, `requires_reboot`, `last_checked` stay. Extras returned in `meta` for newer UI panels.
- UX can show: Up to date, Update staged – reboot to apply, last run time/result, available RPM count, piccolod active vs staged version, and which snapshot will boot next.

## Operational Considerations
- We rely on transactional-update.timer; piccolod does not start its own timer. If TU binaries are missing, status reports `unsupported` and apply/rollback return an error.
- Actions are gated on unlocked state; leader role not required (single-node update semantics).
- Timeout and unit names are fixed per invocation to aid journald troubleshooting; logs include unit name, exit code, and duration.

## Testing Plan
- Unit tests with a fake command runner feeding canned outputs for findmnt/btrfs/snapper/zypper/log tail to cover: up-to-date, staged, apply in-progress, timeout, rollback target validation, unsupported host.
- Handler tests: 429 when apply already running; correct mapping of staged flag and meta fields; unlocked gating enforced.
- (Future CI) Simulated TU success in containerized MicroOS base, marking staged then verifying post-reboot state.

## Rollout
- Land update.Manager + Gin handlers; keep UI consuming existing fields while gradually reading `meta`.
- Document new behavior in `docs/runtime/single-node-remote-milestone.md` acceptance #9 (read-only status + apply path) and cross-link this RFC.

## Implementation Notes & Status
- Pending: design approved; implementation not yet started (current branch).
