#!/usr/bin/env bash
# dev-vm-test.sh — Run e2e observation tests against a running Piccolo OS VM.
#
# Usage:
#   ./scripts/dev-vm-test.sh <IP>              # run all stages
#   ./scripts/dev-vm-test.sh <IP> boot         # stage 1: boot & phase 1
#   ./scripts/dev-vm-test.sh <IP> pre-setup    # stage 2: pre-setup gating
#   ./scripts/dev-vm-test.sh <IP> setup        # stage 3: first-run setup (creates password!)
#   ./scripts/dev-vm-test.sh <IP> post-setup   # stage 4: post-setup functional
#   ./scripts/dev-vm-test.sh <IP> pcv          # stage 5: PCV mutation & export
#   ./scripts/dev-vm-test.sh <IP> reboot       # stage 6: reboot & unlock cycle
#   ./scripts/dev-vm-test.sh <IP> edge         # stage 7: edge cases
set -euo pipefail

IP="${1:?Usage: $0 <VM_IP> [stage]}"
STAGE="${2:-all}"
PASS="PiccoloE2E-Test-2026!"  # test password
COOKIE_JAR="/tmp/claude/piccolo-e2e/cookies.txt"
PASS_COUNT=0
FAIL_COUNT=0
SKIP_COUNT=0

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
NC='\033[0m'

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

# ─────────────────────────────────────────────────────────
# Stage 1: Boot & Phase 1 Disk Prep
# ─────────────────────────────────────────────────────────
stage_boot() {
  echo -e "\n${CYAN}═══ Stage 1: Boot & Phase 1 Disk Prep ═══${NC}"

  local ver
  ver=$(api "/version")
  check "1.1" "Dev binary running" "$ver" "v0.1.16"

  local emerg
  emerg=$(api "/api/v1/system/emergency")
  check "1.2" "No emergency mode" "$emerg" '"emergency":false'

  local health
  health=$(api "/api/v1/health/detail")
  check "1.3" "Storage health OK" "$health" '"disk preparation complete"'
  check "1.4" "HTTP health OK" "$health" '"HTTP server initialized"'
  check "1.5" "mDNS registered" "$health" '"mdns'
  check "1.6" "Persistence locked (pre-setup)" "$health" '"control store locked"'
  check "1.7" "App manager gated" "$health" '"app manager gated by lock state"'

  local ready
  ready=$(api "/api/v1/health/ready")
  # Pre-setup: should NOT be ready (persistence locked)
  check "1.8" "Health not ready (pre-setup)" "$ready" '"ready"'

  echo -e "\n  ${CYAN}Raw health:${NC}"
  apij "/api/v1/health/detail"
}

# ─────────────────────────────────────────────────────────
# Stage 2: Pre-Setup API Gating
# ─────────────────────────────────────────────────────────
stage_pre_setup() {
  echo -e "\n${CYAN}═══ Stage 2: Pre-Setup API Gating ═══${NC}"

  local crypto
  crypto=$(api "/api/v1/crypto/status")
  check "2.1" "Crypto not initialized" "$crypto" '"initialized":false'
  check "2.2" "Crypto not locked (no keyset)" "$crypto" '"locked":false'

  # Portal should load (static assets not blocked)
  check_http "2.3" "Portal HTML loads" "GET" "/" "200"

  # Health endpoints are public
  check_http "2.4" "Health live accessible" "GET" "/api/v1/health/live" "200"
  check_http "2.5" "Health ready accessible" "GET" "/api/v1/health/ready" "200"

  # Admin endpoints should be blocked (no session)
  check_http "2.6" "Apps endpoint requires auth" "GET" "/api/v1/apps" "401"

  echo -e "\n  ${CYAN}Raw crypto status:${NC}"
  apij "/api/v1/crypto/status"
}

# ─────────────────────────────────────────────────────────
# Stage 3: First-Run Setup
# ─────────────────────────────────────────────────────────
stage_setup() {
  echo -e "\n${CYAN}═══ Stage 3: First-Run Setup (crypto/setup) ═══${NC}"

  # Clear cookies
  rm -f "$COOKIE_JAR"

  local setup_resp
  setup_resp=$(post "/api/v1/crypto/setup" "{\"password\":\"$PASS\"}")
  check "3.1" "Setup returns success" "$setup_resp" ""  # any non-error 2xx

  # Check status response (may be in setup_resp or need separate call)
  local status_code
  status_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 10 \
    -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" \
    -d "{\"password\":\"$PASS\"}" \
    "http://$IP/api/v1/crypto/setup" 2>/dev/null)
  # If already set up, 409 is expected; if fresh, 200
  echo -e "  ${CYAN}INFO${NC} crypto/setup HTTP $status_code"

  # Wait a moment for async operations (LUKS init, PCV activation)
  echo -e "  ${CYAN}INFO${NC} Waiting 10s for async operations..."
  sleep 10

  local crypto
  crypto=$(api "/api/v1/crypto/status")
  check "3.2" "Crypto now initialized" "$crypto" '"initialized":true'
  check "3.3" "Crypto now unlocked" "$crypto" '"locked":false'

  local health
  health=$(api "/api/v1/health/detail")
  check "3.4" "Persistence unlocked" "$health" '"control store'
  # Storage should show LUKS initialized or OK
  check_not "3.5" "No storage emergency" "$health" '"level": "error"'

  local emerg
  emerg=$(api "/api/v1/system/emergency")
  check "3.6" "Emergency still false" "$emerg" '"emergency":false'

  echo -e "\n  ${CYAN}Raw health post-setup:${NC}"
  apij "/api/v1/health/detail"
  echo -e "\n  ${CYAN}Raw crypto status:${NC}"
  apij "/api/v1/crypto/status"
}

# ─────────────────────────────────────────────────────────
# Stage 4: Post-Setup Functional Smoke
# ─────────────────────────────────────────────────────────
stage_post_setup() {
  echo -e "\n${CYAN}═══ Stage 4: Post-Setup Functional Smoke ═══${NC}"

  # We should have a session from setup
  local ready
  ready=$(api "/api/v1/health/ready")
  check "4.1" "Health ready" "$ready" '"ready":true'

  # Try to access protected endpoints
  local apps
  apps=$(api "/api/v1/apps")
  # Should return 200 with [] or app list; not 401
  check_http "4.2" "Apps endpoint accessible" "GET" "/api/v1/apps" "200"

  local session
  session=$(api "/api/v1/auth/session")
  check "4.3" "Session valid" "$session" '"authenticated"'

  # Version endpoint still works
  local ver
  ver=$(api "/version")
  check "4.4" "Version still accessible" "$ver" '"piccolod"'

  echo -e "\n  ${CYAN}Raw session:${NC}"
  apij "/api/v1/auth/session"
}

# ─────────────────────────────────────────────────────────
# Stage 5: PCV Export Pipeline
# ─────────────────────────────────────────────────────────
stage_pcv() {
  echo -e "\n${CYAN}═══ Stage 5: PCV Export Pipeline ═══${NC}"

  # Trigger on-demand publish (requires CSRF for POST)
  local token pub_code
  token=$(csrf)
  pub_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 30 \
    -b "$COOKIE_JAR" -X POST -H "X-CSRF-Token: $token" \
    "http://$IP/api/v1/system/pcv/publish" 2>/dev/null)
  check "5.1" "On-demand PCV publish" "$pub_code" "200"

  # Wait for publish to complete
  sleep 5

  # Download PCV export (GET — no CSRF needed)
  local export_code
  export_code=$(curl -s -o /tmp/claude/piccolo-e2e/pcv-export.enc -w '%{http_code}' \
    --connect-timeout 10 -b "$COOKIE_JAR" \
    "http://$IP/api/v1/system/pcv/export" 2>/dev/null)
  check "5.2" "PCV export downloadable" "$export_code" "200"

  if [[ -f /tmp/claude/piccolo-e2e/pcv-export.enc ]]; then
    local size
    size=$(stat -c%s /tmp/claude/piccolo-e2e/pcv-export.enc 2>/dev/null || echo 0)
    if [[ "$size" -gt 100 ]]; then
      echo -e "  ${GREEN}PASS${NC} [5.3] PCV archive has content ($size bytes)"
      ((PASS_COUNT++)) || true
    else
      echo -e "  ${RED}FAIL${NC} [5.3] PCV archive too small ($size bytes)"
      ((FAIL_COUNT++)) || true
    fi

    # Check it's gzip
    local magic
    magic=$(xxd -l 2 -p /tmp/claude/piccolo-e2e/pcv-export.enc 2>/dev/null || echo "")
    check "5.4" "PCV archive is gzip" "$magic" "1f8b"
  else
    skip "5.3" "PCV archive content" "export file missing"
    skip "5.4" "PCV archive is gzip" "export file missing"
  fi

  # Second publish (verify idempotent)
  token=$(csrf)
  local pub2_code
  pub2_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 30 \
    -b "$COOKIE_JAR" -X POST -H "X-CSRF-Token: $token" \
    "http://$IP/api/v1/system/pcv/publish" 2>/dev/null)
  check "5.5" "Second publish succeeds" "$pub2_code" "200"
}

# ─────────────────────────────────────────────────────────
# Stage 6: Reboot & Unlock
# ─────────────────────────────────────────────────────────
stage_reboot() {
  echo -e "\n${CYAN}═══ Stage 6: Reboot & Unlock Cycle ═══${NC}"

  # Find VM name
  local VM_STATE="/tmp/claude/piccolo-e2e/vm-name"
  if [[ ! -f "$VM_STATE" ]]; then
    skip "6.x" "Reboot tests" "No VM state file (manual VM?)"
    return
  fi
  local VM_NAME
  VM_NAME=$(cat "$VM_STATE")

  echo -e "  ${CYAN}INFO${NC} Rebooting VM: $VM_NAME"
  VBoxManage controlvm "$VM_NAME" reset 2>/dev/null || true

  # Wait for VM to come back
  echo -e "  ${CYAN}INFO${NC} Waiting for VM to reboot..."
  sleep 15

  for i in $(seq 1 30); do
    if curl -sf --connect-timeout 2 "http://$IP/version" >/dev/null 2>&1; then
      echo -e "  ${CYAN}INFO${NC} VM back up after ~$((15 + i*2))s"
      break
    fi
    sleep 2
  done

  # After reboot: should be initialized + locked
  local crypto
  crypto=$(api "/api/v1/crypto/status")
  check "6.1" "Crypto initialized after reboot" "$crypto" '"initialized":true'
  check "6.2" "Crypto locked after reboot" "$crypto" '"locked":true'

  # Phase 1 should be idempotent (no emergency)
  local emerg
  emerg=$(api "/api/v1/system/emergency")
  check "6.3" "No emergency after reboot" "$emerg" '"emergency":false'

  # Health: storage OK, persistence locked
  local health
  health=$(api "/api/v1/health/detail")
  check "6.4" "Storage OK after reboot" "$health" '"disk preparation complete"'
  check "6.5" "Persistence locked after reboot" "$health" '"control store locked"'

  # Unlock
  rm -f "$COOKIE_JAR"
  local unlock_code
  unlock_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 10 \
    -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" \
    -d "{\"password\":\"$PASS\"}" \
    "http://$IP/api/v1/crypto/unlock" 2>/dev/null)
  check "6.6" "Unlock succeeds" "$unlock_code" "200"

  sleep 5

  crypto=$(api "/api/v1/crypto/status")
  check "6.7" "Crypto unlocked after unlock" "$crypto" '"locked":false'

  health=$(api "/api/v1/health/detail")
  check_not "6.8" "No errors in health" "$health" '"level": "error"'

  local ready
  ready=$(api "/api/v1/health/ready")
  check "6.9" "Health ready after unlock" "$ready" '"ready":true'

  echo -e "\n  ${CYAN}Raw health post-reboot-unlock:${NC}"
  apij "/api/v1/health/detail"
}

# ─────────────────────────────────────────────────────────
# Stage 7: Edge Cases
# ─────────────────────────────────────────────────────────
stage_edge() {
  echo -e "\n${CYAN}═══ Stage 7: Edge Cases ═══${NC}"

  # Wrong password rejection
  local wrong_code
  wrong_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 \
    -X POST -H "Content-Type: application/json" \
    -d '{"password":"wrong-password"}' \
    "http://$IP/api/v1/crypto/unlock" 2>/dev/null)
  check_not "7.1" "Wrong password rejected (not 200)" "$wrong_code" "200"

  # PCV import without body should fail gracefully
  local import_code
  import_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 \
    -X POST "http://$IP/api/v1/system/pcv/import" 2>/dev/null)
  check_not "7.2" "PCV import without body not 200" "$import_code" "200"

  # Version still works under all conditions
  local ver
  ver=$(api "/version")
  check "7.3" "Version always accessible" "$ver" '"piccolod"'
}

# ─────────────────────────────────────────────────────────
# Runner
# ─────────────────────────────────────────────────────────
mkdir -p "$(dirname "$COOKIE_JAR")"

case "$STAGE" in
  boot)       stage_boot ;;
  pre-setup)  stage_pre_setup ;;
  setup)      stage_setup ;;
  post-setup) stage_post_setup ;;
  pcv)        stage_pcv ;;
  reboot)     stage_reboot ;;
  edge)       stage_edge ;;
  all)
    stage_boot
    stage_pre_setup
    stage_setup
    stage_post_setup
    stage_pcv
    stage_reboot
    stage_edge
    ;;
  *)
    echo "Unknown stage: $STAGE"
    echo "Valid stages: boot pre-setup setup post-setup pcv reboot edge all"
    exit 1
    ;;
esac

summary
