# Pressure-driven memory relief

## Objective

An app author declares a functional minimum and a memory profile. The minimum
can be correct while the app and its rootless runtime need more memory during
normal operation. Piccolod owns the effective enforcement policy; asking an
operator to edit YAML or reinstall the app is not the primary remedy.

The landing incident demonstrated repeated local reclaim stalls at the derived
80 MB soft limit, with an unchanged 128 MB hard limit. A runtime soft-limit
increase restored HTTP 200 and allowed approximately 108 MB of slice usage.
The five-minute resource reconciliation then restored the smaller soft limit.
This amendment implements a small feedback controller for that failure class.

## Scope and decisions

- Adjust only `MemoryHigh` for enabled, observed-running, bounded-profile apps.
  The empty/default profile is bounded. Elastic and undeclared-memory apps
  retain the existing behavior.
- Leave the manifest, CPU policy, and derived `MemoryMax` unchanged. There is
  no automatic workload restart/stop, automatic shrinking, prediction model,
  or new historical database.
- Use sustained local pressure with local `high` event increments and actual
  host headroom. Host PSI can contain the very local stalls being repaired and
  cannot independently veto relief.
- Sample every 30 seconds. A baseline plus two successive qualifying samples
  is needed: PSI `some avg10 >= 10%`, a positive local-high counter delta, and
  usage at least 90% of the effective soft limit. Reject missing, nonfinite,
  malformed, stale, or replayed samples; counter resets and cgroup replacement
  invalidate the previous evidence.
- Preserve a host reserve of `max(total RAM / 5, 256 MiB)`. Share half the
  remaining `MemAvailable` surplus across one polling pass. Debit attempted
  grants conservatively, including file-first application failures whose
  intent might be retried by ordinary reconciliation. This is not a physical
  memory reservation or a guarantee against later host pressure.
- Increase by 25% of the current soft limit, with a 16 MiB minimum step, capped
  at the unchanged hard ceiling. Insufficient allowance defers a full step.
- Allow at most two successful probes per continuous pressure episode, at
  least 60 seconds apart. Check and log whether PSI falls below 10% or improves
  by at least 20% after each probe. A first ineffective probe does not prevent
  a second: 80 -> 100 -> 125 MB may be needed for a 108 MB workload. Two valid
  healthy samples reset the episode. There is no unlimited grant loop.

## Ownership and lifecycle

The pressure monitor retains its existing PSI `avg60`/OOM notifications. It
invokes an optional `MemoryPressureResponder` once per polling pass outside
its own mutex, passing a single host-capacity sample and all app samples.

AppManager is the policy writer. It takes lifecycle admission nonblocking,
then the existing slice-policy mutex nonblocking, matching the existing
lifecycle-to-policy lock order. Busy owners, cancellation, task-pressure
admission, unavailable app state, or interrupted transitions defer response.
It rechecks cgroup identity, page-rounded live limits, usage, and pressure
before applying. Systemctl commands share a ten-second cancellable budget.
Slice-policy removal uses the same mutex. No new callback can race an owned
manifest transaction or uninstall into recreating its policy.

The preexisting periodic resource-reconcile lifecycle behavior is retained;
this change does not redesign app lifecycle or resource admission.

## Persistence and reconciliation

Use the existing `piccolo-resources.conf`, not another state store. An adjusted
file includes a generated comment recording the unadjusted MemoryHigh. Restore
only if the entire file equals the expected generated candidate for the current
baseline, MemoryMax, and CPUWeight. Unmarked/manual files, invalid markers,
out-of-ceiling values, and changed declarations cannot become adaptive state.
Non-missing read errors defer reconciliation rather than discarding a budget
that could not be verified.

Keep the existing persistent systemctl set-property behavior, including its
systemd override layer. A file write records intent; a successful live command
records a successful probe. Failed live application does not produce feedback
or immediately spend the episode's successful-probe count. A process-local
pending target is confirmed by fresh live telemetry if ordinary reconciliation
completes the saved intent; confirmation counts one probe and starts cooldown.
Read errors or canceled polling rounds cannot discard the existing episode cap.
After daemon restart or reboot the saved effective policy is
restored, while fresh samples are required for further response.

## Completion evidence and limits

Fixture tests must cover the landing progression and recovery, bounded probes,
unchanged hard ceiling/declaration, simultaneous-app allowance, low/unknown
host capacity, eligibility, sampling gaps and replay, counter/cgroup reset,
live-limit mismatch, pre-write rechecks, failed application, and admission
contention. Reader tests cover valid kernel-like files and malformed/missing
inputs. Persistence tests cover restart adoption, baseline/profile invalidation,
manual file rejection, file-versus-live failure and retry, removal, cancellation,
and bounded command deadlines. Run affected Go suites and focused race checks.

These are component/integration checks; they do not prove installed-host
recovery or reboot behavior on the user's server. Deployment should reproduce
limit-induced pressure while observing MemoryHigh, MemoryMax, PSI, high-event
deltas, and HTTP recovery, then check a five-minute reconcile and a restart.
No relief is promised when the fixed hard ceiling is insufficient or the host
has no safe capacity. Existing pressure reporting remains active in those cases.

The Codex CLI gating review found a fresh-sample timing error after a slow
earlier application's policy write. Fresh pre-write validation now uses the
current clock, and the cooldown starts at successful application or pending
intent confirmation. A two-app fixture with a simulated six-second first write
reproduced the failure before the fix and now passes. The full app suite,
focused controller race checks, daemon build, and independent re-review passed.
The corrected implementation passed a second Codex CLI gating review with no
actionable regressions. Its affected Go suites and focused race checks passed;
broader server tests were blocked by sandbox socket restrictions.

## Rollback

Returning to the previous binary restores the original derived limits during
its normal policy reconciliation. It ignores the generated baseline comment.
The original too-small soft limit may therefore return; retain an operator's
explicit temporary relief until the incident is resolved. No app manifest or
data migration is involved.
