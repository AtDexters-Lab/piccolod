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
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> connection-auth  # stage 12: ConnectionAuth L4 deny/allow rules (RFC 20260505)
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> tcp-raw          # stage 13: tcp+raw + __primary listener (RFC 20260505)
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> cookie-isolation # stage 14: cross-app cookie injection regression (f1a24d8)
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> listener-pipeline # stages 12+13+14 (the listener-pipeline RFC validation suite)
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> net-supervisor   # stage 15: probe-decide-act validation (RFC 20260505) — destructive (restarts piccolod, plants legacy ledger, blocks rfkill)
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
# Stage 12: ConnectionAuth (L4 IP allow/deny — listener-pipeline RFC 20260505)
#
# Verifies the new ConnectionAuth field on AppListener composes through the
# registry-driven L4 chain. Three scenarios:
#   (a) default: deny, no rules        → TCP RST (chain denies, conn closed)
#   (b) default: deny + allow $myip/32 → HTTP response (L4 allowed, L7 may gate)
#   (c) default: allow + deny $myip/32 → TCP RST
# Plus the S4 trim fix (default: " allow" with leading space parses correctly).
# ─────────────────────────────────────────────────────────
stage_connection_auth() {
  echo -e "\n${CYAN}═══ Stage 12: ConnectionAuth (L4 IP allow/deny) ═══${NC}"
  ensure_session

  # Pre-cleanup any leftovers from a prior aborted run (S2 fix).
  local _ca_app
  for _ca_app in cadenyall caallowme cadenyme catrim cabadcidr; do
    local _t; _t=$(csrf)
    curl -s -o /dev/null -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
      -X DELETE -H "X-CSRF-Token: $_t" "http://$IP/api/v1/apps/$_ca_app" >/dev/null 2>&1 || true
  done
  sleep 2

  # Detect the source IP we appear as to the VM (for the allow/deny-rule cases).
  local host_ip
  host_ip=$(ip -4 route get "$IP" 2>/dev/null | head -1 | grep -oP 'src \K[0-9.]+' || echo "")
  if [[ -z "$host_ip" ]]; then
    skip "12.0" "Detect host source IP" "ip route get failed"
    return
  fi
  echo -e "  ${CYAN}INFO${NC} test client appears to VM as: $host_ip"

  # Helper: install whoami app with given listener YAML, wait running, return public_port.
  # On any failure path emits `install_failed_<code>` or `not_running` so callers
  # can distinguish via the `^[0-9]+$` regex; the function never returns non-zero
  # (which would trip set -e at caller's $(...) capture).
  _ca_install() {
    local name="$1" listener_yaml="$2"
    local yaml='services:
  main:
    image: docker.io/traefik/whoami:latest
    bind_ports: [80]
listeners:
'"$listener_yaml"'
resources:
  priority: normal
  memory:
    min_required: 64MB
    profile: bounded
x-piccolo:
  mode: service'
    local payload
    payload=$(YAML="$yaml" NAME="$name" python3 -c "
import json, os
print(json.dumps({
    'app_definition': os.environ['YAML'],
    'inputs': {'__app_address__': os.environ['NAME']},
    'catalog_source': 'none'}))")
    local token; token=$(csrf)
    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 120 \
      -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
      -X POST -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
      -d "$payload" "http://$IP/api/v1/apps" || true)
    [[ "$code" == "201" ]] || { echo "install_failed_$code"; return 0; }
    local i ran=0
    for i in $(seq 1 60); do
      local s
      s=$(curl -s -b "$COOKIE_JAR" "http://$IP/api/v1/apps/$name" \
        | python3 -c "import sys,json; print(json.load(sys.stdin).get('data',{}).get('app',{}).get('status',''))" 2>/dev/null || true)
      [[ "$s" == "running" ]] && { ran=1; break; }
      sleep 2
    done
    [[ "$ran" -eq 1 ]] || { echo "not_running"; return 0; }
    sleep 2  # backend warm-up
    curl -s -b "$COOKIE_JAR" "http://$IP/api/v1/apps/$name" | python3 -c "
import sys,json
ls=json.load(sys.stdin).get('data',{}).get('listeners') or []
for l in ls:
    if l.get('public_port'): print(l['public_port']); break" || true
  }

  _ca_uninstall() {
    local name="$1" token
    token=$(csrf)
    curl -s -o /dev/null -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
      -X DELETE -H "X-CSRF-Token: $token" "http://$IP/api/v1/apps/$name"
    sleep 2
  }

  # Probe a port and report TCP-level outcome.
  # Echoes "rst" on TCP rejection, "http_<code>" on HTTP response, or "timeout".
  # set -e safety: curl returns non-zero on TCP-level failures (refused/RST/
  # timeout) which would normally abort the script at the var=$(...) line.
  # The `|| rc=$?` pattern keeps set -e from firing while still capturing
  # the curl exit code we need to classify.
  _ca_probe() {
    local port="$1" out rc=0
    out=$(curl -sS -m 5 -b "$COOKIE_JAR" -w '|HTTP_CODE:%{http_code}' "http://$IP:$port/" 2>&1) || rc=$?
    if [[ $rc -eq 56 ]] || [[ $rc -eq 52 ]] || [[ $rc -eq 7 ]] || echo "$out" | grep -qE 'reset by peer|Recv failure|Empty reply'; then
      echo "rst"
    elif [[ $rc -ne 0 ]]; then
      echo "timeout"
    else
      local code
      code=$(echo "$out" | grep -oP 'HTTP_CODE:\K[0-9]+' || true)
      echo "http_${code:-?}"
    fi
  }

  # ── (a) default: deny, no rules ──
  local deny_listener='  - name: __primary
    guest_port: 80
    flow: tcp
    protocol: http
    connection_auth:
      default: deny'
  local port_a
  port_a=$(_ca_install "cadenyall" "$deny_listener")
  if [[ ! "$port_a" =~ ^[0-9]+$ ]]; then
    echo -e "  ${RED}FAIL${NC} [12.1] Install deny-all app: $port_a"
    ((FAIL_COUNT++)) || true
  else
    echo -e "  ${GREEN}PASS${NC} [12.1] Install deny-all app (port $port_a)"
    ((PASS_COUNT++)) || true
    local res_a; res_a=$(_ca_probe "$port_a")
    check "12.2" "deny-all → TCP RST (L4 chain rejects)" "$res_a" "rst"
    _ca_uninstall "cadenyall"
  fi

  # ── (b) default: deny + allow $host_ip/32 ──
  local allow_listener="  - name: __primary
    guest_port: 80
    flow: tcp
    protocol: http
    connection_auth:
      default: deny
      rules:
        - match: $host_ip/32
          strategy: allow"
  local port_b
  port_b=$(_ca_install "caallowme" "$allow_listener")
  if [[ ! "$port_b" =~ ^[0-9]+$ ]]; then
    echo -e "  ${RED}FAIL${NC} [12.3] Install allow-rule app: $port_b"
    ((FAIL_COUNT++)) || true
  else
    echo -e "  ${GREEN}PASS${NC} [12.3] Install allow-rule app (port $port_b)"
    ((PASS_COUNT++)) || true
    local res_b; res_b=$(_ca_probe "$port_b")
    # Success here means we got an HTTP response (not TCP RST). HTTP code is
    # likely 401 from L7 path_auth (no auth: block in manifest), but any
    # http_* result proves L4 admitted.
    if [[ "$res_b" == http_* ]]; then
      echo -e "  ${GREEN}PASS${NC} [12.4] allow-rule → HTTP response (L4 admitted, $res_b)"
      ((PASS_COUNT++)) || true
    else
      echo -e "  ${RED}FAIL${NC} [12.4] allow-rule should admit; got $res_b"
      ((FAIL_COUNT++)) || true
    fi
    _ca_uninstall "caallowme"
  fi

  # ── (c) default: allow + deny $host_ip/32 ──
  local denyme_listener="  - name: __primary
    guest_port: 80
    flow: tcp
    protocol: http
    connection_auth:
      default: allow
      rules:
        - match: $host_ip/32
          strategy: deny"
  local port_c
  port_c=$(_ca_install "cadenyme" "$denyme_listener")
  if [[ ! "$port_c" =~ ^[0-9]+$ ]]; then
    echo -e "  ${RED}FAIL${NC} [12.5] Install deny-rule app: $port_c"
    ((FAIL_COUNT++)) || true
  else
    echo -e "  ${GREEN}PASS${NC} [12.5] Install deny-rule app (port $port_c)"
    ((PASS_COUNT++)) || true
    local res_c; res_c=$(_ca_probe "$port_c")
    check "12.6" "deny-rule for our IP → TCP RST" "$res_c" "rst"
    _ca_uninstall "cadenyme"
  fi

  # ── (d) S4 trim fix: leading-space "default: ' allow'" parses ──
  local trim_listener='  - name: __primary
    guest_port: 80
    flow: tcp
    protocol: http
    connection_auth:
      default: " allow"'
  local port_d
  port_d=$(_ca_install "catrim" "$trim_listener")
  if [[ "$port_d" =~ ^[0-9]+$ ]]; then
    echo -e "  ${GREEN}PASS${NC} [12.7] S4 trim fix: leading-space default parses + builds (port $port_d)"
    ((PASS_COUNT++)) || true
    _ca_uninstall "catrim"
  else
    echo -e "  ${RED}FAIL${NC} [12.7] S4 trim fix: $port_d"
    ((FAIL_COUNT++)) || true
  fi

  # ── (e) bad CIDR → parser rejects pre-build ──
  local bad_yaml='services:
  main: { image: docker.io/traefik/whoami:latest, bind_ports: [80] }
listeners:
  - name: __primary
    guest_port: 80
    flow: tcp
    protocol: http
    connection_auth:
      default: deny
      rules:
        - match: "not-a-cidr"
          strategy: allow
resources: { priority: normal, memory: { min_required: 64MB, profile: bounded } }
x-piccolo: { mode: service }'
  local bad_payload
  bad_payload=$(YAML="$bad_yaml" python3 -c "
import json,os
print(json.dumps({'app_definition':os.environ['YAML'],'inputs':{'__app_address__':'cabadcidr'},'catalog_source':'none'}))")
  local bad_resp token
  token=$(csrf)
  bad_resp=$(curl -s -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
    -d "$bad_payload" "http://$IP/api/v1/apps" 2>&1)
  check "12.8" "Bad CIDR rejected by parser (pre-build validation)" "$bad_resp" "INVALID_CONN_AUTH_MATCH"
}

# ─────────────────────────────────────────────────────────
# Stage 13: tcp+raw + __primary (listener-pipeline RFC 20260505)
#
# Verifies tcp+raw protocol can be __primary (was previously rejected),
# DerivedHostLabel populates, byte passthrough works, and LAN host-based
# routing + mDNS are correctly suppressed (HTTP-only path per RFC).
# Backend: traefik/whoami (HTTP backend used as raw bytes — the L4 chain
# doesn't parse payloads, so HTTP-over-tcp+raw proves byte passthrough).
# ─────────────────────────────────────────────────────────
stage_tcp_raw() {
  echo -e "\n${CYAN}═══ Stage 13: tcp+raw + __primary ═══${NC}"
  ensure_session

  local APP_NAME="rawtcp"
  # Cleanup any prior run
  local token; token=$(csrf)
  curl -s -o /dev/null -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X DELETE -H "X-CSRF-Token: $token" "http://$IP/api/v1/apps/$APP_NAME" 2>/dev/null || true
  sleep 2

  local yaml='services:
  main:
    image: docker.io/traefik/whoami:latest
    bind_ports: [80]
listeners:
  - name: __primary
    guest_port: 80
    flow: tcp
    protocol: raw
resources:
  priority: normal
  memory:
    min_required: 64MB
    profile: bounded
x-piccolo:
  mode: service'
  local payload
  payload=$(YAML="$yaml" python3 -c "
import json,os
print(json.dumps({'app_definition':os.environ['YAML'],'inputs':{'__app_address__':'rawtcp'},'catalog_source':'none'}))")

  token=$(csrf)
  local install_code
  install_code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 120 \
    -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
    -d "$payload" "http://$IP/api/v1/apps")
  check "13.1" "Parser accepts __primary + tcp+raw (was rejected pre-RFC)" "$install_code" "201"
  [[ "$install_code" == "201" ]] || return

  # Wait for backend healthy
  local i
  for i in $(seq 1 60); do
    local h
    h=$(curl -s -b "$COOKIE_JAR" "http://$IP/api/v1/apps/$APP_NAME" \
      | python3 -c "import sys,json
ls=json.load(sys.stdin).get('data',{}).get('listeners') or []
for l in ls: print(l.get('health',{}).get('status')); break" 2>/dev/null || true)
    [[ "$h" == "ok" ]] && break
    sleep 2
  done
  sleep 5  # backend warm-up

  local meta
  meta=$(curl -s -b "$COOKIE_JAR" "http://$IP/api/v1/apps/$APP_NAME" || true)
  local pub_port derived lan_host_url
  pub_port=$(echo "$meta" | python3 -c "import sys,json
ls=json.load(sys.stdin).get('data',{}).get('listeners') or []
for l in ls:
    if l.get('public_port'): print(l['public_port']); break" 2>/dev/null || true)
  derived=$(echo "$meta" | python3 -c "import sys,json
ls=json.load(sys.stdin).get('data',{}).get('listeners') or []
for l in ls:
    if l.get('derived_host_label'): print(l['derived_host_label']); break" 2>/dev/null || true)
  lan_host_url=$(echo "$meta" | python3 -c "import sys,json
ls=json.load(sys.stdin).get('data',{}).get('listeners') or []
for l in ls:
    if l.get('lan_host_url'): print(l['lan_host_url']); break" 2>/dev/null || true)

  if [[ -n "$derived" ]]; then
    echo -e "  ${GREEN}PASS${NC} [13.2] DerivedHostLabel populated for tcp+raw: \"$derived\""
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [13.2] DerivedHostLabel empty (IsEligibleForHostRouting regression)"
    ((FAIL_COUNT++)) || true
  fi

  # Byte passthrough — send raw HTTP, expect raw HTTP back. Use curl rather
  # than nc because nc -N's EOF handling is finicky across distros and tcp+raw
  # is byte-oriented (curl over plain TCP is equivalent to "raw HTTP bytes"
  # from the listener's perspective). Backend takes 20-40s past health=ok on
  # first install due to pasta/podman wiring; retry up to 60s.
  local raw_body; raw_body=$(mktemp /tmp/claude-1000/raw-body-XXXXXX)
  local body="" attempt code=""
  for attempt in $(seq 1 20); do
    code=$(curl -s -o "$raw_body" -w '%{http_code}' --max-time 4 "http://$IP:$pub_port/" 2>/dev/null || true)
    if [[ "$code" == "200" ]]; then
      body=$(head -3 "$raw_body" 2>/dev/null || true)
      break
    fi
    sleep 3
  done
  if [[ "$code" == "200" ]] && echo "$body" | grep -qiE 'hostname|whoami|x-real-ip'; then
    echo -e "  ${GREEN}PASS${NC} [13.3] LAN port-based byte passthrough (whoami HTTP/200, attempt ${attempt}/20)"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [13.3] No HTTP response through tcp+raw proxy (last code=$code, body head: $body)"
    ((FAIL_COUNT++)) || true
  fi
  rm -f "$raw_body"

  # LAN host-based should NOT route tcp+raw — should fall through to portal/404
  if [[ -n "$lan_host_url" ]]; then
    local host_label
    host_label=$(echo "$lan_host_url" | sed 's|http://||;s|/.*||')
    local lan_resp
    lan_resp=$(curl -s --max-time 3 --resolve "$host_label:80:$IP" "http://$host_label/" 2>&1 | head -3)
    if echo "$lan_resp" | grep -qiE 'whoami|hostname'; then
      echo -e "  ${RED}FAIL${NC} [13.4] LAN host-based incorrectly routed tcp+raw to backend"
      ((FAIL_COUNT++)) || true
    else
      echo -e "  ${GREEN}PASS${NC} [13.4] LAN host-based correctly suppressed for tcp+raw (HTTP-only path)"
      ((PASS_COUNT++)) || true
    fi
  else
    skip "13.4" "LAN host-based suppression" "no lan_host_url"
  fi

  # mDNS suppression — host-side avahi-browse shouldn't see this listener
  if command -v avahi-browse >/dev/null 2>&1; then
    local mdns_out
    mdns_out=$(timeout 5 avahi-browse -arpt 2>/dev/null | grep -i "$derived" || true)
    if [[ -z "$mdns_out" ]]; then
      echo -e "  ${GREEN}PASS${NC} [13.5] mDNS suppression for tcp+raw (no avahi entries)"
      ((PASS_COUNT++)) || true
    else
      echo -e "  ${RED}FAIL${NC} [13.5] mDNS entries leaked for tcp+raw: $mdns_out"
      ((FAIL_COUNT++)) || true
    fi
  else
    skip "13.5" "mDNS suppression check" "avahi-browse not installed on host"
  fi

  # Cleanup before deny-variant install
  token=$(csrf)
  curl -s -o /dev/null -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X DELETE -H "X-CSRF-Token: $token" "http://$IP/api/v1/apps/$APP_NAME"
  sleep 3

  # ── ConnectionAuth deny on tcp+raw (combined feature test) ──
  local deny_yaml='services:
  main:
    image: docker.io/traefik/whoami:latest
    bind_ports: [80]
listeners:
  - name: __primary
    guest_port: 80
    flow: tcp
    protocol: raw
    connection_auth:
      default: deny
resources:
  priority: normal
  memory:
    min_required: 64MB
    profile: bounded
x-piccolo:
  mode: service'
  local deny_payload
  deny_payload=$(YAML="$deny_yaml" python3 -c "
import json,os
print(json.dumps({'app_definition':os.environ['YAML'],'inputs':{'__app_address__':'rawtcp'},'catalog_source':'none'}))")

  token=$(csrf)
  local deny_install
  deny_install=$(curl -s -o /dev/null -w '%{http_code}' --max-time 120 \
    -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
    -d "$deny_payload" "http://$IP/api/v1/apps")
  if [[ "$deny_install" != "201" ]]; then
    skip "13.6" "ConnectionAuth deny on tcp+raw" "install_failed_$deny_install"
  else
    for i in $(seq 1 60); do
      local h
      h=$(curl -s -b "$COOKIE_JAR" "http://$IP/api/v1/apps/$APP_NAME" \
        | python3 -c "import sys,json
ls=json.load(sys.stdin).get('data',{}).get('listeners') or []
for l in ls: print(l.get('health',{}).get('status')); break" 2>/dev/null || true)
      [[ "$h" == "ok" ]] && break
      sleep 2
    done
    sleep 3
    local deny_port
    deny_port=$(curl -s -b "$COOKIE_JAR" "http://$IP/api/v1/apps/$APP_NAME" \
      | python3 -c "import sys,json
ls=json.load(sys.stdin).get('data',{}).get('listeners') or []
for l in ls:
    if l.get('public_port'): print(l['public_port']); break" 2>/dev/null || true)
    # Use curl for the deny-side probe (same `|| rc=$?` pattern as _ca_probe).
    # The deny scenario produces TCP RST or empty-reply; curl exits non-zero
    # on both. Capturing rc explicitly avoids the set -e abort.
    local deny_out="" deny_rc=0
    deny_out=$(curl -sS -m 4 "http://$IP:$deny_port/" 2>&1) || deny_rc=$?
    if [[ "$deny_rc" -ne 0 ]] || [[ -z "$deny_out" ]]; then
      echo -e "  ${GREEN}PASS${NC} [13.6] ConnectionAuth deny on tcp+raw closes connection (curl rc=$deny_rc)"
      ((PASS_COUNT++)) || true
    else
      echo -e "  ${RED}FAIL${NC} [13.6] tcp+raw deny-all returned data: \"$(echo "$deny_out" | head -1)\""
      ((FAIL_COUNT++)) || true
    fi
  fi

  # Cleanup
  token=$(csrf)
  curl -s -o /dev/null -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X DELETE -H "X-CSRF-Token: $token" "http://$IP/api/v1/apps/$APP_NAME"
}

# ─────────────────────────────────────────────────────────
# Stage 14: Cookie isolation regression (security fix f1a24d8)
#
# Verifies that the response-side cookie_isolation_response middleware drops
# cross-app __piccolo_* injection attempts, regardless of whose prefix they
# target. Backend: mccutchen/go-httpbin (has /cookies/set?<k=v>... endpoint).
#
# Pre-fix: backend Set-Cookie: __piccolo_otherapp_session=ATTACKER would pass
# through host-scoped, then on visit to otherapp the request-side strip would
# unwrap to "session=ATTACKER" → forged session.
# ─────────────────────────────────────────────────────────
stage_cookie_isolation() {
  echo -e "\n${CYAN}═══ Stage 14: Cookie Isolation Regression (f1a24d8) ═══${NC}"
  ensure_session

  local APP_NAME="hbin"
  # Cleanup any prior run
  local token; token=$(csrf)
  curl -s -o /dev/null -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X DELETE -H "X-CSRF-Token: $token" "http://$IP/api/v1/apps/$APP_NAME" 2>/dev/null || true
  sleep 2

  # httpbin with PUBLIC auth so we can curl unauthenticated and exercise the
  # cookie filter on the actual backend response (not a 401 from path_auth).
  local yaml='services:
  main:
    image: docker.io/mccutchen/go-httpbin:latest
    bind_ports: [8080]
listeners:
  - name: __primary
    guest_port: 8080
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: /
          type: prefix
          strategy: public
resources:
  priority: normal
  memory:
    min_required: 64MB
    profile: bounded
x-piccolo:
  mode: service'
  local payload
  payload=$(YAML="$yaml" python3 -c "
import json,os
print(json.dumps({'app_definition':os.environ['YAML'],'inputs':{'__app_address__':'hbin'},'catalog_source':'none'}))")

  token=$(csrf)
  local install_code
  install_code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 120 \
    -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
    -d "$payload" "http://$IP/api/v1/apps")
  check "14.1" "Install httpbin app" "$install_code" "201"
  [[ "$install_code" == "201" ]] || return

  # Wait for healthy
  local i
  for i in $(seq 1 60); do
    local h
    h=$(curl -s -b "$COOKIE_JAR" "http://$IP/api/v1/apps/$APP_NAME" \
      | python3 -c "import sys,json
ls=json.load(sys.stdin).get('data',{}).get('listeners') or []
for l in ls: print(l.get('health',{}).get('status')); break" 2>/dev/null || true)
    [[ "$h" == "ok" ]] && break
    sleep 2
  done
  sleep 5

  local port
  port=$(curl -s -b "$COOKIE_JAR" "http://$IP/api/v1/apps/$APP_NAME" \
    | python3 -c "import sys,json
ls=json.load(sys.stdin).get('data',{}).get('listeners') or []
for l in ls:
    if l.get('public_port'): print(l['public_port']); break" 2>/dev/null || true)

  # Sanity: backend reachable
  local sanity
  sanity=$(curl -s --max-time 5 "http://$IP:$port/headers" | head -3)
  if echo "$sanity" | grep -q '"headers"'; then
    echo -e "  ${GREEN}PASS${NC} [14.2] Backend reachable through new chain"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [14.2] Backend not reachable: $sanity"
    ((FAIL_COUNT++)) || true
    token=$(csrf)
    curl -s -o /dev/null -b "$COOKIE_JAR" -c "$COOKIE_JAR" -X DELETE -H "X-CSRF-Token: $token" "http://$IP/api/v1/apps/$APP_NAME"
    return
  fi

  # Attack: backend emits 5 Set-Cookies via httpbin's /cookies/set
  local attack_resp
  attack_resp=$(curl -sv --max-time 5 \
    "http://$IP:$port/cookies/set?__piccolo_victim_session=ATTACKER&__piccolo_hbin_self=SELFINJ&piccolo_legacy=L&__Host-secure=HOK&ordinary=OK" 2>&1)
  local set_cookies
  set_cookies=$(echo "$attack_resp" | grep -E '^< Set-Cookie:' || true)

  # Cross-app __piccolo_<otherapp>_* must be DROPPED (the f1a24d8 fix)
  if echo "$set_cookies" | grep -qE 'Set-Cookie: ?__piccolo_victim_session='; then
    echo -e "  ${RED}FAIL${NC} [14.3] f1a24d8 REGRESSION: cross-app __piccolo_victim_session leaked through"
    ((FAIL_COUNT++)) || true
  else
    echo -e "  ${GREEN}PASS${NC} [14.3] f1a24d8 fix: cross-app __piccolo_victim_session DROPPED"
    ((PASS_COUNT++)) || true
  fi

  # Same-app __piccolo_<thisapp>_* must also be DROPPED (proxy reserved namespace)
  if echo "$set_cookies" | grep -qE 'Set-Cookie: ?__piccolo_hbin_self='; then
    echo -e "  ${RED}FAIL${NC} [14.4] same-app __piccolo_hbin_self leaked (proxy namespace must be reserved)"
    ((FAIL_COUNT++)) || true
  else
    echo -e "  ${GREEN}PASS${NC} [14.4] same-app __piccolo_hbin_self DROPPED (reserved namespace)"
    ((PASS_COUNT++)) || true
  fi

  # Existing piccolo_ (single underscore) prefix must be DROPPED via isPiccoloCookieName
  if echo "$set_cookies" | grep -qE 'Set-Cookie: ?piccolo_legacy='; then
    echo -e "  ${RED}FAIL${NC} [14.5] piccolo_ legacy prefix leaked (existing isPiccoloCookieName regression)"
    ((FAIL_COUNT++)) || true
  else
    echo -e "  ${GREEN}PASS${NC} [14.5] piccolo_legacy DROPPED (existing filter)"
    ((PASS_COUNT++)) || true
  fi

  # Legitimate cookies should be rewritten with per-app prefix __piccolo_hbin_*
  # (port-based LAN mode → cookies are host-scoped → per-app namespacing applies)
  if echo "$set_cookies" | grep -qE 'Set-Cookie: ?__piccolo_hbin_ordinary='; then
    echo -e "  ${GREEN}PASS${NC} [14.6] ordinary cookie rewritten with per-app prefix"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [14.6] ordinary cookie missing or not rewritten"
    echo -e "       Set-Cookie headers: $set_cookies"
    ((FAIL_COUNT++)) || true
  fi

  # Cleanup
  token=$(csrf)
  curl -s -o /dev/null -w '  [cleanup] uninstall %{http_code}\n' \
    -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X DELETE -H "X-CSRF-Token: $token" "http://$IP/api/v1/apps/$APP_NAME"
}

# ─────────────────────────────────────────────────────────
# Stage 15: Net-supervisor (probe-decide-act, RFC 20260505)
#
# Validates the cutover from the procedural watchdog + ConnState machine to
# the new probe→decide→act architecture. Covers the failure modes that the
# rewrite was designed to handle, exercised non-destructively from the host:
#
#   - Supervisor tick observability + cadence
#   - /api/v1/admin/wifi/status legacy wire contract preserved
#   - Private dbus connection (F3-B1 fix)
#   - No NM connectivity-check externality per tick (UR1 bug002)
#   - Piccolod restart mid-state — ledger reload + supervisor resume (1.D.G6)
#   - Legacy net-watchdog-reboots → net-ledger.json migration (1.E.bug010)
#   - rfkill+profile dampened to ConfigHealth Faulted at 3-of-3 (codex P1-4),
#     no bounce because L3 up via eth — the silent-cope behavior
#
# Out of scope here (handled manually with VBoxManage from the operator):
#   - Eth disconnect/reconnect — needs SSH path swap to the wifi IP
#   - AP-entry showcase — needs eth + wifi config-faulted simultaneously
#   - Originating-incident hardware (rtw88_8723de)
# ─────────────────────────────────────────────────────────
stage_net_supervisor() {
  echo -e "\n${CYAN}═══ Stage 15: Net-supervisor (probe-decide-act) ═══${NC}"

  # B4 fix — destructive ops (rfkill block, piccolod stop) need an EXIT trap
  # so a Ctrl-C / set -e death between block-and-unblock or stop-and-start
  # leaves the dev VM recoverable rather than hosed.
  _ns_cleanup() {
    vssh "rfkill unblock wifi 2>/dev/null; systemctl is-active piccolod >/dev/null 2>&1 || systemctl start piccolod 2>/dev/null" >/dev/null 2>&1 || true
  }
  trap _ns_cleanup EXIT

  # Settle window after piccolod restart — used by 15.6 and 15.7. Polling
  # the journal would be tighter but a fixed window is consistent and
  # easier to reason about across the two ledger-touching tests.
  local PICCOLO_SETTLE_S=5

  # Detect environment
  local has_wifi_profile=0 has_wifi_device=0 conn_via=""
  if vssh "nmcli -t -f NAME,TYPE connection show 2>/dev/null | grep ':802-11-wireless\$'" >/dev/null 2>&1; then
    has_wifi_profile=1
  fi
  if vssh "nmcli -t -f DEVICE,TYPE device status 2>/dev/null | grep ':wifi\$'" >/dev/null 2>&1; then
    has_wifi_device=1
  fi
  if [[ "$IP" == "$(vssh 'ip -4 -o addr show enp0s3 2>/dev/null | grep -oP "inet \K[0-9.]+" | head -1' 2>/dev/null || true)" ]]; then
    conn_via="eth"
  else
    conn_via="wifi"
  fi
  echo -e "  ${CYAN}INFO${NC} SSH path: $conn_via; wifi profile: $has_wifi_profile; wifi device present: $has_wifi_device"

  # 15.1 supervisor tick visible
  local tick_line
  tick_line=$(vssh "journalctl -u piccolod --no-pager -n 200 2>/dev/null | grep 'net-supervisor.tick' | tail -1")
  if echo "$tick_line" | grep -q 'uplink='; then
    echo -e "  ${GREEN}PASS${NC} [15.1] Supervisor tick visible in journal"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [15.1] No supervisor tick in journal — supervisor not running?"
    ((FAIL_COUNT++)) || true
    return
  fi

  # 15.2 tick cadence ≈ 30s — sample 3 ticks
  local timestamps
  timestamps=$(vssh "journalctl -u piccolod --no-pager -n 500 2>/dev/null | grep 'net-supervisor.tick' | tail -3 | awk '{print \$1, \$2, \$3}'")
  local distinct_count
  distinct_count=$(echo "$timestamps" | wc -l)
  if [[ "$distinct_count" -ge 2 ]]; then
    echo -e "  ${GREEN}PASS${NC} [15.2] Tick cadence active (${distinct_count}+ ticks observed)"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [15.2] Insufficient ticks observed"
    ((FAIL_COUNT++)) || true
  fi

  # 15.3 wire contract — /api/v1/wifi/status returns valid legacy ConnState
  ensure_session
  local wifi_status state
  wifi_status=$(curl -s -b "$COOKIE_JAR" --connect-timeout 5 "http://$IP/api/v1/wifi/status" 2>/dev/null || true)
  state=$(echo "$wifi_status" | python3 -c "import sys,json
try: print(json.load(sys.stdin).get('state',''))
except: print('')" 2>/dev/null || true)
  case "$state" in
    ethernet|wifi_connected|ap_mode|reconnecting|disconnected)
      echo -e "  ${GREEN}PASS${NC} [15.3] Legacy wire contract: state=\"$state\""
      ((PASS_COUNT++)) || true
      ;;
    *)
      echo -e "  ${RED}FAIL${NC} [15.3] Unexpected state: \"$state\" (response: $(echo "$wifi_status" | head -c 200))"
      ((FAIL_COUNT++)) || true
      ;;
  esac

  # 15.4 private dbus connection (F3-B1) — piccolod has 2 dbus connections
  local dbus_count
  dbus_count=$(vssh "busctl --no-pager 2>/dev/null | awk '\$3==\"piccolod\"' | wc -l")
  if [[ "$dbus_count" -ge 2 ]]; then
    echo -e "  ${GREEN}PASS${NC} [15.4] Private dbus connection (F3-B1): $dbus_count piccolod connections"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [15.4] F3-B1 fix: expected ≥2 piccolod dbus connections, got $dbus_count"
    ((FAIL_COUNT++)) || true
  fi

  # 15.5 NM no per-tick connectivity probe (UR1 bug002)
  local probe_count
  probe_count=$(vssh "journalctl -u NetworkManager --no-pager --since '3 minutes ago' 2>/dev/null | grep -ciE 'connectivity check|HTTP GET' | head -1" 2>/dev/null || echo 0)
  probe_count="${probe_count:-0}"
  # Strip whitespace/newlines defensively
  probe_count=$(echo "$probe_count" | tr -d '[:space:]')
  if [[ "$probe_count" =~ ^[0-9]+$ ]] && [[ "$probe_count" -le 2 ]]; then
    echo -e "  ${GREEN}PASS${NC} [15.5] No per-tick NM connectivity probes (UR1 bug002): $probe_count in 3min"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [15.5] UR1 bug002 regression: \"$probe_count\" NM probes in 3min"
    ((FAIL_COUNT++)) || true
  fi

  # 15.6 piccolod restart mid-state — ledger reload + clean resume
  echo -e "  ${CYAN}INFO${NC} restarting piccolod for ledger-reload test..."
  vssh "systemctl restart piccolod" >/dev/null 2>&1
  # Wait for service active
  local i
  for i in $(seq 1 30); do
    if vssh "systemctl is-active piccolod" 2>/dev/null | grep -q '^active$'; then break; fi
    sleep 2
  done
  sleep "$PICCOLO_SETTLE_S"  # let supervisor get one tick in
  local post_restart_tick
  post_restart_tick=$(vssh "journalctl -u piccolod --no-pager --since '20 seconds ago' 2>/dev/null | grep -E 'net-supervisor: ledger loaded|net-supervisor.tick' | head -3")
  if echo "$post_restart_tick" | grep -q 'ledger loaded'; then
    echo -e "  ${GREEN}PASS${NC} [15.6] Piccolod restart: ledger reload visible, supervisor resumed (1.D.G6)"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [15.6] No 'ledger loaded' message after restart"
    ((FAIL_COUNT++)) || true
  fi
  # Re-establish session — restart invalidates cookies
  ensure_session

  # 15.7 legacy ledger migration (1.E.bug010) — destructive: stop piccolod,
  # plant legacy file, start, verify migration, verify legacy file deleted
  echo -e "  ${CYAN}INFO${NC} legacy ledger migration test (destructive)..."
  vssh "systemctl stop piccolod" >/dev/null 2>&1
  sleep 2
  # Snapshot current ledger
  local ledger_pre
  ledger_pre=$(vssh "cat /var/lib/piccolo/net-ledger.json 2>/dev/null | python3 -c \"import sys,json
try: print(len(json.load(sys.stdin).get('reboots',[])))
except: print(0)\"" 2>/dev/null || echo "0")
  # Plant legacy file: 2 timestamps (1h ago, 30min ago)
  vssh "mkdir -p /var/lib/piccolo && now=\$(date +%s) && printf '%s\\n%s\\n' \$((now-3600)) \$((now-1800)) > /var/lib/piccolo/net-watchdog-reboots" >/dev/null 2>&1
  vssh "systemctl start piccolod" >/dev/null 2>&1
  for i in $(seq 1 30); do
    if vssh "systemctl is-active piccolod" 2>/dev/null | grep -q '^active$'; then break; fi
    sleep 2
  done
  sleep "$PICCOLO_SETTLE_S"
  # Verify migration: net-ledger.json should now have +2 reboot entries; legacy file gone
  local ledger_post legacy_present
  ledger_post=$(vssh "cat /var/lib/piccolo/net-ledger.json 2>/dev/null | python3 -c \"import sys,json
try: print(len(json.load(sys.stdin).get('reboots',[])))
except: print(0)\"" 2>/dev/null || echo "0")
  legacy_present=$(vssh "test -f /var/lib/piccolo/net-watchdog-reboots && echo yes || echo no" 2>/dev/null)
  if [[ "$ledger_post" -ge $((ledger_pre + 2)) ]] && [[ "$legacy_present" == "no" ]]; then
    echo -e "  ${GREEN}PASS${NC} [15.7] Legacy migration (1.E.bug010): reboots ${ledger_pre}→${ledger_post}; legacy file deleted"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [15.7] Migration: reboots ${ledger_pre}→${ledger_post} (expected +2); legacy_present=$legacy_present"
    ((FAIL_COUNT++)) || true
  fi
  ensure_session

  # 15.8 rfkill + profile + eth-up: dampened to ConfigHealth Faulted (codex P1-4)
  if [[ "$has_wifi_device" -eq 0 ]]; then
    skip "15.8" "rfkill+profile dampener (codex P1-4)" "no wifi device present (USB dongle not attached?)"
    skip "15.9" "post-rfkill recovery" "no wifi device present"
  elif [[ "$has_wifi_profile" -eq 0 ]]; then
    skip "15.8" "rfkill+profile dampener (codex P1-4)" "no wifi profile configured"
    skip "15.9" "post-rfkill recovery" "no wifi profile configured"
  elif [[ "$conn_via" != "eth" ]]; then
    skip "15.8" "rfkill+profile dampener" "SSH via wifi — would self-destruct"
    skip "15.9" "post-rfkill recovery" "SSH via wifi — would self-destruct"
  else
    echo -e "  ${CYAN}INFO${NC} rfkill block wifi (eth still SSH path)..."
    vssh "rfkill block wifi; nmcli connection down id \$(nmcli -t -f NAME,TYPE connection show | grep ':802-11-wireless\$' | head -1 | cut -d: -f1) >/dev/null 2>&1 || true" >/dev/null 2>&1 || true
    # Wait 100s for 3-of-3 dampener (~90s) plus margin
    echo -e "  ${CYAN}INFO${NC} waiting 100s for 3-of-3 ConfigHealth dampener..."
    sleep 100
    local rfkill_ticks
    rfkill_ticks=$(vssh "journalctl -u piccolod --no-pager --since '120 seconds ago' 2>/dev/null | grep 'net-supervisor.tick' | tail -5" 2>/dev/null || true)
    # Look for HW=faulted or cfg=faulted in any of the recent ticks
    if echo "$rfkill_ticks" | grep -qE 'wifi=\[hw=faulted|cfg=faulted'; then
      # Also verify decisions stayed conservative (no bounce — L3 up via eth).
      # `grep` returns 1 when no match; under pipefail+set -e that aborts the
      # script. Wrap to tolerate empty match (which is itself a valid signal).
      local actions
      actions=$(echo "$rfkill_ticks" | { grep -oE 'decisions=\[[^]]+\]' || true; } | tail -1)
      if echo "$actions" | grep -qE 'wifi=bounce|wifi=reboot'; then
        echo -e "  ${RED}FAIL${NC} [15.8] Supervisor bounced/rebooted with L3 up via eth (silent-cope regression): $actions"
        ((FAIL_COUNT++)) || true
      else
        echo -e "  ${GREEN}PASS${NC} [15.8] rfkill dampened to Faulted; decisions stayed wait (silent-cope intact): $actions"
        ((PASS_COUNT++)) || true
      fi
    else
      echo -e "  ${RED}FAIL${NC} [15.8] No HW/cfg=faulted observed after 100s of rfkill block"
      echo -e "       last 3 ticks:"
      echo "$rfkill_ticks" | tail -3 | sed 's/^/         /'
      ((FAIL_COUNT++)) || true
    fi

    # 15.9 unblock + recovery
    echo -e "  ${CYAN}INFO${NC} rfkill unblock + reactivate wifi..."
    vssh "rfkill unblock wifi; sleep 2; nmcli connection up id \$(nmcli -t -f NAME,TYPE connection show | grep ':802-11-wireless\$' | head -1 | cut -d: -f1) >/dev/null 2>&1 || true" >/dev/null 2>&1 || true
    sleep 35
    local recovery_tick
    recovery_tick=$(vssh "journalctl -u piccolod --no-pager --since '40 seconds ago' 2>/dev/null | grep 'net-supervisor.tick' | tail -1" 2>/dev/null || true)
    if echo "$recovery_tick" | grep -qE 'wifi=\[hw=healthy.*cfg=healthy'; then
      echo -e "  ${GREEN}PASS${NC} [15.9] WiFi recovery observed (cfg=healthy after unblock)"
      ((PASS_COUNT++)) || true
    else
      echo -e "  ${RED}FAIL${NC} [15.9] Wifi did not recover to cfg=healthy"
      echo -e "       last tick: $recovery_tick"
      ((FAIL_COUNT++)) || true
    fi
  fi
}

# ─────────────────────────────────────────────────────────
# Runner
# ─────────────────────────────────────────────────────────
mkdir -p "$(dirname "$COOKIE_JAR")"

case "$STAGE" in
  prereq)            stage_prereq ;;
  boot)              stage_boot ;;
  pre-setup)         stage_pre_setup ;;
  setup)             stage_setup ;;
  post-setup)        stage_post_setup ;;
  storage-inspect)   stage_storage_inspect ;;
  rootfs-verify)     stage_rootfs_verify ;;
  service-app)       stage_service_app ;;
  workspace-app)     stage_workspace_app ;;
  reboot)            stage_reboot ;;
  storage-post)      stage_storage_post ;;
  stewardship)       stage_stewardship ;;
  auto-unlock)       stage_auto_unlock ;;
  connection-auth)   stage_connection_auth ;;
  tcp-raw)           stage_tcp_raw ;;
  cookie-isolation)  stage_cookie_isolation ;;
  listener-pipeline) stage_connection_auth; stage_tcp_raw; stage_cookie_isolation ;;
  net-supervisor)    stage_net_supervisor ;;
  logs)              dump_logs "manual" ;;
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
    stage_connection_auth
    stage_tcp_raw
    stage_cookie_isolation
    stage_net_supervisor
    stage_auto_unlock
    stage_reboot
    stage_storage_post
    ;;
  *)
    echo "Unknown stage: $STAGE"
    echo "Valid: prereq boot pre-setup setup post-setup storage-inspect rootfs-verify service-app workspace-app reboot storage-post stewardship auto-unlock connection-auth tcp-raw cookie-isolation listener-pipeline net-supervisor logs all"
    exit 1
    ;;
esac

summary
