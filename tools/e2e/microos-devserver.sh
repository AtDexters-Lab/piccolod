#!/usr/bin/env bash
set -euo pipefail

# MicroOS piccolod dev server harness for UI workstations.
# - Boots MicroOS qcow2 in QEMU and forwards piccolod API to the host.
# - Ensures gocryptfs is present (uses cached image when available).
# - Sets up crypto + admin defaults for quick UI login.
# - Leaves the VM running until you Ctrl+C; collects basic logs on exit.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ART_ROOT="$ROOT_DIR/artifacts/microos-devserver"
TS="$(date -u +%Y%m%d-%H%M%S)"
RUN_DIR="$ART_ROOT/$TS"
mkdir -p "$RUN_DIR"

MICROOS_IMAGE_URL=${MICROOS_IMAGE_URL:-"https://download.opensuse.org/tumbleweed/appliances/openSUSE-MicroOS.x86_64-16.0.0-ContainerHost-kvm-and-xen-Snapshot20251121.qcow2"}
MICROOS_IMAGE_PATH=${MICROOS_IMAGE_PATH:-"$ROOT_DIR/build/microos-base.qcow2"}
MICROOS_CACHE_IMAGE_PATH=${MICROOS_CACHE_IMAGE_PATH:-"$ROOT_DIR/build/microos-base-gocryptfs.qcow2"}
MICROOS_USE_CACHE=${MICROOS_USE_CACHE:-1}
MICROOS_REFRESH_CACHE=${MICROOS_REFRESH_CACHE:-0}
MICROOS_CACHE_KEY_PATH=${MICROOS_CACHE_KEY_PATH:-"$ROOT_DIR/build/microos-cache-key"}
MICROOS_CACHE_SSH_PORT=${MICROOS_CACHE_SSH_PORT:-10422}
PICCOLOD_BIN=${PICCOLOD_BIN:-"$ROOT_DIR/piccolod"}
SSH_PORT=${MICROOS_SSH_PORT:-10032}
API_PORT=${MICROOS_API_PORT:-8080}
HOST_API_PORT=${MICROOS_HOST_API_PORT:-18080}
MICROOS_VM_CPUS=${MICROOS_VM_CPUS:-2}
MICROOS_VM_RAM=${MICROOS_VM_RAM:-2048}
HEADLESS=${E2E_HEADLESS:-1}
E2E_DAEMONIZE=${E2E_DAEMONIZE:--daemonize}
SSH_USER=root
ADMIN_USERNAME=${ADMIN_USERNAME:-"admin"}
ADMIN_PASSWORD=${ADMIN_PASSWORD:-"Passw0rd!"}
IGNITION_ISO="$RUN_DIR/ignition.iso"
QCOW_WORK="$RUN_DIR/microos.qcow2"
SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=$RUN_DIR/known_hosts -o ConnectTimeout=10"
TU_WAIT_TIMEOUT=${TU_WAIT_TIMEOUT:-1800}
WAIT_FOR_SSH_SECS=${WAIT_FOR_SSH_SECS:-300}

log() { echo "[$(date -u +%H:%M:%S)] $*"; }
log_file="$RUN_DIR/devserver.log"
exec > >(tee -a "$log_file") 2>&1

require() { for c in "$@"; do command -v "$c" >/dev/null 2>&1 || { echo "missing $c"; exit 1; }; done; }
require qemu-system-x86_64 qemu-img mkisofs ssh scp curl jq

if [ ! -f "$MICROOS_IMAGE_PATH" ]; then
  log "Downloading MicroOS image..."
  mkdir -p "$(dirname "$MICROOS_IMAGE_PATH")"
  curl -L "$MICROOS_IMAGE_URL" -o "$MICROOS_IMAGE_PATH"
fi
if ! qemu-img info "$MICROOS_IMAGE_PATH" >/dev/null 2>&1; then
  log "Invalid qcow2 image (check MICROOS_IMAGE_URL)"; exit 1
fi
if [ "$MICROOS_REFRESH_CACHE" = "1" ] && [ -f "$MICROOS_CACHE_IMAGE_PATH" ]; then
  log "MICROOS_REFRESH_CACHE=1, removing cached image at $MICROOS_CACHE_IMAGE_PATH"
  rm -f "$MICROOS_CACHE_IMAGE_PATH"
fi
if [ "$MICROOS_USE_CACHE" = "1" ] && [ ! -f "$MICROOS_CACHE_IMAGE_PATH" ]; then
  log "Cached image missing; building gocryptfs-enabled image first..."
  MICROOS_IMAGE_URL="$MICROOS_IMAGE_URL" \
  MICROOS_IMAGE_PATH="$MICROOS_IMAGE_PATH" \
  MICROOS_CACHE_IMAGE_PATH="$MICROOS_CACHE_IMAGE_PATH" \
  MICROOS_CACHE_KEY_PATH="$MICROOS_CACHE_KEY_PATH" \
  MICROOS_REFRESH_CACHE="$MICROOS_REFRESH_CACHE" \
  MICROOS_VM_CPUS="$MICROOS_VM_CPUS" \
  MICROOS_VM_RAM="$MICROOS_VM_RAM" \
  MICROOS_CACHE_SSH_PORT="$MICROOS_CACHE_SSH_PORT" \
  E2E_HEADLESS="$HEADLESS" \
  "$ROOT_DIR/tools/e2e/microos-cache-gocryptfs.sh"
fi

SOURCE_IMAGE="$MICROOS_IMAGE_PATH"
if [ "$MICROOS_USE_CACHE" = "1" ] && [ -f "$MICROOS_CACHE_IMAGE_PATH" ]; then
  log "Using cached MicroOS image with gocryptfs: $MICROOS_CACHE_IMAGE_PATH"
  SOURCE_IMAGE="$MICROOS_CACHE_IMAGE_PATH"
fi
cp "$SOURCE_IMAGE" "$QCOW_WORK"

# SSH key
if [ -n "${MICROOS_SSH_KEY:-}" ]; then
  SSH_KEY="$MICROOS_SSH_KEY"
elif [ "$MICROOS_USE_CACHE" = "1" ] && [ -f "$MICROOS_CACHE_KEY_PATH" ]; then
  SSH_KEY="$MICROOS_CACHE_KEY_PATH"
else
  SSH_KEY="$RUN_DIR/ephemeral_id_rsa"
  ssh-keygen -q -t rsa -N "" -f "$SSH_KEY" >/dev/null
fi
if [ "$SOURCE_IMAGE" = "$MICROOS_CACHE_IMAGE_PATH" ] && [ "$SSH_KEY" = "$RUN_DIR/ephemeral_id_rsa" ]; then
  log "Cached image in use but cache key not found. Set MICROOS_SSH_KEY to the cache key or rebuild cache with MICROOS_REFRESH_CACHE=1."
  exit 1
fi

# Ignition (ssh key only)
ISO_ROOT="$RUN_DIR/iso_root"
mkdir -p "$ISO_ROOT/ignition"
cat > "$ISO_ROOT/ignition/config.ign" <<IGN
{
  "ignition": { "version": "3.4.0" },
  "passwd": { "users": [{ "name": "$SSH_USER", "sshAuthorizedKeys": [ "$(cat ${SSH_KEY}.pub)" ] }] }
}
IGN
mkisofs -quiet -o "$IGNITION_ISO" -V ignition -J -r "$ISO_ROOT"

QEMU_NETDEV="user,id=net0,hostfwd=tcp:127.0.0.1:${SSH_PORT}-:22,hostfwd=tcp:127.0.0.1:${HOST_API_PORT}-:${API_PORT}"
QEMU_DISPLAY="-display sdl"; [ "$HEADLESS" = "1" ] && QEMU_DISPLAY="-display none"
QEMU_ACCEL=""
if [ -c /dev/kvm ]; then
  QEMU_ACCEL="-cpu host -enable-kvm"
fi

log "Starting devserver VM (ssh $SSH_PORT, api $HOST_API_PORT -> $API_PORT, cpus $MICROOS_VM_CPUS, ram ${MICROOS_VM_RAM}MB)"
qemu-system-x86_64 \
  $QEMU_ACCEL \
  -smp "$MICROOS_VM_CPUS" -m "$MICROOS_VM_RAM" \
  -drive if=virtio,file="$QCOW_WORK",format=qcow2 \
  -drive if=virtio,file="$IGNITION_ISO",format=raw \
  -netdev "$QEMU_NETDEV" -device virtio-net,netdev=net0 \
  -boot n \
  $QEMU_DISPLAY \
  -pidfile "$RUN_DIR/qemu.pid" \
  $E2E_DAEMONIZE

cleanup() {
  collect_logs
  if [ -f "$RUN_DIR/qemu.pid" ]; then kill "$(cat "$RUN_DIR/qemu.pid")" >/dev/null 2>&1 || true; fi
}
collect_logs() {
  set +e
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "journalctl -u piccolod -n 200 --no-pager" > "$RUN_DIR/piccolod.log" 2>/dev/null || true
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "journalctl -t transactional-update -n 200 --no-pager" > "$RUN_DIR/transactional-update.log" 2>/dev/null || true
  set -e
}
trap cleanup EXIT

wait_for_ssh() {
  log "Waiting for SSH..."
  local start
  start=$(date +%s)
  while true; do
    if ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "echo ok" >/dev/null 2>&1; then return; fi
    if [ $(( $(date +%s) - start )) -ge "$WAIT_FOR_SSH_SECS" ]; then
      log "SSH not reachable"
      exit 1
    fi
    sleep 2
  done
}

install_piccolod() {
  if [ ! -x "$PICCOLOD_BIN" ]; then
    log "Building piccolod binary"
    (cd "$ROOT_DIR" && go build ./cmd/piccolod)
  fi
  scp -i "$SSH_KEY" $SSH_OPTS -P "$SSH_PORT" "$PICCOLOD_BIN" "$SSH_USER@127.0.0.1:/tmp/piccolod" >/dev/null
  # Install to /usr/local/bin (writable) since /usr/bin is read-only on MicroOS
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "install -m 0755 /tmp/piccolod /usr/local/bin/piccolod && rm -f /tmp/piccolod && (command -v restorecon >/dev/null && restorecon /usr/local/bin/piccolod || true)" >/dev/null
}

start_piccolod() {
  # Override the piccolod service to use our port and our custom binary in /usr/local/bin
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "mkdir -p /etc/systemd/system/piccolod.service.d && printf '[Service]\nEnvironment=PORT=${API_PORT}\nExecStart=\nExecStart=/usr/local/bin/piccolod\n' > /etc/systemd/system/piccolod.service.d/devserver.conf && systemctl daemon-reload && systemctl restart piccolod" >/dev/null
}

wait_for_api() {
  log "Waiting for piccolod API..."
  for _ in $(seq 1 30); do
    if curl -sf "http://127.0.0.1:${HOST_API_PORT}/api/v1/health/live" >/dev/null 2>&1; then return; fi
    if ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "curl -sf http://127.0.0.1:${API_PORT}/api/v1/health/live" >/dev/null 2>&1; then return; fi
    sleep 2
  done
  log "piccolod API not responding"; exit 1
}

configure_repos() {
  # Optional: Use local mirror for speed
  LOCAL_MIRROR=${LOCAL_OSS_MIRROR:-"http://192.168.0.100:8888/oss/"}
  
  if [ -z "$LOCAL_MIRROR" ]; then
    return
  fi

  # Verify connectivity from the VM before messing with repos
  if ! ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" \
       "curl --head --fail --silent --max-time 2 '$LOCAL_MIRROR'" >/dev/null 2>&1; then
     log "Local mirror $LOCAL_MIRROR not reachable from VM; keeping default repositories."
     return
  fi

  # Check if already configured to avoid re-running
  if ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "zypper lr local-oss >/dev/null 2>&1"; then
    return
  fi

  log "Configuring local OSS mirror: $LOCAL_MIRROR"
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" \
    "zypper mr -d openSUSE-Tumbleweed-Oss repo-oss repo-non-oss && zypper ar -G -f '$LOCAL_MIRROR' local-oss" || true
}

ensure_gocryptfs() {
  if ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "rpm -q gocryptfs piccolod >/dev/null 2>&1"; then
    return
  fi
  log "Adding Piccolo OS repository..."
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" \
    "zypper ar -G -f https://download.opensuse.org/repositories/home:/abhishekborar93:/piccolo-os/openSUSE_Tumbleweed/home:abhishekborar93:piccolo-os.repo" >/dev/null

  log "Installing gocryptfs and piccolod via transactional-update (requires reboot)"
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "transactional-update -n pkg install --no-recommends --no-gpg-checks gocryptfs piccolod" >/dev/null

  log "Rebooting to apply changes"
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "reboot" >/dev/null || true
  sleep 5
  wait_for_ssh
}

api_curl() { curl -sf -b "$RUN_DIR/cookies" -c "$RUN_DIR/cookies" -H "X-CSRF-Token: ${CSRF:-}" "$@"; }
api_post() { curl -sf -X POST -b "$RUN_DIR/cookies" -c "$RUN_DIR/cookies" -H "X-CSRF-Token: ${CSRF:-}" "$@"; }

api_setup_admin() {
  curl -sf -X POST -H 'Content-Type: application/json' -d "{\"password\":\"$ADMIN_PASSWORD\"}" "http://127.0.0.1:${HOST_API_PORT}/api/v1/auth/setup" -b "$RUN_DIR/cookies" -c "$RUN_DIR/cookies" >/dev/null || true
}
api_login() {
  curl -sf -X POST -H 'Content-Type: application/json' -d "{\"username\":\"$ADMIN_USERNAME\",\"password\":\"$ADMIN_PASSWORD\"}" "http://127.0.0.1:${HOST_API_PORT}/api/v1/auth/login" -b "$RUN_DIR/cookies" -c "$RUN_DIR/cookies" >/dev/null || return 1
}
fetch_csrf() {
  CSRF=$(curl -sf -b "$RUN_DIR/cookies" -c "$RUN_DIR/cookies" "http://127.0.0.1:${HOST_API_PORT}/api/v1/auth/csrf" | jq -r '.token // .csrf // .csrftoken')
  if [ -z "$CSRF" ] || [ "$CSRF" = "null" ]; then log "Failed to fetch CSRF"; exit 1; fi
}
api_crypto_setup() {
  curl -sf -X POST -H 'Content-Type: application/json' -d "{\"password\":\"$ADMIN_PASSWORD\"}" "http://127.0.0.1:${HOST_API_PORT}/api/v1/crypto/setup" -b "$RUN_DIR/cookies" -c "$RUN_DIR/cookies" >/dev/null || true
}
api_crypto_unlock() {
  curl -sf -X POST -H 'Content-Type: application/json' -d "{\"password\":\"$ADMIN_PASSWORD\"}" "http://127.0.0.1:${HOST_API_PORT}/api/v1/crypto/unlock" -b "$RUN_DIR/cookies" -c "$RUN_DIR/cookies" >/dev/null
}
require_login() {
  for _ in 1 2; do
    if api_login; then return; fi
    sleep 1
  done
  log "Login failed after retries"
  exit 1
}

wait_for_ssh
ensure_gocryptfs
configure_repos
install_piccolod
start_piccolod
wait_for_api

# Auth bootstrap for UI
api_crypto_setup
api_crypto_unlock
api_setup_admin
require_login
fetch_csrf

BASE_STATUS=$(api_curl "http://127.0.0.1:${HOST_API_PORT}/api/v1/updates/os")
log "Dev server ready."
log "API base: http://127.0.0.1:${HOST_API_PORT}"
log "Login: ${ADMIN_USERNAME} / ${ADMIN_PASSWORD}"
log "Initial status: $BASE_STATUS"
log "SSH: ssh -i ${SSH_KEY} -p ${SSH_PORT} ${SSH_USER}@127.0.0.1"
log "Press Ctrl+C to stop the VM."

tail -f /dev/null
