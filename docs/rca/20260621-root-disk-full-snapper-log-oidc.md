# RCA: Root disk full from snapper log growth caused OIDC failures

**Date:** 2026-06-21
**Device:** Piccolo device serving `drawguess.fun`, `d111.abhishekborar.com`, and `insights-piclu.d111.abhishekborar.com`
**Primary symptom:** Proxy OIDC callback failed with `server_error` and `disk I/O error (778)`

---

## Summary

The device became unhealthy because the root btrfs filesystem (`/dev/sda3`,
20 GiB) reached 100% usage. The most visible symptom was OIDC login failure for
the `piclu` proxy listener:

```text
https://insights-piclu.d111.abhishekborar.com/__piccolod_oidc/callback?error=server_error&error_description=disk+I%2FO+error+%28778%29
```

The initial disk investigation appeared to implicate DrawGuess because plain
`du -sh /piccolo-core/*` reported `17G` under `/piccolo-core/mounts`, and
`14G` under `/piccolo-core/mounts/app-drawguess`. That was a false lead: plain
`du` crossed into mounted app volumes. `df` and `du -x` confirmed DrawGuess
data was on its intended ext4 application volume and was not consuming root
space:

```text
/dev/mapper/piccolo-vol-app-drawguess ext4 40G 14G 26G 36% /piccolo-core/mounts/app-drawguess
du -xsh /piccolo-core/mounts -> 0
```

The real immediate root filler was:

```text
7971930112 /var/log/snapper.log-20260621
```

Once the 8 GiB snapper log was truncated and archived journals were vacuumed,
root recovered from 100% full to about 57% used:

```text
/dev/sda3 20G 12G 8.4G 57% /
```

The control-plane volume then mounted read-write again:

```text
/dev/mapper/piccolo-loop-control-plane ext4 ... /piccolo-core/mounts/control-plane rw
```

The remaining difference between `df` (~11-12 GiB used) and visible active
filesystem `du` (~1.8 GiB root, ~501 MiB `/var`) is consistent with btrfs
snapshots pinning historical extents, including the old large snapper log.

---

## Impact

- OIDC login for `insights-piclu.d111.abhishekborar.com` failed.
- Piccolod could not reliably persist control-plane state while the root
  filesystem was full.
- The control-plane ext4 filesystem, mounted through a LUKS loop file on root,
  hit I/O errors and remounted read-only during the incident.
- NetworkManager, clock epoch persistence, piccolod catalog cache writes, and
  piclu generation GC timestamp writes all showed disk-full or read-only
  symptoms.
- DrawGuess application data remained intact and correctly mounted on its
  application volume.

---

## Timeline

### 2026-06-21 03:26 IST

The first retained diagnostic evidence of root exhaustion appeared in
NetworkManager:

```text
NetworkManager: error saving lease ... write() failed: No space left on device
```

This shows the root filesystem was already full hours before the OIDC failure
was noticed.

### 2026-06-21 04:18-09:48 IST

Other root writes continued failing:

```text
clock-epoch.sh: echo: write error: No space left on device
NetworkManager: error saving lease ... No space left on device
```

Snapper cleanup and timeline units repeatedly failed during the same window.
This matched a prior known failure mode where snapper operations and Piccolo's
update status polling can keep snapper's config locked.

### 2026-06-21 08:12 IST

Piccolod attempted to freeze the control-plane mount and received an I/O error:

```text
fsfreeze: /piccolo-core/mounts/control-plane: freeze failed: Input/output error
```

### 2026-06-21 08:14 IST

The control-plane ext4 filesystem aborted its journal and remounted read-only:

```text
EXT4-fs error (device dm-116): ext4_journal_check_start:86: comm piccolod: Detected aborted journal
Buffer I/O error on dev dm-116, logical block 1, lost sync page write
EXT4-fs (dm-116): I/O error while writing superblock
EXT4-fs (dm-116): Remounting filesystem read-only
```

At the same time, OIDC authorization failed:

```text
ERROR auth request oidc_error.parent="disk I/O error (778)" oidc_error.description="disk I/O error (778)" oidc_error.type=server_error
WARN: proxy OIDC callback error: server_error - disk I/O error (778)
```

### 2026-06-21 10:05 IST

The user opened a host terminal through the portal. Piccolod still served some
UI/API traffic, but control-plane writes and cache writes were unhealthy:

```text
WARN: failed to write catalog cache: write temp file: write /piccolo-core/cache/catalog/.index.yaml.tmp...: no space left on device
WARN: GC piclu: persist GC timestamp ... read-only file system
```

### Triage

Initial app-volume suspicion:

```text
du -sh /piccolo-core/*
17G /piccolo-core/mounts

du -sh /piccolo-core/mounts/app-drawguess/*
14G /piccolo-core/mounts/app-drawguess/data
```

Mount-aware checks disproved that suspicion:

```text
df -hT / /piccolo-core /piccolo-core/mounts/app-drawguess /piccolo-core/mounts/control-plane
/dev/sda3                              btrfs 20G 20G 0 100% /
/dev/sda3                              btrfs 20G 20G 0 100% /piccolo-core
/dev/mapper/piccolo-vol-app-drawguess  ext4  40G 14G 26G 36% /piccolo-core/mounts/app-drawguess
/dev/mapper/piccolo-loop-control-plane ext4  219M 545K 212M 1% /piccolo-core/mounts/control-plane

du -xsh /piccolo-core/*
0 /piccolo-core/mounts
```

Root-visible usage then pointed at `/var/log`:

```text
du -xhd1 /var | sort -h
9.6G /var/log
9.8G /var

journalctl --disk-usage
Archived and active journals take up 2G in the file system.

find /var/log -xdev -type f -printf '%s %p\n' | sort -n | tail -30
7971930112 /var/log/snapper.log-20260621
```

### Immediate mitigation

The user preserved the tail of the large snapper log, truncated it, and vacuumed
journals:

```bash
tail -n 200 /var/log/snapper.log-20260621 > /run/snapper.log.tail
truncate -s 0 /var/log/snapper.log-20260621
journalctl --vacuum-size=200M
sync
```

This recovered root free space:

```text
Filesystem      Size  Used Avail Use% Mounted on
/dev/sda3        20G   12G  8.4G  57% /
```

After reboot, the control-plane mount was healthy:

```text
findmnt -T /piccolo-core/mounts/control-plane
/piccolo-core/mounts/control-plane /dev/mapper/piccolo-loop-control-plane ext4 rw,relatime,seclabel
```

Live active log usage was also healthy:

```text
du -xhd1 / /var /var/log | sort -h
331M /var/log
501M /var
1.8G /

journalctl --disk-usage
Archived and active journals take up 174.7M in the file system.
```

### Post-recovery snapshot evidence

The active root was snapshot `943`:

```text
943* | single | Sat 20 Jun 2026 12:30:26 AM IST | number | Snapshot Update of #876
```

Snapshot `948` existed with an in-progress transactional-update marker:

```text
948 | single | Sun 21 Jun 2026 10:45:54 AM IST | number | Snapshot Update of #876 | transactional-update-in-progress=yes
```

`/.snapshots` reported very large logical usage:

```text
du -sh /.snapshots
890G /.snapshots
```

On btrfs this is logical/reflink double-counting across snapshots, not physical
disk usage. The physical source of truth remained `df` / `btrfs filesystem
usage`, which showed about 11-12 GiB used. The `df` vs live `du` mismatch is
therefore likely snapshot-pinned historical extents from the old large log and
other snapshot contents.

---

## Root Cause

### Immediate root cause

The root btrfs filesystem filled because `/var/log/snapper.log-20260621` grew
to about 8 GiB, with archived systemd journals contributing another roughly
2 GiB before vacuuming.

Once root reached 100%, writes through the control-plane LUKS loop file began
failing. The control-plane filesystem then aborted its ext4 journal and
remounted read-only. OIDC authorization requires writing auth/request state to
the control-plane store, so it surfaced as:

```text
disk I/O error (778)
```

### Underlying system cause

Snapper cleanup was already unhealthy. The diagnostic log showed repeated
snapper cleanup/timeline failures, and live `snapper cleanup number` returned:

```text
Config is locked.
```

This aligns with the known snapper/config-lock issue being worked in a sibling
piccolod worktree, where Piccolo's update polling and snapper operations can
interfere with cleanup. In this incident, the cleanup failure mode also had a
large operational side effect: snapper logging itself consumed enough root
space to take the device down.

### False lead

DrawGuess looked suspicious because its app data appeared under
`/piccolo-core/mounts` in plain `du` output. That was expected path placement,
not physical root usage. Current app data is mounted as a per-app ext4 volume:

```text
/piccolo-core/mounts/app-drawguess -> /dev/mapper/piccolo-vol-app-drawguess
```

The correct root-only disk command in this situation is:

```bash
du -xsh /piccolo-core/*
```

or, better, compare:

```bash
df -hT / /piccolo-core /piccolo-core/mounts/app-drawguess /piccolo-core/mounts/control-plane
findmnt -T /piccolo-core/mounts/app-drawguess
```

---

## Contributing Factors

1. **Unbounded or insufficiently rotated snapper log file.** A single
   `/var/log/snapper.log-YYYYMMDD` file grew to about 8 GiB on a 20 GiB root.
2. **Snapper cleanup/config locking.** Cleanup could not drain old snapshots
   and returned `Config is locked`.
3. **Piccolo update status polling interacts with snapper.** This is a known
   issue under active work in a sibling checkout; during this incident it also
   contributed to repeated snapper failures and excessive logging.
4. **Small root filesystem margin.** The root btrfs filesystem is only 20 GiB.
   An 8 GiB log plus journals was enough to exhaust it.
5. **Control-plane loop file lives on root.** Even though the mounted
   control-plane ext4 filesystem had free space internally, its backing LUKS
   loop file depends on root filesystem writes.
6. **Plain `du` crossed mount boundaries.** The first view made app data look
   like core/root usage until `df`, `findmnt`, and `du -x` separated mount
   accounting.

---

## What Worked

- The app data volume model worked correctly. DrawGuess data remained isolated
  on `/dev/mapper/piccolo-vol-app-drawguess`.
- The portal remained usable enough to open a host terminal and recover space.
- `du -x`, `df -T`, and `findmnt` quickly distinguished root usage from mounted
  application data.
- Truncating the large snapper log and vacuuming journals recovered enough
  space to stabilize the host.
- Reboot restored the control-plane mount to read-write.

---

## What Did Not Work

- Snapper cleanup could not run because the config was locked.
- Snapper logging was allowed to consume most of root.
- Piccolod did not prevent or warn early enough about root exhaustion affecting
  the control plane.
- The OIDC failure surfaced to the user as a low-level SQLite/Zitadel
  `disk I/O error (778)` rather than an operator-facing "root filesystem full"
  diagnosis.
- The first-level storage view was easy to misread because app volumes are
  path-mounted under `/piccolo-core/mounts`.

---

## Immediate Remediation

Completed during incident response:

1. Preserved last 200 lines of the large snapper log in `/run/snapper.log.tail`.
2. Truncated `/var/log/snapper.log-20260621`.
3. Vacuumed systemd journals to `200M`.
4. Confirmed root recovered to about `8.4G` free.
5. Rebooted the machine.
6. Confirmed:
   - `/piccolo-core/mounts/control-plane` mounted read-write.
   - `/var/log` shrank to about `331M`.
   - journald shrank to about `174.7M`.
   - DrawGuess remained mounted on its app volume.

---

## Preventive Remediation

1. **Bound snapper log growth.**
   - Add logrotate or an equivalent cap for `/var/log/snapper.log-*`.
   - Treat a snapper log above a low threshold as an urgent support signal.

2. **Fix snapper config-lock loop.**
   - Continue the sibling-worktree work to prevent Piccolo update polling from
     overlapping with snapper cleanup/list operations in a way that keeps
     cleanup locked out.
   - Add backoff/single-flight around snapper status probes.

3. **Make root disk pressure visible and actionable.**
   - Alert before `/` reaches critical usage.
   - Surface root filesystem pressure in the portal with attribution to the
     largest active paths and mounted-subvolume caveats.

4. **Protect the control-plane loop backing store.**
   - Treat low root free space as control-plane degraded, even when the
     mounted control-plane ext4 filesystem reports internal free space.
   - Consider a guard that pauses nonessential root writes and reports a clear
     degraded state before the control-plane loop can hit I/O errors.

5. **Improve diagnostic guidance.**
   - Include mount-aware disk commands in support diagnostics:

```bash
df -hT / /piccolo-core /piccolo-core/mounts/*
du -xhd1 / /var /var/log /piccolo-core
findmnt -R / -o TARGET,SOURCE,FSTYPE,USED,AVAIL
journalctl --disk-usage
```

6. **Improve OIDC error attribution.**
   - When auth request persistence fails with SQLite disk I/O errors, correlate
     with `statfs("/")`, control-plane mount options, and recent kernel
     read-only remount messages so the operator sees a storage diagnosis.

---

## Remediation Status

| Item | Status |
|------|--------|
| Immediate free-space recovery | Done |
| Control-plane remounted read-write after reboot | Done |
| DrawGuess app data verified off-root | Done |
| Snapper config-lock root cause | Open, active sibling-worktree work |
| Snapper log growth cap/logrotate | Open |
| Root disk pressure alerting and attribution | Open |
| OIDC disk I/O error attribution | Open |
| Snapshot cleanup after config-lock fix | Deferred until snapper/piccolod fix lands |
