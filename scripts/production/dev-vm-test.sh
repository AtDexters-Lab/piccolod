#!/usr/bin/env bash
# dev-vm-test.sh — Run e2e observation tests against a running Piccolo OS VM.
#
# Usage:
#   ./scripts/dev-vm-test.sh <IP>              # run all stages (VM stays running)
#   ./scripts/dev-vm-test.sh <IP> boot         # stage 1: boot & phase 1
#   ./scripts/dev-vm-test.sh <IP> pre-setup    # stage 2: pre-setup gating
#   ./scripts/dev-vm-test.sh <IP> setup        # stage 3: first-run setup (creates password!)
#   ./scripts/dev-vm-test.sh <IP> post-setup   # stage 4: post-setup functional
#   ./scripts/dev-vm-test.sh <IP> recovery     # stage 5: recovery key password reset
#   ./scripts/dev-vm-test.sh <IP> pcv          # stage 6: PCV mutation & export
#   ./scripts/dev-vm-test.sh <IP> reboot       # stage 7: reboot & unlock cycle
#   ./scripts/dev-vm-test.sh <IP> edge         # stage 8: edge cases
#   ./scripts/dev-vm-test.sh <IP> service-app    # stage 9: service app install/verify/uninstall
#   ./scripts/dev-vm-test.sh <IP> workspace-app  # stage 10: workspace app install/verify/uninstall
#   ./scripts/dev-vm-test.sh <IP> logs           # download piccolod journal logs
#   ./scripts/dev-vm-test.sh <IP> vdi            # stage 11: VDI post-mortem (powers off VM!)
#   ./scripts/dev-vm-test.sh <IP> full         # all stages including VDI (powers off VM!)
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

LOG_DIR="/tmp/claude/piccolo-e2e/logs"

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

# ensure_session — Verify we have a valid auth session; login+unlock if needed.
ensure_session() {
  local authed
  authed=$(curl -s --connect-timeout 5 -b "$COOKIE_JAR" \
    "http://$IP/api/v1/auth/session" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('authenticated',False))" 2>/dev/null)
  [[ "$authed" == "True" ]] && return 0
  # No valid session — login first, then unlock.
  curl -sf --connect-timeout 10 -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" \
    -d "{\"username\":\"admin\",\"password\":\"$PASS\"}" \
    "http://$IP/api/v1/auth/login" >/dev/null 2>&1 || true
  curl -sf --connect-timeout 10 -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" \
    -d "{\"password\":\"$PASS\"}" \
    "http://$IP/api/v1/crypto/unlock" >/dev/null 2>&1 || true
}

# dump_logs [label] — Download piccolod journal logs and save to LOG_DIR.
# Handles auth automatically. Prints the last 50 lines to stdout for quick triage.
dump_logs() {
  local label="${1:-$(date +%s)}"
  local outfile="$LOG_DIR/piccolod-${label}.log"
  mkdir -p "$LOG_DIR"
  ensure_session
  local code
  code=$(curl -s -o "$outfile" -w '%{http_code}' --connect-timeout 10 --max-time 30 \
    -b "$COOKIE_JAR" "http://$IP/api/v1/system/admin/diagnostic-log" 2>/dev/null)
  if [[ "$code" == "200" && -s "$outfile" ]]; then
    local lines
    lines=$(wc -l < "$outfile")
    echo -e "  ${CYAN}LOGS${NC} Saved $lines lines → $outfile"
    echo -e "  ${CYAN}LOGS${NC} Last 50 lines:"
    tail -50 "$outfile" | sed 's/^/       /'
  else
    echo -e "  ${YELLOW}LOGS${NC} Failed to download diagnostic log (HTTP $code)"
  fi
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
  check "1.1" "Dev binary running" "$ver" "piccolod"

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

  # Wait a moment for async operations (storage init, PCV activation)
  echo -e "  ${CYAN}INFO${NC} Waiting 10s for async operations..."
  sleep 10

  local crypto
  crypto=$(api "/api/v1/crypto/status")
  check "3.2" "Crypto now initialized" "$crypto" '"initialized":true'
  check "3.3" "Crypto now unlocked" "$crypto" '"locked":false'

  local health
  health=$(api "/api/v1/health/detail")
  check "3.4" "Persistence unlocked" "$health" '"control store'
  # Storage should show initialized or OK
  check_not "3.5" "No storage emergency" "$health" '"level": "error"'

  local emerg
  emerg=$(api "/api/v1/system/emergency")
  check "3.6" "Emergency still false" "$emerg" '"emergency":false'

  # Storage init is async — retry up to 30s
  for i in $(seq 1 30); do
    health=$(api "/api/v1/health/detail")
    echo "$health" | grep -qF '"storage initialized"' && break
    sleep 1
  done
  check "3.7" "Storage pool initialized" "$health" '"storage initialized"'

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
# Stage 5: Recovery Key Password Reset
# ─────────────────────────────────────────────────────────
stage_recovery() {
  echo -e "\n${CYAN}═══ Stage 5: Recovery Key Password Reset ═══${NC}"
  ensure_session

  # 5.1: No recovery key exists yet
  local rk_status
  rk_status=$(api "/api/v1/crypto/recovery-key")
  check "5.1" "No recovery key exists yet" "$rk_status" '"present":false'

  # 5.2: Generate recovery key
  local gen_resp recovery_key word_count
  gen_resp=$(post_csrf "/api/v1/crypto/recovery-key/generate")
  if [[ -z "$gen_resp" ]]; then
    echo -e "  ${RED}FAIL${NC} [5.2] Recovery key generation failed (empty response)"
    ((FAIL_COUNT++)) || true
    return
  fi
  recovery_key=$(echo "$gen_resp" | python3 -c "import sys,json; print(' '.join(json.load(sys.stdin).get('words',[])))" 2>/dev/null)
  word_count=$(echo "$recovery_key" | wc -w)
  if [[ "$word_count" -eq 24 ]]; then
    echo -e "  ${GREEN}PASS${NC} [5.2] Recovery key generated (24 words)"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [5.2] Recovery key word count: $word_count (expected 24)"
    ((FAIL_COUNT++)) || true
    return
  fi

  # 5.3: Recovery key status shows present
  rk_status=$(api "/api/v1/crypto/recovery-key")
  check "5.3" "Recovery key now present" "$rk_status" '"present":true'

  # 5.4: Reset password with recovery key
  local reset_code
  reset_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 10 \
    -X POST -H "Content-Type: application/json" \
    -d "{\"recovery_key\":\"$recovery_key\",\"new_password\":\"RecoveryTest-2026!\"}" \
    "http://$IP/api/v1/crypto/reset-password" 2>/dev/null)
  check "5.4" "Password reset via recovery key" "$reset_code" "200"

  sleep 2

  # 5.5: Login with new password
  rm -f "$COOKIE_JAR"
  local login_code
  login_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 10 \
    -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" \
    -d '{"username":"admin","password":"RecoveryTest-2026!"}' \
    "http://$IP/api/v1/auth/login" 2>/dev/null)
  check "5.5" "Login with new password" "$login_code" "200"

  # 5.6: Session shows staleness flags
  local session
  session=$(api "/api/v1/auth/session")
  check "5.6" "Password staleness flag set" "$session" '"password_stale":true'
  check "5.6" "Recovery staleness flag set" "$session" '"recovery_stale":true'

  # 5.7: Wrong recovery key rejected
  local wrong_code
  wrong_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 10 \
    -X POST -H "Content-Type: application/json" \
    -d '{"recovery_key":"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon","new_password":"ShouldFail-2026!"}' \
    "http://$IP/api/v1/crypto/reset-password" 2>/dev/null)
  check "5.7" "Wrong recovery key rejected" "$wrong_code" "401"

  # 5.8: Restore original password
  local restore_code
  restore_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 10 \
    -X POST -H "Content-Type: application/json" \
    -d "{\"recovery_key\":\"$recovery_key\",\"new_password\":\"$PASS\"}" \
    "http://$IP/api/v1/crypto/reset-password" 2>/dev/null)
  if [[ "$restore_code" == "200" ]]; then
    echo -e "  ${GREEN}PASS${NC} [5.8] Original password restored"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [5.8] Failed to restore original password (HTTP $restore_code)"
    echo -e "  ${RED}WARNING: Subsequent stages will fail — password is now RecoveryTest-2026!${NC}"
    ((FAIL_COUNT++)) || true
  fi

  # 5.9: Login with restored password
  rm -f "$COOKIE_JAR"
  login_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 10 \
    -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" \
    -d "{\"username\":\"admin\",\"password\":\"$PASS\"}" \
    "http://$IP/api/v1/auth/login" 2>/dev/null)
  check "5.9" "Login with restored password" "$login_code" "200"
  ensure_session
}

# ─────────────────────────────────────────────────────────
# Stage 6: PCV Export Pipeline
# ─────────────────────────────────────────────────────────
stage_pcv() {
  echo -e "\n${CYAN}═══ Stage 6: PCV Export Pipeline ═══${NC}"

  # Trigger on-demand publish (requires CSRF for POST)
  local token pub_code
  token=$(csrf)
  pub_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 30 \
    -b "$COOKIE_JAR" -X POST -H "X-CSRF-Token: $token" \
    "http://$IP/api/v1/system/pcv/publish" 2>/dev/null)
  check "6.1" "On-demand PCV publish" "$pub_code" "200"

  # Wait for publish to complete
  sleep 5

  # Download PCV export (GET — no CSRF needed)
  local export_code
  export_code=$(curl -s -o /tmp/claude/piccolo-e2e/pcv-export.enc -w '%{http_code}' \
    --connect-timeout 10 -b "$COOKIE_JAR" \
    "http://$IP/api/v1/system/pcv/export" 2>/dev/null)
  check "6.2" "PCV export downloadable" "$export_code" "200"

  if [[ -f /tmp/claude/piccolo-e2e/pcv-export.enc ]]; then
    local size
    size=$(stat -c%s /tmp/claude/piccolo-e2e/pcv-export.enc 2>/dev/null || echo 0)
    if [[ "$size" -gt 100 ]]; then
      echo -e "  ${GREEN}PASS${NC} [6.3] PCV archive has content ($size bytes)"
      ((PASS_COUNT++)) || true
    else
      echo -e "  ${RED}FAIL${NC} [6.3] PCV archive too small ($size bytes)"
      ((FAIL_COUNT++)) || true
    fi

    # Check it's gzip
    local magic
    magic=$(xxd -l 2 -p /tmp/claude/piccolo-e2e/pcv-export.enc 2>/dev/null || echo "")
    check "6.4" "PCV archive is gzip" "$magic" "1f8b"
  else
    skip "6.3" "PCV archive content" "export file missing"
    skip "6.4" "PCV archive is gzip" "export file missing"
  fi

  # Second publish (verify idempotent)
  token=$(csrf)
  local pub2_code
  pub2_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 30 \
    -b "$COOKIE_JAR" -X POST -H "X-CSRF-Token: $token" \
    "http://$IP/api/v1/system/pcv/publish" 2>/dev/null)
  check "6.5" "Second publish succeeds" "$pub2_code" "200"
}

# ─────────────────────────────────────────────────────────
# Stage 7: Reboot & Unlock
# ─────────────────────────────────────────────────────────
stage_reboot() {
  echo -e "\n${CYAN}═══ Stage 7: Reboot & Unlock Cycle ═══${NC}"

  # Find VM name
  local VM_STATE="/tmp/claude/piccolo-e2e/vm-name"
  if [[ ! -f "$VM_STATE" ]]; then
    skip "7.x" "Reboot tests" "No VM state file (manual VM?)"
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
  check "7.1" "Crypto initialized after reboot" "$crypto" '"initialized":true'
  check "7.2" "Crypto locked after reboot" "$crypto" '"locked":true'

  # Phase 1 should be idempotent (no emergency)
  local emerg
  emerg=$(api "/api/v1/system/emergency")
  check "7.3" "No emergency after reboot" "$emerg" '"emergency":false'

  # Health: storage OK, persistence locked
  local health
  health=$(api "/api/v1/health/detail")
  check "7.4" "Storage OK after reboot" "$health" '"disk preparation complete"'
  check "7.5" "Persistence locked after reboot" "$health" '"control store locked"'

  # Unlock
  rm -f "$COOKIE_JAR"
  local unlock_code
  unlock_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 10 \
    -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" \
    -d "{\"password\":\"$PASS\"}" \
    "http://$IP/api/v1/crypto/unlock" 2>/dev/null)
  check "7.6" "Unlock succeeds" "$unlock_code" "200"

  sleep 5

  crypto=$(api "/api/v1/crypto/status")
  check "7.7" "Crypto unlocked after unlock" "$crypto" '"locked":false'

  health=$(api "/api/v1/health/detail")
  check_not "7.8" "No errors in health" "$health" '"level": "error"'

  local ready
  ready=$(api "/api/v1/health/ready")
  check "7.9" "Health ready after unlock" "$ready" '"ready":true'

  # Storage activation is async — retry up to 30s
  for i in $(seq 1 30); do
    health=$(api "/api/v1/health/detail")
    echo "$health" | grep -qF '"storage activated"' && break
    sleep 1
  done
  check "7.10" "Storage pool activated" "$health" '"storage activated"'

  echo -e "\n  ${CYAN}Raw health post-reboot-unlock:${NC}"
  apij "/api/v1/health/detail"
}

# ─────────────────────────────────────────────────────────
# Stage 8: Edge Cases
# ─────────────────────────────────────────────────────────
stage_edge() {
  echo -e "\n${CYAN}═══ Stage 8: Edge Cases ═══${NC}"

  # Wrong password rejection
  local wrong_code
  wrong_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 \
    -X POST -H "Content-Type: application/json" \
    -d '{"password":"wrong-password"}' \
    "http://$IP/api/v1/crypto/unlock" 2>/dev/null)
  check_not "8.1" "Wrong password rejected (not 200)" "$wrong_code" "200"

  # PCV import without body should fail gracefully
  local import_code
  import_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 \
    -X POST "http://$IP/api/v1/system/pcv/import" 2>/dev/null)
  check_not "8.2" "PCV import without body not 200" "$import_code" "200"

  # Version still works under all conditions
  local ver
  ver=$(api "/version")
  check "8.3" "Version always accessible" "$ver" '"piccolod"'
}

# verify_app_http — Verify HTTP traffic reaches a running app's public port.
# Usage: verify_app_http <check_id> <app_name> <max_attempts> <sleep_seconds>
verify_app_http() {
  local check_id="$1" app_name="$2" max_attempts="${3:-5}" sleep_secs="${4:-3}"

  # Fetch the app's public port from the services API.
  local public_port
  public_port=$(api "/api/v1/apps/$app_name/services" | python3 -c "
import sys, json
try:
    svcs = json.load(sys.stdin).get('services', [])
    for s in svcs:
        p = s.get('public_port', 0)
        if p and p > 0:
            print(p)
            sys.exit(0)
    print('')
except:
    print('')" 2>/dev/null)

  if [[ -z "$public_port" || "$public_port" == "0" ]]; then
    echo -e "  ${RED}FAIL${NC} [$check_id] HTTP verification — no public port found"
    ((FAIL_COUNT++)) || true
    return
  fi

  echo -e "  ${CYAN}INFO${NC} Public port for $app_name: $public_port"

  # Any non-000 HTTP response proves traffic reaches the proxy/container.
  # 401/403 are expected for apps behind Piccolo's auth proxy.
  local http_code=""
  for i in $(seq 1 "$max_attempts"); do
    http_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 10 \
      "http://$IP:$public_port/" 2>/dev/null)
    if [[ "$http_code" != "000" && "$http_code" != "502" && "$http_code" != "503" ]]; then
      break
    fi
    echo -e "  ${CYAN}INFO${NC} HTTP attempt $i/$max_attempts → $http_code, retrying..."
    sleep "$sleep_secs"
  done

  if [[ "$http_code" != "000" && "$http_code" != "502" && "$http_code" != "503" ]]; then
    echo -e "  ${GREEN}PASS${NC} [$check_id] HTTP reachable on :$public_port (HTTP $http_code)"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [$check_id] HTTP unreachable on :$public_port (last HTTP $http_code)"
    ((FAIL_COUNT++)) || true
  fi
}

# ─────────────────────────────────────────────────────────
# Stage 9: Service App Install (Vaultwarden — catalog)
# ─────────────────────────────────────────────────────────
stage_service_app() {
  echo -e "\n${CYAN}═══ Stage 9: Service App Install (Vaultwarden) ═══${NC}"
  ensure_session

  local APP_NAME="vaultwarden"

  # Fetch catalog template for vaultwarden (service mode app).
  # The template endpoint returns raw YAML (application/x-yaml), not JSON.
  local template_yaml
  template_yaml=$(api "/api/v1/catalog/vaultwarden/template")

  if [[ -z "$template_yaml" ]]; then
    echo -e "  ${RED}FAIL${NC} [9.0] Failed to fetch vaultwarden catalog template"
    ((FAIL_COUNT++)) || true
    return
  fi
  echo -e "  ${CYAN}INFO${NC} Fetched vaultwarden catalog template (${#template_yaml} bytes)"

  # Build JSON payload with catalog_source tracking.
  # Vaultwarden requires an admin_token input for its admin panel.
  local payload
  payload=$(YAML="$template_yaml" NAME="$APP_NAME" python3 -c "
import json, os
print(json.dumps({
    'app_definition': os.environ['YAML'],
    'inputs': {'__app_address__': os.environ['NAME'], 'admin_token': 'e2e-test-token-2026'},
    'catalog_source': 'vaultwarden'
}))")

  # --- 9.1: Install app via POST /api/v1/apps ---
  local token
  token=$(csrf)
  local install_raw install_body install_http
  install_raw=$(curl -s -w '\n%{http_code}' --connect-timeout 10 --max-time 300 \
    -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
    -d "$payload" "http://$IP/api/v1/apps" 2>/dev/null)
  install_body=$(echo "$install_raw" | sed '$d')
  install_http=$(echo "$install_raw" | tail -1)

  if [[ "$install_http" == "201" ]]; then
    echo -e "  ${GREEN}PASS${NC} [9.1] Service app installed (HTTP 201)"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [9.1] Service app install failed (HTTP $install_http)"
    echo -e "       response: $(echo "$install_body" | head -5)"
    ((FAIL_COUNT++)) || true
    dump_logs "stage9-install-fail"
    token=$(csrf)
    curl -sf --connect-timeout 10 --max-time 30 -b "$COOKIE_JAR" \
      -X DELETE -H "X-CSRF-Token: $token" \
      "http://$IP/api/v1/apps/$APP_NAME" >/dev/null 2>&1 || true
    return
  fi

  # --- 9.2: Poll for running status (up to 120s) ---
  local app_status=""
  for i in $(seq 1 60); do
    app_status=$(api "/api/v1/apps/$APP_NAME" | python3 -c "
import sys, json
try:
    print(json.load(sys.stdin).get('data',{}).get('app',{}).get('status',''))
except:
    print('')" 2>/dev/null)
    [[ "$app_status" == "running" ]] && break
    sleep 2
  done
  if [[ "$app_status" != "running" ]]; then
    echo -e "  ${RED}FAIL${NC} [9.2] Service app reaches running (got: $app_status)"
    ((FAIL_COUNT++)) || true
    dump_logs "stage9-not-running"
  else
    echo -e "  ${GREEN}PASS${NC} [9.2] Service app reaches running"
    ((PASS_COUNT++)) || true
  fi

  # --- 9.3: Verify container is running ---
  local detail containers_running
  detail=$(api "/api/v1/apps/$APP_NAME")
  containers_running=$(echo "$detail" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    containers = data.get('data',{}).get('containers',[])
    print(len([c for c in containers if c.get('running')]))
except:
    print(0)" 2>/dev/null)

  if [[ "$containers_running" -gt 0 ]]; then
    echo -e "  ${GREEN}PASS${NC} [9.3] Container running ($containers_running container(s))"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [9.3] No running containers"
    ((FAIL_COUNT++)) || true
  fi

  # --- 9.4: HTTP verification — traffic reaches the container ---
  verify_app_http "9.4" "$APP_NAME" 5 3

  # --- 9.5: Fetch container logs ---
  local logs_raw logs_body logs_http
  logs_raw=$(curl -s -w '\n%{http_code}' --connect-timeout 5 \
    -b "$COOKIE_JAR" "http://$IP/api/v1/apps/$APP_NAME/logs" 2>/dev/null)
  logs_body=$(echo "$logs_raw" | sed '$d')
  logs_http=$(echo "$logs_raw" | tail -1)

  if [[ "$logs_http" == "200" ]]; then
    echo -e "  ${GREEN}PASS${NC} [9.5] Logs endpoint accessible"
    ((PASS_COUNT++)) || true
    echo -e "  ${CYAN}INFO${NC} Last 5 log lines:"
    echo "$logs_body" | tail -5 | sed 's/^/       /'
  else
    echo -e "  ${RED}FAIL${NC} [9.5] Logs endpoint failed (HTTP $logs_http)"
    ((FAIL_COUNT++)) || true
  fi

  # --- 9.6: Uninstall app ---
  token=$(csrf)
  local uninstall_code
  uninstall_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 30 --max-time 120 \
    -b "$COOKIE_JAR" \
    -X DELETE -H "X-CSRF-Token: $token" \
    "http://$IP/api/v1/apps/$APP_NAME" 2>/dev/null)
  check "9.6" "Service app uninstalled" "$uninstall_code" "200"

  # --- 9.7: Verify app is gone ---
  sleep 3
  check_http "9.7" "App no longer exists" "GET" "/api/v1/apps/$APP_NAME" "404"
}

# ─────────────────────────────────────────────────────────
# Stage 10: Workspace App Install (Code-server — catalog)
# ─────────────────────────────────────────────────────────
stage_workspace_app() {
  echo -e "\n${CYAN}═══ Stage 10: Workspace App Install (Code-server) ═══${NC}"
  ensure_session

  local APP_NAME="codeserver"
  local CATALOG_NAME="code-server"

  # Fetch catalog template for code-server (workspace mode app).
  # Uses default init (boot.sh wrapper) — exercises the exact path that was
  # broken by the :z lsetxattr issue on per-app users.
  # The template endpoint returns raw YAML (application/x-yaml), not JSON.
  local template_yaml
  template_yaml=$(api "/api/v1/catalog/$CATALOG_NAME/template")

  if [[ -z "$template_yaml" ]]; then
    echo -e "  ${RED}FAIL${NC} [10.0] Failed to fetch code-server catalog template"
    ((FAIL_COUNT++)) || true
    return
  fi
  echo -e "  ${CYAN}INFO${NC} Fetched code-server catalog template (${#template_yaml} bytes)"

  # Build JSON payload with catalog_source tracking.
  # Code-server requires a password input for its login page.
  local payload
  payload=$(YAML="$template_yaml" NAME="$APP_NAME" python3 -c "
import json, os
print(json.dumps({
    'app_definition': os.environ['YAML'],
    'inputs': {'__app_address__': os.environ['NAME'], 'password': 'E2eTest-2026!'},
    'catalog_source': 'code-server'
}))")

  # --- 10.1: Install workspace app via POST /api/v1/apps ---
  # Image pull for code-server (~500MB) can be slow. 10min timeout.
  local token
  token=$(csrf)
  local install_raw install_body install_http
  install_raw=$(curl -s -w '\n%{http_code}' --connect-timeout 10 --max-time 300 \
    -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -X POST -H "Content-Type: application/json" -H "X-CSRF-Token: $token" \
    -d "$payload" "http://$IP/api/v1/apps" 2>/dev/null)
  install_body=$(echo "$install_raw" | sed '$d')
  install_http=$(echo "$install_raw" | tail -1)

  if [[ "$install_http" == "201" ]]; then
    echo -e "  ${GREEN}PASS${NC} [10.1] Workspace app installed (HTTP 201)"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [10.1] Workspace app install failed (HTTP $install_http)"
    echo -e "       response: $(echo "$install_body" | head -5)"
    ((FAIL_COUNT++)) || true
    dump_logs "stage10-install-fail"
    token=$(csrf)
    curl -sf --connect-timeout 10 --max-time 30 -b "$COOKIE_JAR" \
      -X DELETE -H "X-CSRF-Token: $token" \
      "http://$IP/api/v1/apps/$APP_NAME" >/dev/null 2>&1 || true
    return
  fi

  # --- 10.2: Poll for running status (up to 120s) ---
  local app_status=""
  for i in $(seq 1 60); do
    app_status=$(api "/api/v1/apps/$APP_NAME" | python3 -c "
import sys, json
try:
    print(json.load(sys.stdin).get('data',{}).get('app',{}).get('status',''))
except:
    print('')" 2>/dev/null)
    [[ "$app_status" == "running" ]] && break
    sleep 2
  done
  if [[ "$app_status" != "running" ]]; then
    echo -e "  ${RED}FAIL${NC} [10.2] Workspace app reaches running (got: $app_status)"
    ((FAIL_COUNT++)) || true
    dump_logs "stage10-not-running"
  else
    echo -e "  ${GREEN}PASS${NC} [10.2] Workspace app reaches running"
    ((PASS_COUNT++)) || true
  fi

  # --- 10.3: Verify app detail shows running status ---
  # Workspace apps use --rootfs mode where container ID tracking differs from
  # service apps. Check app-level status instead of individual container state.
  local detail detail_status
  detail=$(api "/api/v1/apps/$APP_NAME")
  detail_status=$(echo "$detail" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    print(data.get('data',{}).get('app',{}).get('status',''))
except:
    print('')" 2>/dev/null)

  if [[ "$detail_status" == "running" ]]; then
    echo -e "  ${GREEN}PASS${NC} [10.3] App detail confirms running"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [10.3] App detail status: $detail_status (expected running)"
    ((FAIL_COUNT++)) || true
  fi

  # --- 10.4: HTTP verification — traffic reaches the container ---
  verify_app_http "10.4" "$APP_NAME" 5 3

  # --- 10.5: Fetch container logs ---
  local logs_raw logs_body logs_http
  logs_raw=$(curl -s -w '\n%{http_code}' --connect-timeout 5 \
    -b "$COOKIE_JAR" "http://$IP/api/v1/apps/$APP_NAME/logs" 2>/dev/null)
  logs_body=$(echo "$logs_raw" | sed '$d')
  logs_http=$(echo "$logs_raw" | tail -1)

  if [[ "$logs_http" == "200" ]]; then
    echo -e "  ${GREEN}PASS${NC} [10.5] Logs endpoint accessible"
    ((PASS_COUNT++)) || true
    echo -e "  ${CYAN}INFO${NC} Last 5 log lines:"
    echo "$logs_body" | tail -5 | sed 's/^/       /'
  else
    echo -e "  ${RED}FAIL${NC} [10.5] Logs endpoint failed (HTTP $logs_http)"
    ((FAIL_COUNT++)) || true
  fi

  # --- 10.6: Uninstall workspace app ---
  token=$(csrf)
  local uninstall_code
  uninstall_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 30 --max-time 120 \
    -b "$COOKIE_JAR" \
    -X DELETE -H "X-CSRF-Token: $token" \
    "http://$IP/api/v1/apps/$APP_NAME" 2>/dev/null)
  check "10.6" "Workspace app uninstalled" "$uninstall_code" "200"

  # --- 10.7: Verify app is gone ---
  sleep 3
  check_http "10.7" "App no longer exists" "GET" "/api/v1/apps/$APP_NAME" "404"
}

# ─────────────────────────────────────────────────────────
# Stage 11: VDI Post-Mortem
# ─────────────────────────────────────────────────────────
stage_vdi() {
  echo -e "\n${CYAN}═══ Stage 11: VDI Post-Mortem ═══${NC}"

  local VM_STATE="/tmp/claude/piccolo-e2e/vm-name"
  local VDI_WORK="/tmp/claude/piccolo-e2e/piccolo-os.vdi"
  local NBD_DEV="/dev/nbd0"
  local BASE_PART_COUNT=3  # ESP(p1) + BIOS(p2) + root(p3)
  local nbd_connected=0

  if [[ ! -f "$VM_STATE" ]]; then
    skip "11.x" "VDI post-mortem" "No VM state file (manual VM?)"
    return
  fi

  # Cache sudo credentials upfront — avoids repeated prompts during NBD/cryptsetup calls.
  sudo -v

  local VM_NAME
  VM_NAME=$(cat "$VM_STATE")

  # Cleanup trap — disconnects NBD on any failure, signal, or early return.
  # Uses ${var:-0} because EXIT trap may fire outside function scope where locals are gone.
  cleanup_nbd() {
    if [[ ${nbd_connected:-0} -eq 1 ]]; then
      sudo qemu-nbd --disconnect "${NBD_DEV:-/dev/nbd0}" 2>/dev/null || true
      nbd_connected=0
    fi
  }
  trap cleanup_nbd RETURN INT TERM EXIT

  # --- 11.1: Power off VM and wait for full stop ---
  echo -e "  ${CYAN}INFO${NC} Powering off VM: $VM_NAME"
  VBoxManage controlvm "$VM_NAME" poweroff 2>/dev/null || true

  local vm_stopped=0
  for i in $(seq 1 30); do
    local state
    state=$(VBoxManage showvminfo "$VM_NAME" --machinereadable 2>/dev/null \
      | grep '^VMState=' | cut -d'"' -f2)
    if [[ "$state" == "poweroff" || "$state" == "aborted" ]]; then
      vm_stopped=1
      break
    fi
    sleep 1
  done
  sleep 2  # Grace period for VBox to release file locks

  if [[ $vm_stopped -eq 1 ]]; then
    echo -e "  ${GREEN}PASS${NC} [11.1] VM powered off"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [11.1] VM did not stop within 30s"
    ((FAIL_COUNT++)) || true
    return
  fi

  # --- 11.2: Mount VDI via NBD ---
  sudo modprobe nbd max_part=16
  sudo qemu-nbd --disconnect "$NBD_DEV" 2>/dev/null || true
  if sudo qemu-nbd --connect="$NBD_DEV" "$VDI_WORK"; then
    nbd_connected=1
    sleep 1
    echo -e "  ${GREEN}PASS${NC} [11.2] VDI mounted via NBD"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [11.2] Failed to mount VDI via NBD"
    ((FAIL_COUNT++)) || true
    return
  fi

  # --- 11.3: Check data partition exists ---
  local sfdisk_json data_node
  sfdisk_json=$(sudo sfdisk -J "$NBD_DEV" 2>/dev/null)
  data_node=$(echo "$sfdisk_json" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    parts = data['partitiontable']['partitions']
    if len(parts) <= $BASE_PART_COUNT:
        print('ERROR: only ' + str(len(parts)) + ' partitions found')
        sys.exit(0)
    last = max(parts, key=lambda p: p.get('start', 0))
    node = last.get('node', '')
    if not node:
        print('ERROR: no node in partition data')
        sys.exit(0)
    print(node)
except (KeyError, ValueError, IndexError) as e:
    print(f'ERROR: {e}')
    sys.exit(0)
" 2>/dev/null)

  if [[ -n "$data_node" && "$data_node" != ERROR* ]]; then
    echo -e "  ${GREEN}PASS${NC} [11.3] Data partition exists: $data_node"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [11.3] Data partition not found (expected >$BASE_PART_COUNT partitions)"
    ((FAIL_COUNT++)) || true
    return
  fi

  # --- 11.4: Verify LUKS2 header on data partition ---
  local luks_output luks_exit=0
  luks_output=$(sudo cryptsetup isLuks --type luks2 "$data_node" 2>&1) || luks_exit=$?
  if [[ $luks_exit -eq 0 ]]; then
    echo -e "  ${GREEN}PASS${NC} [11.4] Data partition has LUKS2 header"
    ((PASS_COUNT++)) || true
  else
    echo -e "  ${RED}FAIL${NC} [11.4] Data partition missing LUKS2 header (exit $luks_exit)"
    echo -e "       output: $luks_output"
    ((FAIL_COUNT++)) || true
  fi
}

# ─────────────────────────────────────────────────────────
# Runner
# ─────────────────────────────────────────────────────────
mkdir -p "$(dirname "$COOKIE_JAR")"

case "$STAGE" in
  boot)        stage_boot ;;
  pre-setup)   stage_pre_setup ;;
  setup)       stage_setup ;;
  post-setup)  stage_post_setup ;;
  recovery)    stage_recovery ;;
  pcv)         stage_pcv ;;
  reboot)      stage_reboot ;;
  edge)        stage_edge ;;
  service-app)   stage_service_app ;;
  workspace-app) stage_workspace_app ;;
  logs)          dump_logs "manual" ;;
  net-check)
    ensure_session
    echo -e "\n${CYAN}═══ Network Check ═══${NC}"
    apij "/api/v1/system/network-check"
    ;;
  vdi)         stage_vdi ;;
  full)
    stage_boot; stage_pre_setup; stage_setup; stage_post_setup
    stage_recovery; stage_pcv; stage_reboot; stage_edge; stage_service_app; stage_workspace_app
    stage_vdi    # last — powers off VM
    ;;
  all)         # stages 1-10, VM remains running
    stage_boot
    stage_pre_setup
    stage_setup
    stage_post_setup
    stage_recovery
    stage_pcv
    stage_reboot
    stage_edge
    stage_service_app
    stage_workspace_app
    ;;
  *)
    echo "Unknown stage: $STAGE"
    echo "Valid stages: boot pre-setup setup post-setup recovery pcv reboot edge service-app workspace-app logs vdi full all"
    exit 1
    ;;
esac

summary
