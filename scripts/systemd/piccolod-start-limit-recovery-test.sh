#!/bin/bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
helper="$script_dir/piccolod-start-limit-recovery.sh"
self=$(realpath -- "${BASH_SOURCE[0]}")

if [[ $(basename -- "$0") == "systemctl" ]]; then
  printf 'systemctl %s\n' "$*" >> "$MOCK_LOG"
  if [[ "${1:-}" == "show" ]]; then
    if [[ "${2:-}" == "piccolod.service" ]]; then
      if [[ "${MOCK_PICCOLOD_STATE:-failed}" == "real" ]]; then
        exec /usr/bin/systemctl "$@"
      fi
      printf '%s\n' "${MOCK_PICCOLOD_STATE:-failed}"
      exit 0
    fi
    if [[ "${MOCK_HEALTH_STATE:-active}" == "real" ]]; then
      exec /usr/bin/systemctl "$@"
    fi
    printf 'ActiveState=%s\n' "${MOCK_HEALTH_STATE:-active}"
    printf 'InvocationID=%s\n' "${MOCK_HEALTH_INVOCATION-fixture-invocation}"
    printf 'Result=%s\n' "${MOCK_HEALTH_RESULT:-success}"
    exit 0
  fi
  if [[ "${MOCK_REBOOT_FAIL:-0}" == "1" ]]; then
    exit 1
  fi
  exit 0
fi

if [[ $(basename -- "$0") == "sleep" ]]; then
  printf 'sleep %s\n' "${1:-}" >> "$MOCK_LOG"
  exit 0
fi

test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
systemctl_mock="$test_dir/systemctl"
sleep_mock="$test_dir/sleep"
ln -s "$self" "$systemctl_mock"
ln -s "$self" "$sleep_mock"
log="$test_dir/calls.log"

run_helper() {
  MOCK_LOG="$log" \
  PICCOLO_START_LIMIT_SYSTEMCTL="$systemctl_mock" \
  PICCOLO_START_LIMIT_SLEEP="$sleep_mock" \
  PICCOLO_START_LIMIT_MAX_WAIT_SECONDS="${PICCOLO_START_LIMIT_MAX_WAIT_SECONDS:-2}" \
    "$helper"
}

: > "$log"
MONITOR_UNIT=piccolod.service run_helper
[[ ! -s "$log" ]]

: > "$log"
MONITOR_UNIT=other.service MONITOR_SERVICE_RESULT=start-limit-hit run_helper
[[ ! -s "$log" ]]

: > "$log"
MONITOR_UNIT=piccolod.service MONITOR_SERVICE_RESULT=exit-code MOCK_PICCOLOD_STATE=activating run_helper
grep -Fxq 'sleep 1' "$log"
grep -Fxq 'systemctl show piccolod.service --property=ActiveState --value' "$log"
[[ $(grep -Fc 'systemctl ' "$log") -eq 1 ]]

: > "$log"
MONITOR_UNIT=piccolod.service MONITOR_SERVICE_RESULT=exit-code MOCK_PICCOLOD_STATE=maintenance PICCOLO_START_LIMIT_STATE_PROBE_ATTEMPTS=2 run_helper
[[ $(grep -Fc 'systemctl show piccolod.service --property=ActiveState --value' "$log") -eq 2 ]]
grep -Fxq 'systemctl --no-block reboot' "$log"

: > "$log"
MONITOR_UNIT=piccolod.service MONITOR_SERVICE_RESULT=exit-code MOCK_PICCOLOD_STATE=failed MOCK_HEALTH_STATE=active run_helper
grep -Fxq 'systemctl show piccolod.service --property=ActiveState --value' "$log"
grep -Fxq 'systemctl show health-checker.service --property=ActiveState --property=InvocationID --property=Result' "$log"
grep -Fxq 'systemctl --no-block reboot' "$log"

: > "$log"
MONITOR_UNIT=piccolod.service MONITOR_SERVICE_RESULT=signal MOCK_PICCOLOD_STATE=failed MOCK_HEALTH_STATE=failed run_helper
grep -Fxq 'systemctl --no-block reboot' "$log"

: > "$log"
MONITOR_UNIT=piccolod.service MONITOR_SERVICE_RESULT=exit-code MOCK_PICCOLOD_STATE=failed MOCK_HEALTH_STATE=inactive MOCK_HEALTH_INVOCATION=completed-invocation MOCK_HEALTH_RESULT=success run_helper
grep -Fxq 'systemctl show health-checker.service --property=ActiveState --property=InvocationID --property=Result' "$log"
grep -Fxq 'systemctl --no-block reboot' "$log"

: > "$log"
MONITOR_UNIT=piccolod.service MONITOR_SERVICE_RESULT=watchdog MOCK_PICCOLOD_STATE=failed MOCK_HEALTH_STATE=activating run_helper
[[ $(grep -Fc 'systemctl show health-checker.service --property=ActiveState --property=InvocationID --property=Result' "$log") -eq 2 ]]
[[ $(grep -Fc 'sleep 1' "$log") -eq 3 ]]
grep -Fxq 'systemctl --no-block reboot' "$log"

: > "$log"
MONITOR_UNIT=piccolod.service MONITOR_SERVICE_RESULT=watchdog MOCK_PICCOLOD_STATE=failed MOCK_HEALTH_STATE=inactive MOCK_HEALTH_INVOCATION= run_helper
[[ $(grep -Fc 'systemctl show health-checker.service --property=ActiveState --property=InvocationID --property=Result' "$log") -eq 2 ]]
[[ $(grep -Fc 'sleep 1' "$log") -eq 3 ]]
grep -Fxq 'systemctl --no-block reboot' "$log"

: > "$log"
if MONITOR_UNIT=piccolod.service MONITOR_SERVICE_RESULT=timeout MOCK_PICCOLOD_STATE=failed MOCK_HEALTH_STATE=active MOCK_REBOOT_FAIL=1 run_helper; then
  echo "expected reboot request failure to propagate" >&2
  exit 1
fi

echo "piccolod start-limit recovery tests passed"
