# App Apply publication and lost-response recovery

## Incident

On 26 September 2026, a config-only update to `landing` disconnected its
remote Edit Config dialog. Diagnostic `(20).log` stopped before the result was
known. The later `(17).log` showed that Apply returned HTTP 200 at 15:21:46,
after graceful container stops timed out and the dedicated user session was
quiesced. Replacement containers and the local HTTP proxy were recreated.

The canonical application hostname subsequently reached that proxy but failed
against its local upstream. Raising the app account's memory soft limit from
approximately 80 MB to 128 MB restored local and canonical-host HTTP 200 twice.
The custom domain continued closing connections. Memory-policy changes are
separate work and are not part of this fix.

## Publication restoration

Suspension withdraws portal aliases and LAN discovery. The config-only Apply
path recreates containers using a prepared reconciliation with unchanged
listeners. Publishing that reconciliation restarted the local proxies and
cleared the suspended marker, but produced no configuration delta and omitted
runtime advertisement. The later Resume call saw an already-active app and
returned without restoring either projection.

Prepared publication now advertises the committed active projection when it
reactivates an inactive app. The callback runs after releasing the registry
mutex, while lifecycle ownership is retained. Discovery receives the complete
restored endpoint set, including retained listeners, with removed endpoints
preserved and no duplicate Added/Updated labels. Configuration results remain
unchanged. Failed activation, stale ownership, repeated publication, and passive
unchanged reconciliation do not advertise routes.

## Operation result recovery

Portal projection changes can restart the shared tunnel and interrupt the HTTP
Apply response while the detached server operation continues. The config and
manifest dialogs previously cleared their task ID on a transport exception,
which disposed the progress connection before it could reconnect.

The dialogs retain the original task after a transport exception, disable
another Apply, and show that the result is unknown. Completion, including a
rollback error, is recovered through the existing task stream. A terminal event
received before the HTTP request settles is retained in full. Access-repair
completion includes structured metadata so recovery cannot turn a committed
update with pending access repair into an ordinary success. Real HTTP rejection
and stale-preview handling remain available. No automatic Apply retry is added.

The stream subscribes before reading the last-event snapshot, preventing a
completion between replay and subscription from being lost. Recovery uses the
existing process-local reporter: terminal events are retained for two minutes.
If the daemon restarts or an event has expired, the dialog keeps the outcome
unknown rather than inferring success from saved config. The dialog can be closed.

## Validation boundaries

Service regressions reproduce the missing advertisement on the previous code
and cover unchanged/changed listeners, ownership rejection, activation failure,
and repeated publication. App transaction tests cover commit and rollback;
WebSocket tests cover replay of success, rollback, and access-repair completion.
Dialog tests exercise transport loss, completion ordering, rejection, and repair.

The two transport-loss widget regressions fail against v0.2.45 because the task
panel is discarded, and pass with this fix. The focused UI suite has 34 passing
tests, including 24 dialog cases; focused analysis reports no issues. Tests of
the app, services, server, and mDNS packages pass, as do focused publication and
WebSocket race checks, the release UI build, and a PIE daemon build embedding it.

Real-dialog tests also exposed Flutter 3.44's requirement for a Material ancestor
below colored containers around ExpansionTiles. Two transparent Material wrappers
retain the existing layout and colors; no framework diagnostics are suppressed.

These are component and local integration checks. Installed-host verification
requires a new build and an actual update through the portal, followed by checks
of both the canonical and custom-domain routes. The memory-throttling issue can
still affect the backend independently of publication restoration.
