#!/bin/bash

# Compose Piccolod's systemd start limit with the current MicroOS boot-health
# transaction. Snapshot and rollback decisions remain owned by health-checker;
# this helper only asks PID 1 for a normal reboot after that decision is
# terminal, or after a bounded wait when no checker transaction reaches one.

set -u

if [[ "${MONITOR_UNIT:-}" != "piccolod.service" ||
      -z "${MONITOR_SERVICE_RESULT:-}" ]]; then
  exit 0
fi

readonly health_unit="health-checker.service"
readonly systemctl_bin="${PICCOLO_START_LIMIT_SYSTEMCTL:-/usr/bin/systemctl}"
readonly sleep_bin="${PICCOLO_START_LIMIT_SLEEP:-/usr/bin/sleep}"
readonly max_wait_seconds="${PICCOLO_START_LIMIT_MAX_WAIT_SECONDS:-300}"
readonly state_probe_attempts="${PICCOLO_START_LIMIT_STATE_PROBE_ATTEMPTS:-5}"

if [[ ! "$max_wait_seconds" =~ ^[1-9][0-9]*$ ||
      ! "$state_probe_attempts" =~ ^[1-9][0-9]*$ ]]; then
  echo "Invalid Piccolod start-limit recovery wait bound: $max_wait_seconds" >&2
  exit 2
fi

# OnFailure receives the service's terminal process result (for example,
# "exit-code"), including when the following restart is rejected by the start
# limiter. systemd does not replace it with "start-limit-hit". Give PID 1 one
# second to queue the normal automatic restart, then continue only if the unit
# remains terminally failed. During RestartSec the unit is already activating
# with substate auto-restart, so ordinary one-off failures return here.
service_state=""
for ((probe = 0; probe < state_probe_attempts; probe += 1)); do
  "$sleep_bin" 1
  service_state=$("$systemctl_bin" show "$MONITOR_UNIT" --property=ActiveState --value 2>/dev/null || true)
  case "$service_state" in
    failed)
      break
      ;;
    active|activating|reloading|deactivating|inactive)
      exit 0
      ;;
  esac
done

if [[ "$service_state" != "failed" ]]; then
  echo "Piccolod failure state remained unavailable after ${state_probe_attempts} seconds; entering bounded boot-health recovery." >&2
fi

waited=0

while (( waited < max_wait_seconds )); do
  health_snapshot=$("$systemctl_bin" show "$health_unit" \
    --property=ActiveState --property=InvocationID --property=Result 2>/dev/null || true)
  state=""
  invocation=""
  result=""
  while IFS='=' read -r key value; do
    case "$key" in
      ActiveState) state="$value" ;;
      InvocationID) invocation="$value" ;;
      Result) result="$value" ;;
    esac
  done <<< "$health_snapshot"
  case "$state" in
    active)
      echo "Piccolod start limit reached after boot health succeeded; requesting normal reboot."
      exec "$systemctl_bin" --no-block reboot
      ;;
    failed)
      echo "Piccolod start limit reached after boot health failed; preserving its recovery decision and requesting normal reboot."
      exec "$systemctl_bin" --no-block reboot
      ;;
    inactive)
      if [[ -n "$invocation" && "$result" == "success" ]]; then
        echo "Piccolod start limit reached after boot health succeeded; requesting normal reboot."
        exec "$systemctl_bin" --no-block reboot
      fi
      ;;
  esac

  "$sleep_bin" 1
  ((waited += 1))
done

echo "Piccolod start limit reached but boot health did not finish within ${max_wait_seconds} seconds; requesting normal reboot."
exec "$systemctl_bin" --no-block reboot
