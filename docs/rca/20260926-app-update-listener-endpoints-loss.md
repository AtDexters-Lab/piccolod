# App update lost unchanged listener endpoints during runtime replacement

- Date: 2026-09-26
- Observed version: UI telemetry reported `v0.2.44`; the affected code matches
  the local `v0.2.44` tag and `77c0cb0` baseline.
- Impact: The `landing` manifest update prepared its new image/rootfs, failed
  before creating replacement containers, and restored the previous app.

## Evidence

`piccolod-diagnostic (18).log` records two failed
`POST /api/v1/apps/landing/manifest/update` requests at 12:06:01 and 12:07:56
IST, lasting 44.82 and 50.37 seconds. Both attempts created the candidate
rootfs, stopped and removed the previous containers, then restored the old
rootfs and started replacement containers for the previous app.

The operator's UI reported:

```text
app update rolled back: existing listener endpoints unavailable
```

The wizard also showed `ClientException: Failed to fetch`. The confirmed
backend error identifies the listener failure; that fetch error alone does
not identify a transport cause. The image download and Google credentials
were not the cause of the confirmed rollback.

## Cause

`installedAppApplyTransaction.prepareListenersIfNeeded` correctly skips a
prepared listener plan when the manifest's listeners are unchanged. The
transaction then calls `SuspendAppPublication`, which withdraws routing but
retains the endpoints, allocated ports, and the owning resume token.

Common runtime quiescence called `DeactivateApp` after stopping containers.
That operation deleted the endpoint registry and released its ports, even
though the transaction's suspension record remained. The staged recreation
path then asked `manifestUpdateRuntimeEndpoints` for the existing endpoints;
none remained, so it returned the confirmed error before candidate creation.

The storage test fixture also enables `PICCOLO_ALLOW_UNMOUNTED_TESTS`, which
bypasses the missing-endpoint check. Earlier image-update tests could therefore
complete without exercising the production failure.

## Fix and boundaries

Common quiescence now calls `DeactivateAppUnlessSuspended`. Its atomic check
under the publication lifecycle lock preserves transaction-owned endpoints,
port allocations, and resume authority. It clears the stopped runtime's
container identity and transient health tracking. Publication remains inactive
until the owning operation resumes it after readiness and commit, or restores
the previous app during rollback.

Ordinary stop still tears down endpoints. Explicit `DeactivateApp` retains its
existing allocation-release behavior because recovery uses it to retry a
conflicting host port. Permanent `RemoveApp` still releases endpoints and
suspension state. No new durable state or recovery protocol is introduced.

## Validation

- The app regression failed on the baseline with the exact observed error.
- The regression runs full apply/quiescence/recreation with the production
  missing-endpoint guard enabled, and verifies original bindings, replacement
  readiness before publication, successful commit, and restoration after an
  injected readiness failure.
- Service tests cover retained allocations and token authority during
  quiescence, stopped-runtime bookkeeping cleanup, ordinary stop, explicit
  deactivation under suspension, and permanent removal.

These are local tests using container/storage fixtures. They do not establish
installed-device or production update success. The post-incident stop/start
and network-tab discrepancy require separate live verification after rollout.
