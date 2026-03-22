# Hardware Watchdog Reset During transactional-update dup

**Date:** 2026-03-20
**Device:** LXY MN/MN mini PC (Intel Alder Lake PCH, 8 GB RAM)
**Disk:** Generic "SATA SSD" (FW VE1R900B, 512 GB, 2.5" SATA, serial A2403SATASESL512G002)
**OS:** openSUSE MicroOS, kernel 6.19.6-2-default, systemd 259.x

---

## Summary

`transactional-update dup` causes a hard system reset on a Piccolo device. The reset leaves no trace — no kernel panic, no pstore dump, no journal error, no console output. The crash is 100% reproducible and occurs only when systemd has `/dev/watchdog0` (`intel_oc_wdt`) open. Disabling the hardware watchdog (`RuntimeWatchdogSec=0`) eliminates the crash.

The device also has a degraded SATA link with persistent CRC/handshake errors, forcing operation at 1.5 Gbps. Whether the SATA degradation is a contributing factor or a red herring is unconfirmed — a replacement SSD is needed to isolate.

---

## Timeline (2026-03-18 — 2026-03-19)

### Initial symptoms (Mar 18)

1. **00:20 UTC** — Auto-timer runs `transactional-update cleanup dup reboot`. System enters zombie state (unresponsive but power button triggers ACPI shutdown). Hung for ~4 hours until manually power-cycled.
2. **04:36 UTC** — Manual `transactional-update dup` runs. System crashes ~20 seconds after `tukit close`. Reboots automatically.
3. **07:15 UTC** — Another manual dup. Same crash at `tukit close`.
4. **07:20 UTC** — `transactional-update pkg install piccolod piccolo-os-support` (2 packages). Completes successfully. Manual reboot works.
5. **07:24 UTC** — Another dup attempt. Crashes again.

All crash reboots land on the old snapshot (#95, later #200) because `tukit close` never completes the `btrfs subvolume set-default`.

### Diagnostic log analysis

Full diagnostic log (45,043 lines) revealed **135 ATA bus errors** on `ata1` (the system disk):

```
ata1.00: exception Emask 0x10 ... interface fatal error
ata1: SError: { UnrecovData 10B8B BadCRC }
ata1: SError: { UnrecovData 10B8B Handshk }
ata1: hard resetting link
```

Errors appeared within seconds of every boot. The kernel auto-downgraded the SATA link speed from 6 Gbps to 3 Gbps multiple times.

SMART diagnostics showed the SSD itself is healthy internally:
- `UDMA_CRC_Error_Count: 0` (drive doesn't see the errors — they're on the host AHCI controller side)
- `Reallocated_Sector_Ct: 0`, `Raw_Read_Error_Rate: 0`
- Power-on hours: 925, Temperature: 42C

---

## Investigation

### Hypotheses tested

| # | Hypothesis | Test | Result |
|---|-----------|------|--------|
| 1 | SATA link errors cause crash | Force 1.5 Gbps (`libata.force=1:1.5G`) — zero ATA errors | Still crashes |
| 2 | Thermal issue | Checked `/sys/class/thermal/` — 27-48C | Normal |
| 3 | Memory pressure | `free -h` — 6.9 GB available of 7.5 GB | Normal |
| 4 | MCE / hardware error | `dmesg \| grep mce` | None |
| 5 | Kernel package specifically | Lock `kernel-default`, run dup | Still crashes |
| 6 | btrfs qgroup bug | `btrfs quota disable /` | Still crashes |
| 7 | Orphaned snapshot accumulation | `snapper delete` all failed snapshots | Still crashes |
| 8 | PID 1 blocked on I/O (D state) | Monitor `/proc/1/status` every 100ms, log to ext4 | PID 1 always in S state (`do_epoll_wait`), never D |
| 9 | Hardware watchdog timeout | Set `RuntimeWatchdogSec=300` (5 min) | Still crashes |
| 10 | iTCO_wdt (second watchdog) interference | Blacklist `iTCO_wdt` | Still crashes |
| 11 | intel_oc_wdt driver loaded (without systemd using it) | `RuntimeWatchdogSec=0`, driver loaded | Succeeds |
| 12 | systemd opens /dev/watchdog0 | `RuntimeWatchdogSec=30` (default) | Crashes |

### Key finding: isolation matrix

| intel_oc_wdt loaded | systemd opens /dev/watchdog0 | dup result |
|---|---|---|
| Yes | No (`RuntimeWatchdogSec=0`) | Success |
| Yes | Yes (`RuntimeWatchdogSec=30`) | **Crash** |
| Yes | Yes (`RuntimeWatchdogSec=300`) | **Crash** |

The crash requires systemd to have `/dev/watchdog0` open and actively pinging via `WDIOC_KEEPALIVE` ioctl. It is **not** a watchdog timeout — a 5-minute timeout still crashes within seconds.

### PID 1 state at crash time

A monitoring script running on ext4 (non-btrfs, survives crash) sampled `/proc/1/status` and `/proc/1/wchan` every 100ms:

```
18:44:52.249 State:S (sleeping) wchan=do_epoll_wait
18:44:52.360 State:S (sleeping) wchan=do_epoll_wait
...
18:44:55.608 State:S (sleeping) wchan=do_epoll_wait
[crash — log ends]
```

352 samples over 38 seconds, no gaps, PID 1 in `S` state (sleeping in `epoll_wait`) the entire time. Never entered `D` (disk sleep). Only one anomaly: a single sample with `wchan=0` (momentarily running between syscalls — normal).

### Crash point variability

The crash does not occur at a fixed point. Observed crash locations during RPM scriptlet execution:
- `container-selinux` posttrans
- `fcoe-utils` posttrans
- `kbd` posttrans
- 3-10 seconds after `tukit close` completes

This variability suggests a timing-sensitive hardware condition rather than a specific software operation triggering the crash.

### Console output

With a monitor attached, the screen shows MicroOS startup logs, then goes completely black at crash time. No kernel panic text, no oops dump. Consistent with a hardware reset (CPU RESET# asserted by the PCH watchdog timer block).

---

## Root cause (current understanding)

The `intel_oc_wdt` watchdog timer on this Intel Alder Lake PCH fires a spurious hardware reset when systemd is actively pinging `/dev/watchdog0` and the system is under sustained I/O from `transactional-update dup`. This is not a watchdog timeout — the reset occurs regardless of the configured timeout value.

The mechanism is not fully understood. Possible explanations:
1. **PCH-internal interaction** between the watchdog timer block and the AHCI SATA controller, both on the same die. The SATA controller has link integrity issues (CRC/handshake errors) that may put the PCH in an abnormal state.
2. **intel_oc_wdt driver bug** where the `WDIOC_KEEPALIVE` ioctl has a side effect under certain PCH conditions.
3. **Firmware/BIOS bug** in the LXY MN platform's watchdog implementation.

### Why small installs succeed

`transactional-update pkg install` with 2 small packages generates minimal I/O (~48 KB snapshot delta). The dup generates ~225 MB of package writes plus btrfs COW metadata, RPM scriptlet execution (dracut, semodule, grub2-mkconfig), and snapshot operations. The sustained I/O from dup is what triggers the condition — though the exact interaction with the watchdog is unclear.

---

## Impact

- **Piccolo OS auto-updates are broken** on this specific device.
- The device cannot receive OS-level security patches via the normal `transactional-update dup` path.
- Small targeted package installs (`transactional-update pkg install`) still work.

---

## Workaround

Disable the hardware watchdog so systemd never opens `/dev/watchdog0`:

```bash
mount -o remount,rw /
mkdir -p /etc/systemd/system.conf.d/
cat > /etc/systemd/system.conf.d/zzz-no-watchdog.conf << 'EOF'
[Manager]
RuntimeWatchdogSec=0
EOF
```

The filename must sort lexicographically after `piccolo.conf` (which sets `RuntimeWatchdogSec=30` in `/usr/lib/systemd/system.conf.d/`).

---

## Open questions

1. **Does replacing the SSD fix the watchdog crash?** The SATA link degradation may be putting the PCH in an abnormal state. A healthy SSD at 6 Gbps may eliminate the interaction that triggers the spurious reset.
2. **Is this reproducible on other Piccolo devices?** If the bug is specific to this PCH stepping or BIOS version, it may not be a systemic product issue.
3. **Should Piccolo OS use `iTCO_wdt` instead of `intel_oc_wdt`?** iTCO_wdt is the standard Intel PCH watchdog. intel_oc_wdt is the over-clocking watchdog. They are separate hardware blocks on the PCH.
4. **Should `transactional-update` (or a Piccolo wrapper) temporarily suspend the hardware watchdog during updates?** This would add resilience against watchdog-related failures during heavy I/O, regardless of root cause.

---

## Remediation status

| Item | Status |
|------|--------|
| Workaround (`RuntimeWatchdogSec=0`) applied on affected device | Done |
| SATA link forced to 1.5 Gbps (`libata.force=1:1.5G`) | Done |
| SSD replacement | Pending |
| Re-test watchdog with healthy SSD | Pending |
| Evaluate `intel_oc_wdt` vs `iTCO_wdt` for product | Open |
| Evaluate watchdog suspension during transactional-update | Open |
