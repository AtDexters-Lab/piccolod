# Local MicroOS Update E2E (developer-only)

This guide runs a real transactional-update apply + rollback cycle in a MicroOS VM. It is **not** wired to CI and is opt-in for developers.

## Prereqs
- Host binaries: `qemu-system-x86_64`, `qemu-img`, `mkisofs`/`genisoimage`, `ssh`, `scp`, `curl`, `jq`.
- KVM enabled for reasonable speed (tcg fallback works but is slow).
- Internet access to fetch the MicroOS qcow2 image (or provide your own via env vars).

## Script
`tools/e2e/microos-update.sh`

What it does:
1) Downloads (or reuses) a MicroOS qcow2 (default Tumbleweed Base).
2) Seeds an ignition ISO that injects your SSH key for the `root` user (no cloud-init password).
3) Launches QEMU with port-forwarded SSH (`localhost:10022`) and API.
4) Installs `gocryptfs` via `transactional-update` (once per QCOW) and reboots so auth/crypto can work.
5) Copies your built `piccolod` into the VM and starts it via `systemd-run`.
6) Bootstraps auth/crypto, then drives the update API: baseline status → apply → wait for transactional-update to finish → reboot → status → rollback → wait → reboot → status.
7) Collects logs under `artifacts/e2e-microos/<timestamp>/` (piccolod log, transactional-update journal, JSON summary).

## Usage
```bash
# From repo root
./tools/e2e/microos-update.sh
```

Environment knobs:
- `MICROOS_IMAGE_URL` (default Tumbleweed Base qcow2)
- `MICROOS_IMAGE_PATH` (cache path, default `build/microos-base.qcow2`)
- `PICCOLOD_BIN` (path to piccolod binary; default `./piccolod` and built if missing)
- `MICROOS_SSH_PORT` (default `10022`)
- `MICROOS_VM_CPUS` (default `2`), `MICROOS_VM_RAM` (default `2048` MB)
- `E2E_HEADLESS=0` to request a QEMU window; set `E2E_DAEMONIZE=` (empty) if you also want the process in the foreground.
- `MICROOS_SSH_KEY` to use an existing key; otherwise the script generates an ephemeral key under `artifacts/e2e-microos/<run>/ephemeral_id_rsa`.
- `WAIT_FOR_SSH_SECS` to wait for SSH to come up (default `300` seconds).
- `TU_WAIT_TIMEOUT` seconds to wait for transactional-update apply/rollback before failing (default `1800`).

Expected duration: ~10–20 minutes with KVM; slower without.

## Outputs
`artifacts/e2e-microos/<timestamp>/` contains:
- `summary.json` with baseline/post-apply/post-rollback `/updates/os` payloads
- `transactional-update.log` (journal tail)
- `piccolod.log` (daemon stdout/stderr from systemd-run)

## Notes
- The script currently assumes MicroOS x86_64 Base image; adjust URL/path for other flavors.
- It reboots the VM twice; if SSH doesn’t return within the wait window, the run fails.
- Use only on a trusted host network; the VM runs root SSH with a known password, bound to localhost.
