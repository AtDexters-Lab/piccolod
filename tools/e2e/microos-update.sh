#!/usr/bin/env bash
set -euo pipefail

# Local MicroOS update E2E harness (API-driven)
# - Boots MicroOS qcow2 in QEMU
# - Sets up admin + crypto via API
# - Calls /updates/os/apply and /updates/os/rollback via API (with CSRF/cookie)
# - Reboots between stages, restarts piccolod, and collects logs + summary
# Prereqs: qemu-system-x86_64, qemu-img, mkisofs/genisoimage, ssh, scp, curl, jq

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ART_ROOT="$ROOT_DIR/artifacts/e2e-microos"
TS="$(date -u +%Y%m%d-%H%M%S)"
RUN_DIR="$ART_ROOT/$TS"
mkdir -p "$RUN_DIR"

# Config (override via env)
MICROOS_IMAGE_URL=${MICROOS_IMAGE_URL:-"https://download.opensuse.org/tumbleweed/appliances/openSUSE-MicroOS.x86_64-16.0.0-ContainerHost-kvm-and-xen-Snapshot20251121.qcow2"}
MICROOS_IMAGE_PATH=${MICROOS_IMAGE_PATH:-"$ROOT_DIR/build/microos-base.qcow2"}
PICCOLOD_BIN=${PICCOLOD_BIN:-"$ROOT_DIR/piccolod"}
SSH_PORT=${MICROOS_SSH_PORT:-10022}
API_PORT=${MICROOS_API_PORT:-8080}
HOST_API_PORT=${MICROOS_HOST_API_PORT:-${API_PORT}}
MICROOS_VM_CPUS=${MICROOS_VM_CPUS:-2}
MICROOS_VM_RAM=${MICROOS_VM_RAM:-2048}
CSRF=""
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
log_file="$RUN_DIR/e2e.log"
exec > >(tee -a "$log_file") 2>&1

require() { for c in "$@"; do command -v "$c" >/dev/null 2>&1 || { echo "missing $c"; exit 1; }; done; }
require qemu-system-x86_64 qemu-img mkisofs ssh scp curl jq

# Image prep
if [ ! -f "$MICROOS_IMAGE_PATH" ]; then
  log "Downloading MicroOS image..."
  mkdir -p "$(dirname "$MICROOS_IMAGE_PATH")"
  curl -L "$MICROOS_IMAGE_URL" -o "$MICROOS_IMAGE_PATH"
fi
if ! qemu-img info "$MICROOS_IMAGE_PATH" >/dev/null 2>&1; then
  log "Invalid qcow2 image (check MICROOS_IMAGE_URL)"; exit 1
fi
cp "$MICROOS_IMAGE_PATH" "$QCOW_WORK"

# SSH key
if [ -n "${MICROOS_SSH_KEY:-}" ]; then
  SSH_KEY="$MICROOS_SSH_KEY"
else
  SSH_KEY="$RUN_DIR/ephemeral_id_rsa"
  ssh-keygen -q -t rsa -N "" -f "$SSH_KEY" >/dev/null
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

# QEMU
QEMU_NETDEV="user,id=net0,hostfwd=tcp:127.0.0.1:${SSH_PORT}-:22,hostfwd=tcp:127.0.0.1:${HOST_API_PORT}-:${API_PORT}"
QEMU_DISPLAY="-display sdl"; [ "$HEADLESS" = "1" ] && QEMU_DISPLAY="-display none"
log "Starting VM (ssh port $SSH_PORT, api port $API_PORT, cpus $MICROOS_VM_CPUS, ram ${MICROOS_VM_RAM}MB)"
qemu-system-x86_64 \
  -smp "$MICROOS_VM_CPUS" -m "$MICROOS_VM_RAM" \
  -drive if=virtio,file="$QCOW_WORK",format=qcow2 \
  -drive if=virtio,file="$IGNITION_ISO",format=raw \
  -netdev "$QEMU_NETDEV" -device virtio-net,netdev=net0 \
  -boot n \
  $QEMU_DISPLAY \
  -pidfile "$RUN_DIR/qemu.pid" \
  $E2E_DAEMONIZE

cleanup() {
  if [ -f "$RUN_DIR/qemu.pid" ]; then kill "$(cat "$RUN_DIR/qemu.pid")" >/dev/null 2>&1 || true; fi
}
collect_logs() {
  set +e
  log "Collecting logs"
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "journalctl -u piccolod -n 200 --no-pager" > "$RUN_DIR/piccolod.log" 2>/dev/null || true
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "journalctl -t transactional-update -n 200 --no-pager" > "$RUN_DIR/transactional-update.log" 2>/dev/null || true
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "cat /var/log/transactional-update.log" >> "$RUN_DIR/transactional-update.log" 2>/dev/null || true
  set -e
}
trap 'collect_logs; cleanup' EXIT

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

api_curl() { curl -sf -b "$RUN_DIR/cookies" -c "$RUN_DIR/cookies" -H "X-CSRF-Token: ${CSRF:-}" "$@"; }
api_post() { curl -sf -X POST -b "$RUN_DIR/cookies" -c "$RUN_DIR/cookies" -H "X-CSRF-Token: ${CSRF:-}" "$@"; }

install_piccolod() {
  if [ ! -x "$PICCOLOD_BIN" ]; then
    log "Building piccolod binary"
    (cd "$ROOT_DIR" && go build ./cmd/piccolod)
  fi
  scp -i "$SSH_KEY" $SSH_OPTS -P "$SSH_PORT" "$PICCOLOD_BIN" "$SSH_USER@127.0.0.1:/tmp/piccolod" >/dev/null
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "install -m 0755 /tmp/piccolod /usr/local/bin/piccolod && rm -f /tmp/piccolod && (command -v restorecon >/dev/null && restorecon /usr/local/bin/piccolod || true)" >/dev/null
}

start_piccolod() {
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "systemd-run --unit=piccolod --collect --setenv=PORT=${API_PORT} /usr/local/bin/piccolod >/root/piccolod.log 2>&1" >/dev/null
}

wait_for_api() {
  log "Waiting for piccolod API..."
  for _ in $(seq 1 30); do
    if curl -sf "http://127.0.0.1:${HOST_API_PORT}/api/v1/updates/os" >/dev/null 2>&1; then return; fi
    if ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "curl -sf http://127.0.0.1:${API_PORT}/api/v1/updates/os" >/dev/null 2>&1; then return; fi
    sleep 2
  done
  log "piccolod API not responding"; exit 1
}

ensure_gocryptfs() {
  if ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "command -v gocryptfs >/dev/null 2>&1"; then
    return
  fi
  log "Installing gocryptfs via transactional-update (requires reboot)"
  ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "transactional-update -n pkg install gocryptfs" >/dev/null
  log "Rebooting to apply gocryptfs"
  request_reboot
  sleep 5
  wait_for_ssh
}

wait_for_tu() {
  local label=$1
  local start
  start=$(date +%s)
  log "Waiting for transactional-update to finish ($label)..."
  while true; do
    local out
    out=$(ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "systemctl list-units --type=service --state=running 'piccolo-tu-*' transactional-update.service --no-legend --no-pager || true")
    if [ -z "$(echo "$out" | tr -d ' \n\t')" ]; then
      log "transactional-update idle"
      break
    fi
    if [ $(( $(date +%s) - start )) -ge "$TU_WAIT_TIMEOUT" ]; then
      log "Timeout waiting for transactional-update"
      exit 1
    fi
    sleep 5
  done
}

request_reboot() {
  if ssh -i "$SSH_KEY" $SSH_OPTS -p "$SSH_PORT" "$SSH_USER@127.0.0.1" "reboot"; then
    return
  fi
  code=$?
  # SSH usually exits 255 when the connection drops during reboot; treat only other codes as fatal.
  if [ "$code" -ne 255 ]; then
    log "Reboot command failed (exit $code)"
    exit 1
  fi
}

# Session/bootstrap helpers
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

# Flow
wait_for_ssh
ensure_gocryptfs
install_piccolod
start_piccolod
wait_for_api

# Auth bootstrap
api_crypto_setup
api_crypto_unlock
api_setup_admin
require_login
fetch_csrf

BASE_STATUS=$(api_curl "http://127.0.0.1:${HOST_API_PORT}/api/v1/updates/os")
log "Baseline: $BASE_STATUS"

log "Triggering apply via API"
api_post "http://127.0.0.1:${HOST_API_PORT}/api/v1/updates/os/apply"
wait_for_tu "apply"

log "Rebooting"
request_reboot
sleep 5
wait_for_ssh
start_piccolod
wait_for_api
require_login
fetch_csrf
POST_APPLY=$(api_curl "http://127.0.0.1:${HOST_API_PORT}/api/v1/updates/os")
log "Post-apply: $POST_APPLY"

log "Triggering rollback via API"
api_post "http://127.0.0.1:${HOST_API_PORT}/api/v1/updates/os/rollback"
wait_for_tu "rollback"

log "Rebooting after rollback"
request_reboot
sleep 5
wait_for_ssh
start_piccolod
wait_for_api
require_login
fetch_csrf
POST_ROLLBACK=$(api_curl "http://127.0.0.1:${HOST_API_PORT}/api/v1/updates/os")
log "Post-rollback: $POST_ROLLBACK"

cat > "$RUN_DIR/summary.json" <<JSON
{
  "base_status": $BASE_STATUS,
  "post_apply": $POST_APPLY,
  "post_rollback": $POST_ROLLBACK
}
JSON

log "Artifacts in $RUN_DIR"
log "Done"
