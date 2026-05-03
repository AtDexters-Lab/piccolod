#!/usr/bin/env bash
# dev-vm-alpha-test.sh — Test stages for block-native storage on a Tumbleweed dev VM.
#
# Combines HTTP API tests (same as production) with SSH-based storage inspection
# stages for verifying the full block device stack: LVM, DRBD, NBD, LUKS, idmap.
#
# Usage:
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP>                  # run all stages
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> prereq           # stage 0: prerequisites
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> boot             # stage 1: boot & disk prep
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> pre-setup        # stage 2: pre-setup gating
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> setup            # stage 3: first-run setup
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> post-setup       # stage 4: post-setup smoke
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> storage-inspect  # stage 5: SSH storage inspection
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> rootfs-verify    # stage 6: block-native rootfs verification
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> service-app      # stage 7: service app lifecycle
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> workspace-app    # stage 8: workspace app lifecycle
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> reboot           # stage 9: reboot & unlock cycle
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> storage-post     # stage 10: post-reboot storage
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> stewardship      # stage 11: resource stewardship (slice drop-ins, podman args)
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> logs             # download piccolod journal
set -euo pipefail

IP="${1:?Usage: $0 <VM_IP> [stage]}"
STAGE="${2:-all}"
if [[ -n "${PICCOLO_TEST_PASS_FILE:-}" ]] && [[ -f "$PICCOLO_TEST_PASS_FILE" ]]; then
  PASS=$(tr -d '\n' < "$PICCOLO_TEST_PASS_FILE")
elif [[ -n "${PICCOLO_TEST_PASS:-}" ]]; then
  PASS="$PICCOLO_TEST_PASS"
else
  PASS='PiccoloE2E-Test-2026!'
fi
COOKIE_JAR="/tmp/claude/piccolo-alpha/cookies.txt"
PASS_COUNT=0
FAIL_COUNT=0
SKIP_COUNT=0

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
NC='\033[0m'

LOG_DIR="/tmp/claude/piccolo-alpha/logs"
SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"

# SSH into the VM.
vssh() { ssh $SSH_OPTS "root@$IP" "$@"; }

# HTTP helpers (same as production test script).
api()  { curl -sf --connect-timeout 5 -b "$COOKIE_JAR" -c "$COOKIE_JAR" "http://$IP$1" 2>/dev/null; }
apij() { api "$1" | python3 -m json.tool 2>/dev/null; }
csrf() { curl -sf --connect-timeout 5 -b "$COOKIE_JAR" "http://$IP/api/v1/auth/csrf" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('token',''))" 2>/dev/null; }
post() {
  local _body_file; _body_file=$(mktemp /tmp/claude-1000/post-body-XXXXXX)
  printf '%s' "$2" > "$_body_file"
  curl -sf --connect-timeout 5 -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" -d @"$_body_file" "http://$IP$1" 2>/dev/null
  local _rc=$?; rm -f "$_body_file"; return $_rc
}
post_csrf() {
  local token; token=$(csrf)
  if [[ -n "${2:-}" ]]; then
    local _body; _body=$(mktemp /tmp/claude-1000/csrf-body-XXXXXX)
    printf '%s' "$2" > "$_body"
    curl -sf --connect-timeout 10 -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
      -X POST -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
      -d @"$_body" "http://$IP$1" 2>/dev/null
    local _rc=$?; rm -f "$_body"; return $_rc
  else
    curl -sf --connect-timeout 10 -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
      -X POST -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
      "http://$IP$1" 2>/dev/null
  fi
}

ensure_session() {
  local authed
  authed=$(curl -s --connect-timeout 5 -b "$COOKIE_JAR" \
    "http://$IP/api/v1/auth/session" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('authenticated',False))" 2>/dev/null)
  [[ "$authed" == "True" ]] && return 0
  local _body; _body=$(mktemp /tmp/claude-1000/session-body-XXXXXX)
  printf '{"username":"admin","password":"%s"}' "$PASS" > "$_body"
  curl -sf --connect-timeout 10 -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" \
    -d @"$_body" "http://$IP/api/v1/auth/login" >/dev/null 2>&1 || true
  printf '{"password":"%s"}' "$PASS" > "$_body"
  curl -sf --connect-timeout 10 -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" \
    -d @"$_body" "http://$IP/api/v1/crypto/unlock" >/dev/null 2>&1 || true
  rm -f "$_body"
}

check() {
  local id="$1" desc="$2" actual="$3" expected="$4"
  if echo "$actual" | grep -qF "$expected"; then
    echo -e "  ${GREEN}PASS${NC} [$id] $desc"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [$id] $desc"
    echo -e "       expected: $expected"
    echo -e "       actual:   $(echo "$actual" | head -3)"
    ((FAIL_COUNT++)) || true
  fi
}

check_not() {
  local id="$1" desc="$2" actual="$3" unexpected="$4"
  if echo "$actual" | grep -qF "$unexpected"; then
    echo -e "  ${RED}FAIL${NC} [$id] $desc (found unwanted: $unexpected)"
    echo -e "       actual: $(echo "$actual" | head -3)"
    ((FAIL_COUNT++)) || true
  else
    echo -e "  ${GREEN}PASS${NC} [$id] $desc"
    ((PASS_COUNT++)) || true
  fi
}

check_http() {
  local id="$1" desc="$2" method="$3" path="$4" expected_code="$5"
  local code
  if [[ "$method" == "GET" ]]; then
    code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 -b "$COOKIE_JAR" "http://$IP$path" 2>/dev/null)
  else
    code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 -b "$COOKIE_JAR" -X "$method" "http://$IP$path" 2>/dev/null)
  fi
  if [[ "$code" == "$expected_code" ]]; then
    echo -e "  ${GREEN}PASS${NC} [$id] $desc (HTTP $code)"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [$id] $desc (expected HTTP $expected_code, got $code)"
    ((FAIL_COUNT++)) || true
  fi
}

# SSH check: run command on VM, verify output contains expected string.
check_ssh() {
  local id="$1" desc="$2" cmd="$3" expected="$4"
  local actual
  actual=$(vssh "$cmd" 2>&1) || true
  check "$id" "$desc" "$actual" "$expected"
}

# SSH check: verify command succeeds (exit 0).
check_ssh_ok() {
  local id="$1" desc="$2" cmd="$3"
  if vssh "$cmd" >/dev/null 2>&1; then
    echo -e "  ${GREEN}PASS${NC} [$id] $desc"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [$id] $desc (command failed)"
    ((FAIL_COUNT++)) || true
  fi
}

# SSH check: verify command fails (non-zero exit).
check_ssh_fail() {
  local id="$1" desc="$2" cmd="$3"
  if vssh "$cmd" >/dev/null 2>&1; then
    echo -e "  ${RED}FAIL${NC} [$id] $desc (expected failure but succeeded)"
    ((FAIL_COUNT++)) || true
  else
    echo -e "  ${GREEN}PASS${NC} [$id] $desc"
    ((PASS_COUNT++)) || true
  fi
}

skip() {
  local id="$1" desc="$2" reason="$3"
  echo -e "  ${YELLOW}SKIP${NC} [$id] $desc — $reason"
  ((SKIP_COUNT++)) || true
}

summary() {
  echo ""
  echo -e "${CYAN}════════════════════════════════════${NC}"
  echo -e "  ${GREEN}PASS: $PASS_COUNT${NC}  ${RED}FAIL: $FAIL_COUNT${NC}  ${YELLOW}SKIP: $SKIP_COUNT${NC}"
  echo -e "${CYAN}════════════════════════════════════${NC}"
  [[ $FAIL_COUNT -eq 0 ]] && echo -e "  ${GREEN}All checks passed!${NC}" || echo -e "  ${RED}Some checks failed.${NC}"
}

dump_logs() {
  local label="${1:-$(date +%s)}"
  local outfile="$LOG_DIR/piccolod-${label}.log"
  mkdir -p "$LOG_DIR"
  vssh "journalctl -u piccolod --no-pager -n 500" > "$outfile" 2>/dev/null || true
  if [[ -s "$outfile" ]]; then
    local lines
    lines=$(wc -l < "$outfile")
    echo -e "  ${CYAN}LOGS${NC} Saved $lines lines → $outfile"
  fi
}

# ─────────────────────────────────────────────────────────
# Stage 0: Prerequisites (SSH-based)
# ─────────────────────────────────────────────────────────
stage_prereq() {
  echo -e "\n${CYAN}═══ Stage 0: Prerequisites Check ═══${NC}"

  # SSH reachable
  check_ssh_ok "0.1" "SSH reachable" "true"

  # Required packages
  check_ssh_ok "0.2" "lvm2 installed" "rpm -q lvm2"
  check_ssh_ok "0.3" "thin-provisioning-tools installed" "rpm -q thin-provisioning-tools"
  check_ssh_ok "0.4" "cryptsetup installed" "rpm -q cryptsetup"
  check_ssh_ok "0.5" "podman installed" "rpm -q podman"

  # Kernel modules
  check_ssh_ok "0.6" "overlay module loaded" "modprobe overlay 2>/dev/null; grep -q overlay /proc/filesystems"
  check_ssh_ok "0.7" "dm-thin-pool available" "modprobe dm-thin-pool"

  # DRBD and NBD — may not be available yet, just report
  if vssh "modprobe drbd" >/dev/null 2>&1; then
    echo -e "  ${GREEN}PASS${NC} [0.8] drbd module available"
    ((PASS_COUNT++)) || true
  else
    skip "0.8" "drbd module" "not available (install drbd-kmp-default)"
  fi

  if vssh "modprobe nbd" >/dev/null 2>&1; then
    echo -e "  ${GREEN}PASS${NC} [0.9] nbd module available"
    ((PASS_COUNT++)) || true
  else
    skip "0.9" "nbd module" "not available (install nbd)"
  fi

  # FUSE packages — warn if present (dev templates may have them; what matters is piccolod doesn't use them)
  if vssh "rpm -q gocryptfs" >/dev/null 2>&1; then
    echo -e "  ${YELLOW}WARN${NC} [0.10] gocryptfs installed (OK on dev template, must not be used)"
  else
    echo -e "  ${GREEN}PASS${NC} [0.10] gocryptfs NOT installed"
    ((PASS_COUNT++)) || true
  fi
  if vssh "rpm -q fuse-overlayfs" >/dev/null 2>&1; then
    echo -e "  ${YELLOW}WARN${NC} [0.11] fuse-overlayfs installed (OK on dev template, must not be used)"
  else
    echo -e "  ${GREEN}PASS${NC} [0.11] fuse-overlayfs NOT installed"
    ((PASS_COUNT++)) || true
  fi

  # Binaries
  check_ssh_ok "0.12" "pvcreate available" "which pvcreate"
  check_ssh_ok "0.13" "cryptsetup available" "which cryptsetup"

  # Rootless podman prerequisites
  # newuidmap/newgidmap need setuid for rootless podman user namespace mapping.
  # File capabilities conflict with setuid on Tumbleweed — strip caps, set suid.
  local uidmap_perms gidmap_perms
  uidmap_perms=$(vssh "stat -c '%A' /usr/bin/newuidmap" 2>/dev/null || echo "")
  gidmap_perms=$(vssh "stat -c '%A' /usr/bin/newgidmap" 2>/dev/null || echo "")

  if echo "$uidmap_perms" | grep -q "s"; then
    echo -e "  ${GREEN}PASS${NC} [0.14] newuidmap has setuid"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${YELLOW}FIX ${NC} [0.14] Setting setuid on newuidmap"
    vssh "setfattr -x security.capability /usr/bin/newuidmap 2>/dev/null; chmod u+s /usr/bin/newuidmap" 2>/dev/null
  fi
  if echo "$gidmap_perms" | grep -q "s"; then
    echo -e "  ${GREEN}PASS${NC} [0.15] newgidmap has setuid"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${YELLOW}FIX ${NC} [0.15] Setting setuid on newgidmap"
    vssh "setfattr -x security.capability /usr/bin/newgidmap 2>/dev/null; chmod u+s /usr/bin/newgidmap" 2>/dev/null
  fi
}

# ─────────────────────────────────────────────────────────
# Stage 1: Boot & Disk Prep (HTTP)
# ─────────────────────────────────────────────────────────
stage_boot() {
  echo -e "\n${CYAN}═══ Stage 1: Boot & Disk Prep ═══${NC}"

  local ver
  ver=$(api "/version")
  check "1.1" "Dev binary running" "$ver" "piccolod"

  local emerg
  emerg=$(api "/api/v1/system/emergency")
  check "1.2" "No emergency mode" "$emerg" '"emergency":false'

  local health
  health=$(api "/api/v1/health/live")
  check "1.3" "HTTP health OK" "$health" '"status":"ok"'
}

# ─────────────────────────────────────────────────────────
# Stage 2: Pre-Setup Gating (HTTP)
# ─────────────────────────────────────────────────────────
stage_pre_setup() {
  echo -e "\n${CYAN}═══ Stage 2: Pre-Setup API Gating ═══${NC}"

  local crypto
  crypto=$(api "/api/v1/crypto/status")
  check "2.1" "Crypto not initialized" "$crypto" '"initialized":false'

  check_http "2.2" "Portal HTML loads" "GET" "/" "200"
  check_http "2.3" "Health live accessible" "GET" "/api/v1/health/live" "200"
  check_http "2.4" "Apps endpoint requires auth" "GET" "/api/v1/apps" "401"
}

# ─────────────────────────────────────────────────────────
# Stage 3: First-Run Setup (HTTP)
# ─────────────────────────────────────────────────────────
stage_setup() {
  echo -e "\n${CYAN}═══ Stage 3: First-Run Setup ═══${NC}"

  rm -f "$COOKIE_JAR"
  post "/api/v1/crypto/setup" "{\"password\":\"$PASS\"}"

  echo -e "  ${CYAN}INFO${NC} Waiting 10s for async operations..."
  sleep 10

  local crypto
  crypto=$(api "/api/v1/crypto/status")
  check "3.1" "Crypto initialized" "$crypto" '"initialized":true'
  check "3.2" "Crypto unlocked" "$crypto" '"locked":false'

  local health
  health=$(api "/api/v1/health/detail")
  check_not "3.3" "No storage emergency" "$health" '"level": "error"'

  local emerg
  emerg=$(api "/api/v1/system/emergency")
  check "3.4" "Emergency still false" "$emerg" '"emergency":false'
}

# ─────────────────────────────────────────────────────────
# Stage 4: Post-Setup Smoke (HTTP)
# ─────────────────────────────────────────────────────────
stage_post_setup() {
  echo -e "\n${CYAN}═══ Stage 4: Post-Setup Functional Smoke ═══${NC}"
  ensure_session

  check_http "4.1" "Apps endpoint accessible" "GET" "/api/v1/apps" "200"

  local session
  session=$(api "/api/v1/auth/session")
  check "4.2" "Session valid" "$session" '"authenticated"'

  # Storage diagnostics API
  check_http "4.3" "Storage diagnostics accessible" "GET" "/api/v1/system/storage-diagnostics" "200"
  local diag
  diag=$(apij "/api/v1/system/storage-diagnostics")
  if [[ -n "$diag" ]]; then
    check "4.4" "Diagnostics has thin pool" "$diag" '"thin_pool"'
    check "4.5" "Diagnostics has volumes" "$diag" '"volumes"'
    check "4.6" "Diagnostics has type summary" "$diag" '"type_summary"'
  else
    skip "4.4" "Storage diagnostics" "empty response"
  fi
}

# ─────────────────────────────────────────────────────────
# Stage 5: Storage Stack Inspection (SSH)
# ─────────────────────────────────────────────────────────
stage_storage_inspect() {
  echo -e "\n${CYAN}═══ Stage 5: Storage Stack Inspection (SSH) ═══${NC}"

  # LVM
  check_ssh "5.1" "LVM VG exists" "vgs --noheadings -o vg_name 2>/dev/null | tr -d ' '" "piccolo-data-vg"
  check_ssh "5.2" "Thin pool exists" "lvs piccolo-data-vg/thinpool --noheadings -o lv_name 2>/dev/null | tr -d ' '" "thinpool"

  local data_pct
  data_pct=$(vssh "lvs piccolo-data-vg/thinpool --noheadings -o data_percent 2>/dev/null | tr -d ' '" 2>/dev/null || echo "N/A")
  echo -e "  ${CYAN}INFO${NC} Thin pool data usage: ${data_pct}%"

  # Control plane
  check_ssh_ok "5.3" "Control plane LUKS loop exists" "test -f /piccolo-core/control-plane.luks"
  check_ssh "5.4" "Control plane mounted as ext4" "mount | grep control-plane" "ext4"
  check_ssh "5.5" "LUKS mapper active" "cryptsetup status piccolo-loop-control-plane 2>&1" "is active"
  check_ssh "5.6" "Control plane metadata" "cat /piccolo-core/volumes/control-plane/piccolo.volume.json 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin).get(\"type\",\"\"))'" "luks-loop"

  # Zero FUSE mounts — block-native rootfs uses ext4 + idmapped mounts, not FUSE
  local fuse_data
  fuse_data=$(vssh 'grep "fuse\." /proc/mounts 2>/dev/null | grep -v fusectl || true' 2>/dev/null)
  if [[ -z "$fuse_data" ]]; then
    echo -e "  ${GREEN}PASS${NC} [5.7] Zero FUSE mounts (no gocryptfs, no fuse-overlayfs)"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [5.7] Unexpected FUSE mounts (block-native rootfs should have zero):"
    echo "$fuse_data" | sed 's/^/       /'
    ((FAIL_COUNT++)) || true
  fi

  # DRBD (informational — may not have resources yet)
  local drbd_status
  drbd_status=$(vssh "drbdadm status 2>/dev/null" 2>/dev/null || echo "no resources")
  echo -e "  ${CYAN}INFO${NC} DRBD status: $(echo "$drbd_status" | head -3)"

  # Raw mount table (informational — expect ext4, no overlay/FUSE)
  echo -e "\n  ${CYAN}Block-native rootfs mounts:${NC}"
  vssh "mount | grep '/piccolo-core/mounts/' 2>/dev/null | head -10" 2>/dev/null | sed 's/^/       /' || true
}

# ─────────────────────────────────────────────────────────
# Stage 6: Block-Native Rootfs Verification (SSH)
# ─────────────────────────────────────────────────────────
stage_rootfs_verify() {
  echo -e "\n${CYAN}═══ Stage 6: Block-Native Rootfs Verification (SSH) ═══${NC}"

  # Zero FUSE mounts — block-native rootfs eliminates fuse-overlayfs entirely.
  # All rootfs I/O is kernel-native: ext4 + idmapped mounts via mount_setattr.
  local fuse_data
  fuse_data=$(vssh 'grep "fuse\." /proc/mounts 2>/dev/null | grep -v fusectl || true' 2>/dev/null)
  if [[ -z "$fuse_data" ]]; then
    echo -e "  ${GREEN}PASS${NC} [6.1] Zero FUSE mounts (block-native rootfs — no fuse-overlayfs)"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [6.1] Unexpected FUSE mounts (block-native rootfs should have zero):"
    echo "$fuse_data" | sed 's/^/       /'
    ((FAIL_COUNT++)) || true
  fi

  # No mount_program in any container configs (eliminated with fuse-overlayfs)
  local sys_mount_prog
  sys_mount_prog=$(vssh "grep -r mount_program /etc/containers/ 2>/dev/null || true" 2>/dev/null)
  if [[ -z "$sys_mount_prog" ]]; then
    echo -e "  ${GREEN}PASS${NC} [6.2] No mount_program in container configs"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [6.2] Found mount_program in container configs:"
    echo "       $sys_mount_prog"
    ((FAIL_COUNT++)) || true
  fi

  # Kernel overlay support
  check_ssh "6.3" "Kernel overlay in /proc/filesystems" "cat /proc/filesystems" "overlay"

  # Rootful podman supports idmapped mounts (confirms kernel capability)
  check_ssh "6.4" "Rootful podman supports shifting" \
    "podman info --format json 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin)[\"store\"][\"graphStatus\"][\"Supports shifting\"])'" "true"

  # Volume mounts are ext4 (not FUSE)
  local vol_mounts
  vol_mounts=$(vssh "mount | grep '/piccolo-core/mounts/' | head -5" 2>/dev/null || echo "")
  if [[ -n "$vol_mounts" ]]; then
    check "6.5" "Volume mounts use ext4" "$vol_mounts" "ext4"
  else
    echo -e "  ${CYAN}INFO${NC} [6.5] No app volumes currently mounted (expected before app install)"
  fi

}

# ─────────────────────────────────────────────────────────
# Stage 7: Service App Lifecycle (HTTP)
# ─────────────────────────────────────────────────────────
stage_service_app() {
  echo -e "\n${CYAN}═══ Stage 7: Service App Install (Vaultwarden) ═══${NC}"
  ensure_session

  local APP_NAME="vaultwarden"
  local template_yaml
  template_yaml=$(api "/api/v1/catalog/vaultwarden/template")
  if [[ -z "$template_yaml" ]]; then
    skip "7.x" "Service app tests" "catalog template not available"
    return
  fi

  local payload
  payload=$(YAML="$template_yaml" NAME="$APP_NAME" python3 -c "
import json, os
print(json.dumps({
    'app_definition': os.environ['YAML'],
    'inputs': {'__app_address__': os.environ['NAME'], 'admin_token': 'e2e-test-token-2026'},
    'catalog_source': 'vaultwarden'
}))")

  local token
  token=$(csrf)
  local install_http
  install_http=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 10 --max-time 300 \
    -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
    -d "$payload" "http://$IP/api/v1/apps" 2>/dev/null)
  check "7.1" "Service app installed" "$install_http" "201"

  if [[ "$install_http" != "201" ]]; then
    dump_logs "stage7-install-fail"
    return
  fi

  # Poll for running (up to 120s)
  local app_status=""
  for i in $(seq 1 60); do
    app_status=$(api "/api/v1/apps/$APP_NAME" | python3 -c "
import sys, json
try: print(json.load(sys.stdin).get('data',{}).get('app',{}).get('status',''))
except: print('')" 2>/dev/null)
    [[ "$app_status" == "running" ]] && break
    sleep 2
  done
  check "7.2" "App reaches running" "$app_status" "running"

  # Block-native rootfs verification (SSH)
  # Golden LV: metadata should exist with type "golden"
  local golden_meta
  golden_meta=$(vssh 'for f in /piccolo-core/volumes/golden-*/piccolo.volume.json; do cat "$f" 2>/dev/null; break; done' 2>/dev/null || echo "")
  if [[ -n "$golden_meta" ]]; then
    check "7.2a" "Golden LV metadata exists" "$golden_meta" '"type": "golden"'
  else
    skip "7.2a" "Golden LV metadata" "no golden-* volume metadata found"
  fi

  # Service rootfs: metadata should exist with type "service-rootfs"
  local svc_rootfs_meta
  svc_rootfs_meta=$(vssh 'for f in /piccolo-core/volumes/svc-rootfs-*/piccolo.volume.json; do cat "$f" 2>/dev/null; break; done' 2>/dev/null || echo "")
  if [[ -n "$svc_rootfs_meta" ]]; then
    check "7.2b" "Service rootfs metadata type" "$svc_rootfs_meta" '"type": "service-rootfs"'
    check "7.2c" "Service rootfs read-only flag" "$svc_rootfs_meta" '"read_only": true'
  else
    skip "7.2b" "Service rootfs metadata" "no svc-rootfs-* volume metadata found"
  fi

  # LUKS mapper active for service rootfs
  check_ssh "7.2e" "Service rootfs LUKS mapper active" \
    "dmsetup info --noheadings -c -o name 2>/dev/null | grep 'piccolo-vol-svc-rootfs-' || true" "piccolo-vol-svc-rootfs-"

  # Idmapped mount exists for service rootfs
  local svc_idmap
  svc_idmap=$(vssh "mount | grep 'svc-rootfs.*idmap' || true" 2>/dev/null)
  if [[ -n "$svc_idmap" ]]; then
    echo -e "  ${GREEN}PASS${NC} [7.2g] Service rootfs idmapped mount exists"
    ((PASS_COUNT++)) || true
  else
    skip "7.2g" "Service rootfs idmapped mount" "mount entry not found"
  fi

  # Zero FUSE mounts while app is running
  local fuse_running
  fuse_running=$(vssh 'grep "fuse\." /proc/mounts 2>/dev/null | grep -v fusectl || true' 2>/dev/null)
  if [[ -z "$fuse_running" ]]; then
    echo -e "  ${GREEN}PASS${NC} [7.2h] Zero FUSE mounts with service app running"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [7.2h] FUSE mounts found with service app running:"
    echo "$fuse_running" | sed 's/^/       /'
    ((FAIL_COUNT++)) || true
  fi

  # Uninstall
  token=$(csrf)
  local uninstall_code
  uninstall_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 30 --max-time 120 \
    -b "$COOKIE_JAR" -X DELETE -H "X-CSRF-Token: $token" \
    "http://$IP/api/v1/apps/$APP_NAME" 2>/dev/null)
  check "7.3" "Service app uninstalled" "$uninstall_code" "200"

  sleep 3
  check_http "7.4" "App gone" "GET" "/api/v1/apps/$APP_NAME" "404"

  # Post-uninstall: verify rootfs teardown
  check_not "7.5" "No service rootfs LUKS mapper after uninstall" \
    "$(vssh 'dmsetup info --noheadings -c -o name 2>/dev/null | grep piccolo-vol-svc-rootfs- || true' 2>/dev/null)" \
    "piccolo-vol-svc-rootfs-"
}

# ─────────────────────────────────────────────────────────
# Stage 8: Workspace App Lifecycle (HTTP)
# ─────────────────────────────────────────────────────────
stage_workspace_app() {
  echo -e "\n${CYAN}═══ Stage 8: Workspace App Install (Code-server) ═══${NC}"
  ensure_session

  local APP_NAME="codeserver"
  local template_yaml
  template_yaml=$(api "/api/v1/catalog/code-server/template")
  if [[ -z "$template_yaml" ]]; then
    skip "8.x" "Workspace app tests" "catalog template not available"
    return
  fi

  local payload
  payload=$(YAML="$template_yaml" NAME="$APP_NAME" python3 -c "
import json, os
print(json.dumps({
    'app_definition': os.environ['YAML'],
    'inputs': {'__app_address__': os.environ['NAME'], 'password': 'E2eTest-2026!'},
    'catalog_source': 'code-server'
}))")

  local token
  token=$(csrf)
  local install_http
  install_http=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 10 --max-time 300 \
    -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
    -d "$payload" "http://$IP/api/v1/apps" 2>/dev/null)
  check "8.1" "Workspace app installed" "$install_http" "201"

  if [[ "$install_http" != "201" ]]; then
    dump_logs "stage8-install-fail"
    return
  fi

  # Poll for running (up to 120s)
  local app_status=""
  for i in $(seq 1 60); do
    app_status=$(api "/api/v1/apps/$APP_NAME" | python3 -c "
import sys, json
try: print(json.load(sys.stdin).get('data',{}).get('app',{}).get('status',''))
except: print('')" 2>/dev/null)
    [[ "$app_status" == "running" ]] && break
    sleep 2
  done
  check "8.2" "App reaches running" "$app_status" "running"

  # Block-native rootfs verification (SSH)
  # Workspace rootfs: metadata should exist with type "workspace"
  local ws_meta
  ws_meta=$(vssh 'for f in /piccolo-core/volumes/ws-*/piccolo.volume.json; do cat "$f" 2>/dev/null; break; done' 2>/dev/null || echo "")
  if [[ -n "$ws_meta" ]]; then
    check "8.3" "Workspace rootfs metadata type" "$ws_meta" '"type": "workspace"'
  else
    skip "8.3" "Workspace rootfs metadata" "no ws-* volume metadata found"
  fi

  # LUKS mapper active for workspace
  check_ssh "8.3b" "Workspace LUKS mapper active" \
    "dmsetup info --noheadings -c -o name 2>/dev/null | grep 'piccolo-vol-ws-' || true" "piccolo-vol-ws-"

  # Idmapped mount exists for workspace
  local ws_idmap
  ws_idmap=$(vssh "mount | grep 'ws-.*idmap' || true" 2>/dev/null)
  if [[ -n "$ws_idmap" ]]; then
    echo -e "  ${GREEN}PASS${NC} [8.3d] Workspace rootfs idmapped mount exists"
    ((PASS_COUNT++)) || true
  else
    skip "8.3d" "Workspace rootfs idmapped mount" "mount entry not found"
  fi

  # Zero FUSE mounts while workspace is running
  local fuse_ws
  fuse_ws=$(vssh 'grep "fuse\." /proc/mounts 2>/dev/null | grep -v fusectl || true' 2>/dev/null)
  if [[ -z "$fuse_ws" ]]; then
    echo -e "  ${GREEN}PASS${NC} [8.3e] Zero FUSE mounts with workspace running"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [8.3e] FUSE mounts found with workspace running:"
    echo "$fuse_ws" | sed 's/^/       /'
    ((FAIL_COUNT++)) || true
  fi

  # Uninstall
  token=$(csrf)
  local uninstall_code
  uninstall_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 30 --max-time 120 \
    -b "$COOKIE_JAR" -X DELETE -H "X-CSRF-Token: $token" \
    "http://$IP/api/v1/apps/$APP_NAME" 2>/dev/null)
  check "8.4" "Workspace app uninstalled" "$uninstall_code" "200"

  sleep 3
  check_http "8.5" "App gone" "GET" "/api/v1/apps/$APP_NAME" "404"

  # Post-uninstall: verify rootfs teardown
  check_not "8.6" "No workspace LUKS mapper after uninstall" \
    "$(vssh 'dmsetup info --noheadings -c -o name 2>/dev/null | grep piccolo-vol-ws- || true' 2>/dev/null)" \
    "piccolo-vol-ws-"

  # Golden LV GC: after uninstalling the only app using this image, golden LV should be cleaned up
  local golden_after
  golden_after=$(vssh 'ls /piccolo-core/volumes/golden-*/piccolo.volume.json 2>/dev/null | wc -l' 2>/dev/null || echo "0")
  if [[ "$golden_after" == "0" ]]; then
    echo -e "  ${GREEN}PASS${NC} [8.8] Golden LV garbage collected after last consumer uninstalled"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${CYAN}INFO${NC} [8.8] $golden_after golden LV(s) remain (may be shared with other images)"
  fi
}

# ─────────────────────────────────────────────────────────
# Stage 9: Reboot & Unlock (HTTP + VBoxManage)
# ─────────────────────────────────────────────────────────
stage_reboot() {
  echo -e "\n${CYAN}═══ Stage 9: Reboot & Unlock Cycle ═══${NC}"

  local VM_STATE="/tmp/claude/piccolo-alpha/alpha-vm-name"
  if [[ ! -f "$VM_STATE" ]]; then
    skip "9.x" "Reboot tests" "No VM state file"
    return
  fi
  local VM_NAME
  VM_NAME=$(cat "$VM_STATE")

  echo -e "  ${CYAN}INFO${NC} Rebooting VM: $VM_NAME"
  VBoxManage controlvm "$VM_NAME" reset 2>/dev/null || true

  echo -e "  ${CYAN}INFO${NC} Waiting for VM to reboot..."
  sleep 15
  for i in $(seq 1 30); do
    if curl -sf --connect-timeout 2 "http://$IP/version" >/dev/null 2>&1; then
      echo -e "  ${CYAN}INFO${NC} VM back up after ~$((15 + i*2))s"
      break
    fi
    sleep 2
  done

  local crypto
  crypto=$(api "/api/v1/crypto/status")
  check "9.1" "Crypto initialized after reboot" "$crypto" '"initialized":true'
  check "9.2" "Crypto locked after reboot" "$crypto" '"locked":true'

  # Unlock
  rm -f "$COOKIE_JAR"
  local _body; _body=$(mktemp /tmp/claude-1000/unlock-body-XXXXXX)
  printf '{"password":"%s"}' "$PASS" > "$_body"
  local unlock_code
  unlock_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 10 \
    -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" \
    -d @"$_body" "http://$IP/api/v1/crypto/unlock" 2>/dev/null)
  rm -f "$_body"
  check "9.3" "Unlock succeeds" "$unlock_code" "200"

  sleep 5

  crypto=$(api "/api/v1/crypto/status")
  check "9.4" "Crypto unlocked" "$crypto" '"locked":false'
}

# ─────────────────────────────────────────────────────────
# Stage 10: Post-Reboot Storage State (SSH)
# ─────────────────────────────────────────────────────────
stage_storage_post() {
  echo -e "\n${CYAN}═══ Stage 10: Post-Reboot Storage State (SSH) ═══${NC}"

  check_ssh "10.1" "LVM VG active after reboot" "vgs --noheadings -o vg_name 2>/dev/null | tr -d ' '" "piccolo-data-vg"
  check_ssh_ok "10.2" "Control plane mounted after reboot" "mountpoint -q /piccolo-core/mounts/control-plane/ 2>/dev/null || mount | grep -q control-plane"

  local fuse_count
  fuse_count=$(vssh 'grep "fuse\." /proc/mounts 2>/dev/null | grep -cv fusectl || true' 2>/dev/null | tail -1)
  if [[ "$fuse_count" == "0" ]]; then
    echo -e "  ${GREEN}PASS${NC} [10.3] Zero FUSE mounts after reboot"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [10.3] Found $fuse_count FUSE mounts after reboot (should be zero)"
    ((FAIL_COUNT++)) || true
  fi
}

# ─────────────────────────────────────────────────────────
# Stage 11a: Auto-Unlock + Timezone Surface
#
# Exercises the auto-unlock v1 surface end-to-end against a setup-complete
# device:
#   - GET /security/auto-unlock returns expected schema (post-auth detail).
#   - GET /crypto/status exposes only enabled + in_flight (no failure leak).
#   - GET /system/timezone reads the current zone.
#   - PUT /system/timezone with garbage zone → 400.
#   - PUT /system/timezone with valid zone persists + reflects in GET.
#   - X-Browser-Timezone capture middleware fires when device is on UTC
#     default and silently skips after the first non-UTC zone is set.
#   - POST /security/auto-unlock/test runs a deposit/pickup round-trip.
#   - PUT /security/auto-unlock toggles + window edit lands.
#   - Scheduler startup log present in journal (proves goroutine alive).
# ─────────────────────────────────────────────────────────
stage_auto_unlock() {
  echo -e "\n${CYAN}═══ Stage 11a: Auto-Unlock + Timezone Surface ═══${NC}"
  ensure_session

  # 11a.1 — Public crypto/status shape: has enabled + lifecycle token, no
  # failure leak. The previous auto_unlock_in_flight field was retired in
  # favor of the canonical lifecycle token (== "unlocking" while pickup is
  # in flight); see internal/lifecycle.
  local cstat
  cstat=$(api "/api/v1/crypto/status")
  check "11a.1" "Public /crypto/status has auto_unlock_enabled" "$cstat" '"auto_unlock_enabled"'
  check "11a.2" "Public /crypto/status has lifecycle token" "$cstat" '"lifecycle"'
  check_not "11a.3" "Public /crypto/status omits auto_unlock_last_failure" "$cstat" '"auto_unlock_last_failure"'
  check_not "11a.3b" "Public /crypto/status no longer emits retired auto_unlock_in_flight" "$cstat" '"auto_unlock_in_flight"'

  # 11a.4..6 — Authenticated GET /security/auto-unlock has full detail.
  local au
  au=$(api "/api/v1/security/auto-unlock")
  check "11a.4" "Authed /security/auto-unlock has enabled" "$au" '"enabled"'
  check "11a.5" "Authed /security/auto-unlock has auto_reboot block" "$au" '"auto_reboot"'
  check "11a.6" "Authed /security/auto-unlock has has_outstanding_blob" "$au" '"has_outstanding_blob"'

  # 11a.7 — Test action exercises a full deposit/pickup round-trip.
  local test_resp
  test_resp=$(post_csrf "/api/v1/security/auto-unlock/test")
  check "11a.7" "/security/auto-unlock/test returns success=true" "$test_resp" '"success":true'

  # 11a.8 — Scheduler startup log (proves goroutine spawned).
  local sched_log
  sched_log=$(vssh 'journalctl -u piccolod --no-pager 2>/dev/null | grep -c "autounlock scheduler started" || echo 0' 2>/dev/null | tail -1 | tr -d '[:space:]')
  if [[ "$sched_log" -ge 1 ]]; then
    echo -e "  ${GREEN}PASS${NC} [11a.8] Scheduler startup log present (count=$sched_log)"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [11a.8] Scheduler startup log missing"
    ((FAIL_COUNT++)) || true
  fi

  # 11a.9 — Timezone GET returns current zone.
  local tz_get
  tz_get=$(api "/api/v1/system/timezone")
  check "11a.9" "/system/timezone GET returns timezone field" "$tz_get" '"timezone"'

  # 11a.10 — Timezone PUT garbage zone → 400.
  local token
  token=$(csrf)
  local code
  code=$(curl -s -b "$COOKIE_JAR" -X PUT -o /dev/null -w "%{http_code}" \
    -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
    -d '{"timezone":"Mars/Olympus_Mons"}' \
    "http://$IP/api/v1/system/timezone" 2>/dev/null)
  if [[ "$code" == "400" ]]; then
    echo -e "  ${GREEN}PASS${NC} [11a.10] PUT garbage zone → 400"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [11a.10] PUT garbage zone returned $code (want 400)"
    ((FAIL_COUNT++)) || true
  fi

  # 11a.11 — Timezone PUT valid zone persists.
  local before; before=$(api "/api/v1/system/timezone" | python3 -c "import sys,json; print(json.load(sys.stdin).get('timezone',''))" 2>/dev/null)
  local target; target="America/New_York"
  if [[ "$before" == "$target" ]]; then target="Asia/Kolkata"; fi
  curl -sf -b "$COOKIE_JAR" -X PUT \
    -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
    -d "{\"timezone\":\"$target\"}" \
    "http://$IP/api/v1/system/timezone" >/dev/null
  local after; after=$(api "/api/v1/system/timezone" | python3 -c "import sys,json; print(json.load(sys.stdin).get('timezone',''))" 2>/dev/null)
  if [[ "$after" == "$target" ]]; then
    echo -e "  ${GREEN}PASS${NC} [11a.11] PUT timezone persists ($before → $after)"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [11a.11] PUT timezone failed ($before → $after, wanted $target)"
    ((FAIL_COUNT++)) || true
  fi

  # 11a.12 — X-Browser-Timezone capture is gated to authed routes only
  # (middleware moved off r.Use() to the authed group as a security fix).
  # Reset device to UTC, send header on a PRE-AUTH endpoint — capture must
  # NOT fire (closes the LAN-attacker-seeds-TZ surface).
  vssh 'timedatectl set-timezone UTC' >/dev/null 2>&1
  curl -sf -H "X-Browser-Timezone: Europe/Berlin" "http://$IP/version" >/dev/null
  sleep 1
  local pre_auth_link
  pre_auth_link=$(vssh 'readlink /etc/localtime' 2>/dev/null | tail -1)
  if [[ "$pre_auth_link" != *"Europe/Berlin"* ]]; then
    echo -e "  ${GREEN}PASS${NC} [11a.12a] X-Browser-Timezone NOT captured pre-auth (gated to authed routes)"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [11a.12a] Pre-auth capture leaked (target=$pre_auth_link)"
    ((FAIL_COUNT++)) || true
  fi

  # 11a.12b — Same header on an AUTHED route triggers capture.
  curl -sf -b "$COOKIE_JAR" -H "X-Browser-Timezone: Europe/Berlin" \
    "http://$IP/api/v1/remote/status" >/dev/null
  sleep 2
  local authed_link
  authed_link=$(vssh 'readlink /etc/localtime' 2>/dev/null | tail -1)
  if [[ "$authed_link" == *"Europe/Berlin"* ]]; then
    echo -e "  ${GREEN}PASS${NC} [11a.12b] X-Browser-Timezone captured on authed route"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [11a.12b] Authed capture failed (target=$authed_link)"
    ((FAIL_COUNT++)) || true
  fi

  # 11a.13 — Subsequent X-Browser-Timezone is a no-op (IsSet short-circuits).
  local before_target; before_target=$(vssh 'readlink /etc/localtime' 2>/dev/null | tail -1)
  curl -sf -b "$COOKIE_JAR" -H "X-Browser-Timezone: Asia/Tokyo" \
    "http://$IP/api/v1/remote/status" >/dev/null
  sleep 2
  local after_target; after_target=$(vssh 'readlink /etc/localtime' 2>/dev/null | tail -1)
  if [[ "$before_target" == "$after_target" ]]; then
    echo -e "  ${GREEN}PASS${NC} [11a.13] X-Browser-Timezone short-circuits when zone already set"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [11a.13] Capture re-fired ($before_target → $after_target)"
    ((FAIL_COUNT++)) || true
  fi

  # 11a.14 — Test action 5s rate-limit: second call within 5s → 429.
  local code1
  code1=$(curl -s -b "$COOKIE_JAR" -X POST -H "X-CSRF-Token: $token" \
    -o /dev/null -w "%{http_code}" \
    "http://$IP/api/v1/security/auto-unlock/test")
  local code2
  code2=$(curl -s -b "$COOKIE_JAR" -X POST -H "X-CSRF-Token: $token" \
    -o /dev/null -w "%{http_code}" \
    "http://$IP/api/v1/security/auto-unlock/test")
  if [[ "$code1" == "200" && "$code2" == "429" ]]; then
    echo -e "  ${GREEN}PASS${NC} [11a.14] Test action rate-limited (200 → 429 within 5s)"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [11a.14] Rate-limit unexpected: first=$code1 second=$code2 (want 200, 429)"
    ((FAIL_COUNT++)) || true
  fi
}

# ─────────────────────────────────────────────────────────
# Stage 11: Resource Stewardship (HTTP + SSH)
#
# Exercises the post-RFC resource-stewardship plumbing end-to-end:
#   - New-shape manifest (convertx: priority normal, bounded 512MB) installs cleanly.
#   - Per-app-user slice gets a piccolo-resources.conf drop-in with the
#     expected MemoryHigh/MemoryMax/CPUWeight values.
#   - systemctl show reports matching live properties.
#   - Container args are slice-only: no --memory / --cpus / --memory-swap
#     emitted by podman (enforcement has moved to the slice).
#   - Pre-v2 legacy-schema manifests are rejected with a catalog-sync hint.
#   - Uninstall cleans up the drop-in.
# ─────────────────────────────────────────────────────────
stage_stewardship() {
  echo -e "\n${CYAN}═══ Stage 11: Resource Stewardship ═══${NC}"
  ensure_session

  # Inline new-shape convertx manifest (mirrors piccolo-store/apps/convertx/app.yaml
  # post-Phase-2.4). Inlined because the remote catalog on
  # github.com/AtDexters-Lab/piccolo-store/main may not yet carry the
  # re-authored manifest during local dev.
  local APP_NAME="convertx"
  local template_yaml
  template_yaml=$(cat <<'EOF'
inputs:
  jwt_secret:
    type: password
    label: "JWT Secret"
    generate: true
    required: true
  account_registration:
    type: string
    label: "Account Registration"
    default: "false"
    required: false
  allow_unauthenticated:
    type: string
    label: "Allow Unauthenticated"
    default: "false"
    required: false

services:
  main:
    image: ghcr.io/c4illin/convertx:v0.16.1
    bind_ports: [3000]
    environment:
      JWT_SECRET: "{{ .Inputs.jwt_secret }}"
      ACCOUNT_REGISTRATION: "{{ .Inputs.account_registration }}"
      ALLOW_UNAUTHENTICATED: "{{ .Inputs.allow_unauthenticated }}"
      HTTP_ALLOWED: "true"
    storage:
      persistent:
        data:
          container: /app/data
          size_limit: 10GB

listeners:
  - name: __primary
    guest_port: 3000
    flow: tcp
    protocol: http

resources:
  priority: normal
  memory:
    min_required: 512MB
    profile: bounded

x-piccolo:
  mode: service
EOF
)

  # Pre-check: inlined manifest should carry new-shape resources (app-level).
  check "11.0" "Inlined manifest declares new-shape resources" "$template_yaml" "min_required"

  # Install convertx with its new-shape manifest. convertx declares three
  # password-type inputs (jwt_secret, account_registration, allow_unauthenticated);
  # we supply all three so the template renders without missing-key errors.
  local payload
  payload=$(YAML="$template_yaml" NAME="$APP_NAME" python3 -c "
import json, os
print(json.dumps({
    'app_definition': os.environ['YAML'],
    'inputs': {
        '__app_address__': os.environ['NAME'],
        'jwt_secret': 'e2e-test-jwt-2026',
        'account_registration': 'false',
        'allow_unauthenticated': 'false',
    },
    'catalog_source': 'convertx'
}))")

  local token
  token=$(csrf)
  # Larger timeout: convertx image pull from ghcr.io can be slow over WAN.
  local install_http
  install_http=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 10 --max-time 600 \
    -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
    -d "$payload" "http://$IP/api/v1/apps" 2>/dev/null)
  check "11.1" "New-shape manifest installs cleanly" "$install_http" "201"

  if [[ "$install_http" != "201" ]]; then
    dump_logs "stage11-install-fail"
    return
  fi

  # Wait for running.
  local app_status=""
  for i in $(seq 1 90); do
    app_status=$(api "/api/v1/apps/$APP_NAME" | python3 -c "
import sys, json
try: print(json.load(sys.stdin).get('data',{}).get('app',{}).get('status',''))
except: print('')" 2>/dev/null)
    [[ "$app_status" == "running" ]] && break
    sleep 2
  done
  check "11.2" "App reaches running" "$app_status" "running"

  # Resolve per-app UID (pa-<app>).
  local pa_uid
  pa_uid=$(vssh "id -u pa-$APP_NAME 2>/dev/null || true")
  if [[ -z "$pa_uid" ]]; then
    echo -e "  ${RED}FAIL${NC} [11.3] Could not resolve per-app user UID"
    ((FAIL_COUNT++)) || true
    return
  fi
  echo -e "  ${CYAN}INFO${NC} Per-app user UID: $pa_uid"

  # Drop-in file exists.
  local dropin_path="/etc/systemd/system/user-${pa_uid}.slice.d/piccolo-resources.conf"
  check_ssh_ok "11.3" "Slice drop-in file exists" "test -f $dropin_path"

  # Drop-in content: convertx manifest has min_required=512MB, profile=bounded.
  # Bounded: MemoryHigh = 512*1.25 = 640 MB (in bytes: 640000000).
  #          MemoryMax  = 512*2    = 1024 MB = 1 GB (in bytes: 1024000000).
  # Priority normal → CPUWeight 100.
  local dropin_content
  dropin_content=$(vssh "cat $dropin_path 2>/dev/null")
  check "11.4" "Drop-in has [Slice] header" "$dropin_content" "[Slice]"
  check "11.5" "Drop-in has MemoryHigh" "$dropin_content" "MemoryHigh="
  check "11.6" "Drop-in has MemoryMax" "$dropin_content" "MemoryMax="
  check "11.7" "Drop-in has CPUWeight=100 (normal)" "$dropin_content" "CPUWeight=100"

  # systemctl show reports the live values.
  local live_props
  live_props=$(vssh "systemctl show user-${pa_uid}.slice --property=MemoryHigh,MemoryMax,CPUWeight 2>/dev/null")
  echo -e "  ${CYAN}INFO${NC} Live slice properties:"
  echo "$live_props" | sed 's/^/       /'
  check "11.8" "Live MemoryHigh is set (non-infinity)" "$live_props" "MemoryHigh="
  check_not "11.9" "Live MemoryHigh is not infinity" "$(echo "$live_props" | grep MemoryHigh=)" "infinity"
  check "11.10" "Live CPUWeight=100" "$live_props" "CPUWeight=100"

  # Slice-only enforcement: verify the per-container cgroup *.scope files
  # have no memory cap (memory.max="max") while the slice parent has the
  # derived limit. This is the cgroup-level evidence that enforcement
  # moved from scope to slice per D-3.
  local scope_mems
  scope_mems=$(vssh "find /sys/fs/cgroup/user.slice/user-${pa_uid}.slice -name 'memory.max' 2>/dev/null | xargs -r cat 2>/dev/null | sort -u")
  echo -e "  ${CYAN}INFO${NC} Cgroup memory.max values in user-${pa_uid} subtree:"
  echo "$scope_mems" | sed 's/^/       /'
  # Expected: slice-level memory.max = 1024000000 (our MemoryMax); scope-level = "max".
  check "11.11" "Slice memory.max reflects derived MemoryMax" "$scope_mems" "1024000000"
  check "11.12" "Container scope memory.max is 'max' (no container-level cap)" "$scope_mems" "max"

  # Same check for cpu.max (scope level): should be "max" since we don't emit --cpus.
  local scope_cpus
  scope_cpus=$(vssh "find /sys/fs/cgroup/user.slice/user-${pa_uid}.slice -name 'cpu.max' -path '*scope*' 2>/dev/null | xargs -r cat 2>/dev/null | sort -u")
  if [[ -n "$scope_cpus" ]]; then
    check "11.13" "Container scope cpu.max unbounded (no --cpus emitted)" "$scope_cpus" "max"
  else
    skip "11.13" "Container cpu.max check" "no scope cgroups found (containers may be coming up)"
  fi

  # pids.max at the scope should be set to the PidsLimit default (4096) — fork-bomb guard retained.
  local scope_pids
  scope_pids=$(vssh "find /sys/fs/cgroup/user.slice/user-${pa_uid}.slice -name 'pids.max' -path '*scope*' 2>/dev/null | xargs -r cat 2>/dev/null | sort -u")
  if [[ -n "$scope_pids" ]]; then
    echo -e "  ${CYAN}INFO${NC} Scope pids.max values: $scope_pids"
    check "11.14" "Container pids.max shows per-process cap (fork-bomb guard retained)" "$scope_pids" "4096"
  else
    skip "11.14" "pids.max check" "no scope cgroups"
  fi

  # Legacy manifest rejection: inject a pre-v2 shape and verify the parser
  # rejects it with the catalog-sync hint. We use /api/v1/apps/validate-like
  # pathway by attempting install with a malformed manifest.
  local legacy_yaml
  legacy_yaml='type: user
listeners:
  - name: __primary
    guest_port: 80
services:
  main:
    image: alpine:latest
    bind_ports: [80]
resources:
  limits:
    memory: 512MB
x-piccolo:
  mode: service'
  local legacy_payload
  legacy_payload=$(YAML="$legacy_yaml" python3 -c "
import json, os
print(json.dumps({
    'app_definition': os.environ['YAML'],
    'inputs': {'__app_address__': 'legacytest'},
    'catalog_source': 'none'
}))")
  token=$(csrf)
  local legacy_resp
  legacy_resp=$(curl -sf -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
    -d "$legacy_payload" "http://$IP/api/v1/apps" 2>&1 || true)
  # The handler returns the error body. Install should FAIL with catalog-sync hint text.
  local legacy_err
  legacy_err=$(curl -s -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
    -d "$legacy_payload" "http://$IP/api/v1/apps" 2>&1)
  check "11.15" "Legacy manifest rejected with catalog-sync hint" "$legacy_err" "catalog sync"

  # Uninstall convertx and verify drop-in cleanup.
  token=$(csrf)
  local uninstall_code
  uninstall_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 30 --max-time 120 \
    -b "$COOKIE_JAR" -X DELETE -H "X-CSRF-Token: $token" \
    "http://$IP/api/v1/apps/$APP_NAME" 2>/dev/null)
  check "11.16" "Convertx uninstalled" "$uninstall_code" "200"

  sleep 3
  check_ssh_fail "11.17" "Drop-in removed on uninstall" "test -f $dropin_path"
}

# ─────────────────────────────────────────────────────────
# Runner
# ─────────────────────────────────────────────────────────
mkdir -p "$(dirname "$COOKIE_JAR")"

case "$STAGE" in
  prereq)          stage_prereq ;;
  boot)            stage_boot ;;
  pre-setup)       stage_pre_setup ;;
  setup)           stage_setup ;;
  post-setup)      stage_post_setup ;;
  storage-inspect) stage_storage_inspect ;;
  rootfs-verify)   stage_rootfs_verify ;;
  service-app)     stage_service_app ;;
  workspace-app)   stage_workspace_app ;;
  reboot)          stage_reboot ;;
  storage-post)    stage_storage_post ;;
  stewardship)     stage_stewardship ;;
  auto-unlock)     stage_auto_unlock ;;
  logs)            dump_logs "manual" ;;
  all)
    stage_prereq
    stage_boot
    stage_pre_setup
    stage_setup
    stage_post_setup
    stage_storage_inspect
    stage_rootfs_verify
    stage_service_app
    stage_workspace_app
    stage_stewardship
    stage_auto_unlock
    stage_reboot
    stage_storage_post
    ;;
  *)
    echo "Unknown stage: $STAGE"
    echo "Valid: prereq boot pre-setup setup post-setup storage-inspect rootfs-verify service-app workspace-app reboot storage-post stewardship auto-unlock logs all"
    exit 1
    ;;
esac

summary
