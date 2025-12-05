#!/usr/bin/env bash
set -euo pipefail

# Build a reusable MicroOS qcow2 image with gocryptfs preinstalled.
# microos-update.sh and microos-devserver.sh will reuse this image when present
# to avoid re-running transactional-update on every test run.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ART_ROOT="$ROOT_DIR/artifacts/microos-cache"
TS="$(date -u +%Y%m%d-%H%M%S)"
RUN_DIR="$ART_ROOT/$TS"
mkdir -p "$RUN_DIR"

MICROOS_IMAGE_URL=${MICROOS_IMAGE_URL:-"https://download.opensuse.org/tumbleweed/appliances/openSUSE-MicroOS.x86_64-16.0.0-ContainerHost-kvm-and-xen-Snapshot20251121.qcow2"}
MICROOS_IMAGE_PATH=${MICROOS_IMAGE_PATH:-"$ROOT_DIR/build/microos-base.qcow2"}
MICROOS_CACHE_IMAGE_PATH=${MICROOS_CACHE_IMAGE_PATH:-"$ROOT_DIR/build/microos-base-gocryptfs.qcow2"}
MICROOS_REFRESH_CACHE=${MICROOS_REFRESH_CACHE:-0}
MICROOS_CACHE_KEY_PATH=${MICROOS_CACHE_KEY_PATH:-"$ROOT_DIR/build/microos-cache-key"}
MICROOS_VM_CPUS=${MICROOS_VM_CPUS:-2}
MICROOS_VM_RAM=${MICROOS_VM_RAM:-2048}
SSH_PORT=${MICROOS_CACHE_SSH_PORT:-10422}
HEADLESS=${E2E_HEADLESS:-1}
SSH_USER=root
SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=$RUN_DIR/known_hosts -o ConnectTimeout=10"
CACHE_WAIT_FOR_SSH_SECS=${CACHE_WAIT_FOR_SSH_SECS:-300}

log() { echo "[$(date -u +%H:%M:%S)] $*"; }
log_file="$RUN_DIR/cache.log"
exec > >(tee -a "$log_file") 2>&1

require() { for c in "$@"; do command -v "$c" >/dev/null 2>&1 || { echo "missing $c"; exit 1; }; done; }
require qemu-system-x86_64 qemu-img mkisofs ssh scp curl jq

if [ "$MICROOS_REFRESH_CACHE" = "0" ] && [ -f "$MICROOS_CACHE_IMAGE_PATH" ]; then
  log "Cached image already exists at $MICROOS_CACHE_IMAGE_PATH; skipping rebuild."
  exit 0
fi

if [ ! -f "$MICROOS_IMAGE_PATH" ]; then
  log "Downloading MicroOS image..."
  mkdir -p "$(dirname "$MICROOS_IMAGE_PATH")"
  curl -L "$MICROOS_IMAGE_URL" -o "$MICROOS_IMAGE_PATH"
fi
if ! qemu-img info "$MICROOS_IMAGE_PATH" >/dev/null 2>&1; then
  log "Invalid qcow2 image (check MICROOS_IMAGE_URL)"; exit 1
fi

QCOW_WORK="$RUN_DIR/microos-prep.qcow2"
cp "$MICROOS_IMAGE_PATH" "$QCOW_WORK"

# SSH key
SSH_KEY="$MICROOS_CACHE_KEY_PATH"
mkdir -p "$(dirname "$SSH_KEY")"
if [ ! -f "$SSH_KEY" ]; then
  ssh-keygen -q -t rsa -N "" -f "$SSH_KEY" >/dev/null
fi

# Ignition (ssh key only)
IGNITION_ISO="$RUN_DIR/ignition.iso"
ISO_ROOT="$RUN_DIR/iso_root"
mkdir -p "$ISO_ROOT/ignition"
cat > "$ISO_ROOT/ignition/config.ign" <<IGN
{
  "ignition": { "version": "3.4.0" },
  "passwd": { "users": [{ "name": "$SSH_USER", "sshAuthorizedKeys": [ "$(cat ${SSH_KEY}.pub)" ] }] }
}
IGN
mkisofs -quiet -o "$IGNITION_ISO" -V ignition -J -r "$ISO_ROOT"

cleanup() {
  if [ -f "$RUN_DIR/qemu.pid" ]; then
    kill "$(cat "$RUN_DIR/qemu.pid")" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

wait_for_ssh() {
  log "Waiting for SSH on port $SSH_PORT..."
  local start
  start=$(date +%s)
  while true; do
    if ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "echo ok" >/dev/null 2>&1; then return; fi
    if [ $(( $(date +%s) - start )) -ge "$CACHE_WAIT_FOR_SSH_SECS" ]; then
      log "SSH not reachable"
      exit 1
    fi
    sleep 2
  done
}

request_reboot() {
  if ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "reboot"; then
    return
  fi
  code=$?
  if [ "$code" -ne 255 ]; then
    log "Reboot command failed (exit $code)"
    exit 1
  fi
}

ensure_gocryptfs_and_piccolod() {
  if ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "rpm -q gocryptfs piccolod >/dev/null 2>&1"; then
    return
  fi
  log "Adding Piccolo OS repository..."
  # -G disables GPG check for the repository permanently
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" \
    "zypper ar -G -f https://download.opensuse.org/repositories/home:/abhishekborar93:/piccolo-os/openSUSE_Tumbleweed/home:abhishekborar93:piccolo-os.repo" >/dev/null

  log "Installing gocryptfs and piccolo-os-support via transactional-update (requires reboot)"
  # We use -n (non-interactive) and accept keys implicitly via repo setup or zypper flags if needed,
  # but Tumbleweed might prompt for new repo keys.
  # Let's try installing with --no-recommends to keep it slim.
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" \
    "transactional-update -n pkg install --no-recommends gocryptfs piccolo-os-support" >/dev/null

  log "Rebooting to apply changes"
  request_reboot
  sleep 5
  wait_for_ssh
}

persist_cache_key() {
  log "Persisting cache SSH key to root authorized_keys"
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "mkdir -p /root/.ssh && chmod 700 /root/.ssh && printf '%s\n' '$(cat "$SSH_KEY.pub")' > /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys"
}

wait_for_shutdown() {
  if [ ! -f "$RUN_DIR/qemu.pid" ]; then return; fi
  local pid
  pid=$(cat "$RUN_DIR/qemu.pid")
  for _ in $(seq 1 60); do
    if ! kill -0 "$pid" >/dev/null 2>&1; then return; fi
    sleep 2
  done
  log "Timeout waiting for graceful shutdown; forcing stop"
  kill "$pid" >/dev/null 2>&1 || true
}

QEMU_NETDEV="user,id=net0,hostfwd=tcp:127.0.0.1:${SSH_PORT}-:22"
QEMU_DISPLAY="-display sdl"; [ "$HEADLESS" = "1" ] && QEMU_DISPLAY="-display none"
QEMU_ACCEL=""
if [ -c /dev/kvm ]; then
  QEMU_ACCEL="-cpu host -enable-kvm"
fi

log "Starting MicroOS prep VM (ssh port $SSH_PORT, cpus $MICROOS_VM_CPUS, ram ${MICROOS_VM_RAM}MB)"
qemu-system-x86_64 \
  $QEMU_ACCEL \
  -smp "$MICROOS_VM_CPUS" -m "$MICROOS_VM_RAM" \
  -drive if=virtio,file="$QCOW_WORK",format=qcow2 \
  -drive if=virtio,file="$IGNITION_ISO",format=raw \
  -netdev "$QEMU_NETDEV" -device virtio-net,netdev=net0 \
  -boot n \
  $QEMU_DISPLAY \
  -pidfile "$RUN_DIR/qemu.pid" \
  -daemonize

wait_for_ssh
ensure_gocryptfs_and_piccolod
persist_cache_key

log "Powering off VM to finalize cached image"
ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "poweroff" >/dev/null 2>&1 || true
wait_for_shutdown

log "Writing cached image to $MICROOS_CACHE_IMAGE_PATH"
mkdir -p "$(dirname "$MICROOS_CACHE_IMAGE_PATH")"
cp "$QCOW_WORK" "$MICROOS_CACHE_IMAGE_PATH"
log "Cached image ready: $MICROOS_CACHE_IMAGE_PATH"
