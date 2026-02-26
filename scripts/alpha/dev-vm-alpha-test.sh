#!/usr/bin/env bash
# dev-vm-alpha-test.sh — Test stages for block-native storage on a Tumbleweed dev VM.
#
# Combines HTTP API tests (same as production) with SSH-based storage inspection
# stages for verifying the full block device stack: LVM, DRBD, NBD, LUKS, overlay.
#
# Usage:
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP>                  # run all stages
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> prereq           # stage 0: prerequisites
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> boot             # stage 1: boot & disk prep
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> pre-setup        # stage 2: pre-setup gating
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> setup            # stage 3: first-run setup
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> post-setup       # stage 4: post-setup smoke
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> storage-inspect  # stage 5: SSH storage inspection
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> overlay-verify   # stage 6: zero-FUSE verification
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> service-app      # stage 7: service app lifecycle
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> workspace-app    # stage 8: workspace app lifecycle
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> reboot           # stage 9: reboot & unlock cycle
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> storage-post     # stage 10: post-reboot storage
#   ./scripts/alpha/dev-vm-alpha-test.sh <IP> logs             # download piccolod journal
set -euo pipefail

IP="${1:?Usage: $0 <VM_IP> [stage]}"
STAGE="${2:-all}"
PASS="${PICCOLO_TEST_PASS:-PiccoloE2E-Test-2026!}"
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
post() { curl -sf --connect-timeout 5 -b "$COOKIE_JAR" -c "$COOKIE_JAR" -X POST -H "Content-Type: application/json" -d "$2" "http://$IP$1" 2>/dev/null; }
post_csrf() {
  local token; token=$(csrf)
  curl -sf --connect-timeout 10 -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
    ${2:+-d "$2"} "http://$IP$1" 2>/dev/null
}

ensure_session() {
  local authed
  authed=$(curl -s --connect-timeout 5 -b "$COOKIE_JAR" \
    "http://$IP/api/v1/auth/session" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('authenticated',False))" 2>/dev/null)
  [[ "$authed" == "True" ]] && return 0
  curl -sf --connect-timeout 10 -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" \
    -d "{\"username\":\"admin\",\"password\":\"$PASS\"}" \
    "http://$IP/api/v1/auth/login" >/dev/null 2>&1 || true
  curl -sf --connect-timeout 10 -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" \
    -d "{\"password\":\"$PASS\"}" \
    "http://$IP/api/v1/crypto/unlock" >/dev/null 2>&1 || true
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
  check_ssh_ok "0.6" "overlay module loaded" "lsmod | grep -q overlay || grep -q overlay /proc/filesystems"
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

  # No FUSE storage packages
  check_ssh_fail "0.10" "gocryptfs NOT installed" "rpm -q gocryptfs"
  check_ssh_fail "0.11" "fuse-overlayfs NOT installed" "rpm -q fuse-overlayfs"

  # Binaries
  check_ssh_ok "0.12" "pvcreate available" "which pvcreate"
  check_ssh_ok "0.13" "cryptsetup available" "which cryptsetup"
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
  health=$(api "/api/v1/health/detail")
  check "1.3" "HTTP health OK" "$health" '"HTTP server initialized"'
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

  check_http "4.1" "Apps endpoint accessible" "GET" "/api/v1/apps" "200"

  local session
  session=$(api "/api/v1/auth/session")
  check "4.2" "Session valid" "$session" '"authenticated"'
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
  check_ssh_ok "5.3" "Control plane LUKS loop exists" "test -f /piccolo-core/volumes/control-plane/control-plane.luks"
  check_ssh "5.4" "Control plane mounted as ext4" "mount | grep control-plane" "ext4"

  # No FUSE mounts
  local fuse_count
  fuse_count=$(vssh "grep -c fuse /proc/mounts 2>/dev/null" 2>/dev/null || echo "0")
  if [[ "$fuse_count" == "0" ]]; then
    echo -e "  ${GREEN}PASS${NC} [5.5] Zero FUSE mounts"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [5.5] Found $fuse_count FUSE mounts"
    ((FAIL_COUNT++)) || true
  fi

  # DRBD (informational — may not have resources yet)
  local drbd_status
  drbd_status=$(vssh "drbdadm status 2>/dev/null" 2>/dev/null || echo "no resources")
  echo -e "  ${CYAN}INFO${NC} DRBD status: $(echo "$drbd_status" | head -3)"

  # Raw mount table (informational)
  echo -e "\n  ${CYAN}Overlay mounts:${NC}"
  vssh "grep overlay /proc/mounts 2>/dev/null | head -5" 2>/dev/null | sed 's/^/       /' || true
}

# ─────────────────────────────────────────────────────────
# Stage 6: Overlay Verification (SSH)
# ─────────────────────────────────────────────────────────
stage_overlay_verify() {
  echo -e "\n${CYAN}═══ Stage 6: Zero-FUSE Overlay Verification (SSH) ═══${NC}"

  # No fuse-overlayfs processes
  check_ssh_fail "6.1" "No fuse-overlayfs processes" "pgrep -c fuse-overlayfs"

  # No mount_program in storage configs
  local mount_prog
  mount_prog=$(vssh "grep -r mount_program /etc/containers/ 2>/dev/null || true" 2>/dev/null)
  if [[ -z "$mount_prog" ]]; then
    echo -e "  ${GREEN}PASS${NC} [6.2] No mount_program in container storage configs"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [6.2] Found mount_program references:"
    echo "       $mount_prog"
    ((FAIL_COUNT++)) || true
  fi

  # Kernel overlay support
  check_ssh "6.3" "Kernel overlay in /proc/filesystems" "cat /proc/filesystems" "overlay"
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

  # Uninstall
  token=$(csrf)
  local uninstall_code
  uninstall_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 30 --max-time 120 \
    -b "$COOKIE_JAR" -X DELETE -H "X-CSRF-Token: $token" \
    "http://$IP/api/v1/apps/$APP_NAME" 2>/dev/null)
  check "7.3" "Service app uninstalled" "$uninstall_code" "200"

  sleep 3
  check_http "7.4" "App gone" "GET" "/api/v1/apps/$APP_NAME" "404"
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

  # Verify overlay mount is kernel native (not FUSE) — SSH check after workspace install
  local ws_overlay
  ws_overlay=$(vssh "grep 'workspace.*merged' /proc/mounts 2>/dev/null | head -1" 2>/dev/null || echo "")
  if [[ -n "$ws_overlay" ]]; then
    check "8.3" "Workspace overlay is kernel native" "$ws_overlay" "overlay"
  else
    skip "8.3" "Workspace overlay check" "no workspace mount found"
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
  local unlock_code
  unlock_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 10 \
    -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" \
    -d "{\"password\":\"$PASS\"}" \
    "http://$IP/api/v1/crypto/unlock" 2>/dev/null)
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
  fuse_count=$(vssh "grep -c 'fuse\\.fuse-overlayfs' /proc/mounts 2>/dev/null" 2>/dev/null || echo "0")
  if [[ "$fuse_count" == "0" ]]; then
    echo -e "  ${GREEN}PASS${NC} [10.3] No stale FUSE overlay mounts"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [10.3] Found $fuse_count stale FUSE mounts"
    ((FAIL_COUNT++)) || true
  fi
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
  overlay-verify)  stage_overlay_verify ;;
  service-app)     stage_service_app ;;
  workspace-app)   stage_workspace_app ;;
  reboot)          stage_reboot ;;
  storage-post)    stage_storage_post ;;
  logs)            dump_logs "manual" ;;
  all)
    stage_prereq
    stage_boot
    stage_pre_setup
    stage_setup
    stage_post_setup
    stage_storage_inspect
    stage_overlay_verify
    stage_service_app
    stage_workspace_app
    stage_reboot
    stage_storage_post
    ;;
  *)
    echo "Unknown stage: $STAGE"
    echo "Valid: prereq boot pre-setup setup post-setup storage-inspect overlay-verify service-app workspace-app reboot storage-post logs all"
    exit 1
    ;;
esac

summary
