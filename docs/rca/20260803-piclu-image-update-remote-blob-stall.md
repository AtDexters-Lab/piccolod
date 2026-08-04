# RCA: Piclu image update stalled on remote blob delivery

- **Status:** Immediate cause and request amplification confirmed; exact
  throughput-collapse mechanism unresolved
- **Date:** 2026-08-03
- **Impact:** One installed app update remained on `Pulling new image` for
  approximately 16 minutes; duplicate backend executions continued after the
  successful update
- **App:** Piclu, six service images on the mutable tag `v0.1.0`
- **Registry:** `git.piccolo0.atdexters.com`
- **Related RFCs:**
  - `docs/rfc/20260609-app-detail-operation-state.md`
  - `docs/rfc/20260611-service-app-update-v2.md`

## Executive finding

The update did not fail because Gitea rejected a request, Piccolo lost network
connectivity, the Nexus host exhausted CPU or disk, Podman spent the time
unpacking an image, or the `pasta` process failed. A single 44.5 MiB shared
Piclu layer was delivered intermittently at roughly 0.5 Mbps over the remote
registry path even though the same layer completed in 2.07 seconds during the
planning pull immediately before the mutation pull.

The first slow `piclu-admin` pull occupied Podman for 12 minutes 34 seconds. A
duplicate execution then pulled the same image for another 11 minutes 6
seconds. Gitea returned HTTP 200 in both cases, but its handler took 3 minutes
46 seconds and 4 minutes 3 seconds respectively to stream the layer. The exact
Nexus client connections remained active until the consuming Podman processes
finished, with periodic downstream writes preventing the Nexus server's
60-second idle timeout from closing them.

This proves that the dominant delay was a trickling response across the remote
delivery path, not a completed download followed by local image unpack. The
available historical evidence does not distinguish transient TCP
loss/congestion from an unreported Piccolo portal/Nexus scheduling or flow-
control stall. Nexus emitted no buffer, credit, write, bandwidth-gate, or
backend-session error for either affected connection.

The incident was amplified at two independent layers:

1. One logical image update downloads changed images while resolving the plan,
   destroys that ephemeral Podman runtime, then downloads them again in a fresh
   runtime while applying the plan.
2. Although the user initiated the update once and the Flutter service method
   sends one POST, Piccolod observed 17 executions of the update endpoint. The
   replay owner was not proven. The long-running handler has a 30-minute
   operation context while the secure loopback HTTP server has a 60-second
   write timeout, which is a confirmed lifecycle mismatch and a likely replay
   boundary, not by itself proof of replay ownership.

The original transaction committed successfully at 16:11:25 IST, created data
snapshot generation 43, refreshed all six service images, and restored the app
to a ready state. No reinstall, data deletion, daemon restart, or process kill
was required.

## User impact

- App detail remained on `Pulling new image`, with no transfer rate, affected
  service, or remaining-work signal.
- The original update took 16 minutes 20 seconds end to end.
- The app remained installed and its persistent data was preserved.
- Duplicate update executions continued consuming registry, Nexus, Podman, and
  rootfs-planning work after the successful transaction.
- Thirteen duplicated requests eventually returned HTTP 500 after their
  30-minute operation contexts expired. Four executions returned HTTP 200,
  including later no-op results after the active rootfs already matched the
  registry digests.

## Timeline

All timestamps below are normalized to IST. Nexus-host journal entries were
recorded in UTC and converted by adding 05:30.

| Time | Event |
| --- | --- |
| 15:52:00-15:52:12 | Six Piclu `v0.1.0` images are published. The 44.5 MiB layer `sha256:08f84a42...` is cross-mounted into the Piclu repositories. |
| 15:55:04 | Planning pull of the affected `piclu-admin` layer completes from Gitea in 2.068 seconds. |
| 15:55:05 | Original `POST /api/v1/apps/piclu/update` execution begins. |
| 15:55:16 | Mutation pull opens Nexus client `421d221d-b9c8-46f7-8306-609d05a02da8` for the registry route. |
| 15:59:02 | Gitea completes the affected blob GET with HTTP 200 after 225.973 seconds. |
| 16:07:50 | Nexus and the registry-host Piccolo portal close client `421d...`; consuming Podman records completion after 754.577 seconds. |
| 16:11:21 | Piccolod creates `snap-app-piclu--gen43`. |
| 16:11:25 | Original update reports `image updated for 6 service(s)` and returns HTTP 200 after 16 minutes 20 seconds. |
| 16:11:26 | A duplicate execution opens Nexus client `89e56f08-5310-496a-8cee-c128c78845a3` for the same registry route. |
| 16:15:29 | Gitea completes the same blob GET with HTTP 200 after 243.025 seconds. |
| 16:22:32 | Nexus and the registry-host Piccolo portal close client `89e5...`; consuming Podman records completion after 666.411 seconds. |
| 16:24:06 onward | Some duplicate requests observe that registry digests already match active rootfs and return no-op HTTP 200 responses; others expire at 30 minutes with HTTP 500. |

## Evidence

### The update completed transactionally

The consuming-device journal contains the commit sequence:

```text
2026-08-03 16:11:21 thin snapshot created: piccolo-data-vg/snap-app-piclu--gen43
2026-08-03 16:11:25 INFO: update piclu: image updated for 6 service(s)
2026-08-03 16:11:25 | 200 | 16m20s | POST "/api/v1/apps/piclu/update"
```

The later absence of `app_transition_v2.json` and
`image_update_transaction.json` was therefore normal post-commit cleanup, not
evidence that the update had never started.

### One shared layer dominated both slow pulls

The affected digest was:

```text
sha256:08f84a42d4d1982d8a89f47018bae67fb0d1c389a8789082446d45c26247f6be
```

An authenticated registry HEAD request reported `Content-Length: 46679741`,
or approximately 44.5 MiB. Gitea recorded:

```text
15:55:04 GET .../sha256:08f84a42... 200 OK in 2068.4ms
15:59:02 GET .../sha256:08f84a42... 200 OK in 225972.9ms
16:15:29 GET .../sha256:08f84a42... 200 OK in 243025.2ms
```

The corresponding Podman wall times imply approximately 0.50 Mbps and
0.56 Mbps end-to-end. The immediately preceding 2.068-second response implies
approximately 180 Mbps for the same bytes. Later requests for the same digest
also varied from sub-second completion to tens or hundreds of seconds. This is
an intermittent per-connection behavior, not a stable configured bandwidth
limit or an intrinsically oversized image.

Gitea returned HTTP 200 for the affected GETs. Its log contains no registry
5xx, authentication failure, missing manifest, panic, database lock, disk-full,
or storage I/O error associated with the pulls. The ordinary Registry V2
`401 -> token -> 200` exchange is expected authentication negotiation.

### The exact remote connections match the Podman wall times

For the original pull, Nexus recorded:

```text
15:55:16 route git.piccolo0.atdexters.com client 421d221d-... connected
16:07:50 client 421d221d-... disconnected
```

The registry-host Piccolo portal recorded the same ClientID and timestamps:

```text
15:55:16 [piccolo-portal] Received 'connect' for ClientID 421d221d-...
16:07:50 [piccolo-portal] Received 'disconnect' for ClientID 421d221d-...
```

The duplicate pull produced the same alignment for ClientID `89e56f08-...`
from 16:11:26 through 16:22:32. The two portal connections had no associated
write-buffer, enqueue, credit, local-read, or stall warning. Two enqueue
warnings in the queried period named different ClientIDs and are unrelated.

The deployed Nexus server was `v0.3.15`, revision
`a4f88eaf0eb9566caf74c1890738461a008fc9a2`, including the earlier symmetric
credit-loss and read-pump head-of-line fixes. Its configured idle timeout was
60 seconds. In this version, a response-side successful write after the read
deadline began causes the idle deadline to roll forward. The affected
connections survived for more than 11 minutes without idle timeout, providing
strong evidence that response bytes continued to make downstream progress at
least periodically throughout the Podman wall time.

### Nexus was not resource- or policy-limited

The Nexus configuration did not set `totalBandwidthMbps`; bandwidth management
was therefore unlimited. The affected ClientIDs had no:

- `gate-blocked` warning;
- forward-credit timeout;
- client write-buffer overflow;
- failed data send or failed client write;
- backend reconnect or session failure.

Historical `sysstat` samples for the incident window showed approximately:

- 95.5 percent CPU idle;
- 0.06 percent I/O wait;
- 0.07 percent disk utilization;
- zero `eth0` receive/transmit errors, drops, carrier errors, or FIFO errors.

The Nexus kernel journal contained no incident-window entries. These samples
do not contain per-connection TCP retransmission, RTT, or congestion-window
history, so they cannot exclude WAN packet loss.

### The `pasta` shutdown messages are unrelated health-check noise

The registry host emitted repeated messages such as:

```text
pasta: Flow N (TCP connection (spliced)): shutdown() on HOST
pasta: Transport endpoint is not connected
```

They occur on the regular `:07`, `:22`, `:37`, and `:52` cadence and align with
short Gitea SSH-listener probes that connect and close without authenticating.
They do not name either affected registry ClientID or the blob connection. The
message means the opposite endpoint had already closed when `pasta` attempted
shutdown; it is not evidence that the registry transfer failed.

### One logical update pulls each changed image twice

`UpdateImage` first calls `resolveUpdateImageRootfsPlan`. That function creates
an ephemeral flatten runtime and calls `PullImage` for each mutable service
image so it can inspect the resolved digest and compare it with active rootfs.
The runtime is then cleaned up.

`updateServiceModeImage` creates a new ephemeral flatten runtime and calls
`PullImage` again for every changed image before rootfs materialization. The
second pull is therefore part of the current operation design, not a duplicate
button click. Isolated Podman storage means the successful planning download
did not cache the layer for the mutation runtime.

This doubled the opportunity for a transient transport stall and made the
mutation pull pay again for bytes that had completed successfully seconds
earlier.

### The endpoint executions were additionally replayed

The consuming-device diagnostic contains 17 completed executions of:

```text
POST /api/v1/apps/piclu/update
```

Four returned HTTP 200 and thirteen returned HTTP 500 after exactly 30
minutes. The Flutter `updateApp` method issues one POST and stops awaiting it
after ten seconds, treating that timeout as expected for an image pull. The
backend handler continues synchronously under a server-owned operation context
for up to 30 minutes. It does not make the request idempotent or attach a
duplicate call to the active same-app operation.

The secure portal HTTP server has a 60-second write timeout. New backend
executions appeared at approximately one-minute intervals, so the timeout is a
relevant boundary. The evidence does not identify which browser, HTTP, portal,
or transport component replayed the POST. The RCA therefore treats replay
ownership as unresolved and the absence of backend single-flight/idempotency as
the proven amplification defect.

## Causal chain

```text
mutable Piclu v0.1.0 images are republished
        ↓
operator starts Refresh current image once
        ↓
planning phase pulls each mutable service image into an ephemeral runtime
        ↓
planning pull of the affected 44.5 MiB layer completes in 2.07 seconds
        ↓
mutation phase creates a fresh runtime and pulls the same images again
        ↓
one registry response falls to roughly 0.5 Mbps over the remote delivery path
        ↓
progress remains at Pulling new image for more than 12 minutes
        ↓
the original update eventually stages rootfs, snapshots data, switches runtime,
and commits successfully
        ↓
replayed POSTs start additional full update executions without same-app
single-flight/idempotent attachment
        ↓
duplicate pulls continue after success; later requests either observe a no-op
or expire at their 30-minute limit
```

## What is proven and what remains unknown

### Proven

- The original six-service update succeeded and committed snapshot generation
  43.
- A 44.5 MiB shared layer dominated both long `piclu-admin` pulls.
- End-to-end delivery of that layer fell to approximately 0.5-0.56 Mbps even
  though the same layer traversed the same route in approximately two seconds
  immediately beforehand.
- Gitea returned HTTP 200 and did not report a registry or storage failure.
- Response-side activity continued through the Nexus connection during the
  long Podman wall time; pure post-download unpack is not the dominant delay.
- Nexus had no configured bandwidth cap and no resource exhaustion, interface
  error, or relevant flow-control/write error.
- The affected Piccolo portal connections had clean connect/disconnect
  lifecycles.
- The image-update implementation pulls changed images once during planning
  and again during mutation using separate ephemeral runtimes.
- Seventeen backend executions occurred although the operator initiated the UI
  action once; the backend did not collapse them into one active operation.
- The visible `pasta` shutdown messages belong to unrelated periodic probes.

### Not recoverable from this incident

- Per-connection TCP retransmissions, RTT, congestion window, and zero-window
  history on either side of Nexus.
- Per-ClientID Nexus/portal byte counters and queue/credit wait durations.
- Whether the final throughput collapse occurred on the registry-host portal
  WebSocket, Nexus scheduling, Nexus-to-consumer TCP path, or consumer-side
  portal/Podman ingress.
- Which component replayed the original POST at roughly one-minute intervals.

The final mechanism must not be recorded as “Nexus bug,” “WAN outage,” “Gitea
failure,” or “Podman unpack” without new evidence.

## Immediate recovery and disposition

Waiting was safe in this incident because the transaction continued making
progress, the existing app remained recoverable, and no error indicated data
or rollback-journal corruption. The operation self-completed. No manual process
termination, service restart, app reinstall, or transaction-file deletion was
performed.

If a future operation shows no byte or task progress, operators should inspect
the transition and image-update transaction records before terminating
Piccolod or deleting any artifact. A pending transaction may own rollback or
forward-completion state.

## Preventive remediation

### 1. Make same-app updates single-flight and idempotent

- Treat the task ID as an operation identity, not only a progress-stream key.
- A retry with the same task ID should reattach to the same operation.
- A second task for an app with an active mutation should return or attach to
  the active operation rather than launching another pull.
- Preserve an explicit conflict result where attachment is not safe.

### 2. Download each changed image once per logical operation

Choose and validate one design that preserves digest-drift and rollback safety:

- carry a staged content-addressed pull/runtime artifact from planning into
  mutation; or
- resolve the plan's manifest/config digest without downloading all layers,
  leaving the single full pull to mutation.

Do not remove the second pull mechanically unless the replacement still proves
that the staged rootfs corresponds to the planned digest.

### 3. Decouple operation lifetime from one long HTTP response

- Accept an image update as a task-backed operation and return submission state
  promptly.
- Let app detail reattach by task ID across dialog dismissal, request timeout,
  and browser reload while daemon task state exists.
- Align secure-loopback timeouts with the submission contract so an HTTP
  boundary cannot create ambiguous retry behavior.
- Preserve “status unknown/checking” when the UI loses progress rather than
  declaring completion or silently resubmitting.

This is consistent with `docs/rfc/20260609-app-detail-operation-state.md`.

### 4. Add transport attribution before the next recurrence

For every routed TCP ClientID, record bounded counters at disconnect:

- bytes in each direction;
- connection duration and calculated throughput;
- queue/credit wait duration and peak depth;
- downstream TCP retransmissions, RTT, congestion window, and zero-window
  state where the platform exposes them;
- route hostname and backend ID without credentials or request contents.

Image-update progress should also name the service/image being pulled and
surface elapsed time. Byte progress is preferable when Podman exposes it
reliably.

### 5. Capture live TCP state on recurrence

Before restarting a service or cancelling the update, capture repeated
`ss -ti` samples for the exact registry connection on both the Nexus and
consumer hosts. Interpretation:

- increasing retransmissions with collapsed congestion window indicates the
  TCP/network path;
- healthy TCP acknowledgement progress with application bytes stalled points
  to portal/Nexus scheduling or flow control;
- completed transfer followed by disk I/O or CPU activity points to local image
  materialization.

Also retain the exact ClientID from the Nexus route log so registry-host portal
events can be correlated without broad journal export.

## Evidence sources

- Consuming-device diagnostic: `piccolod-diagnostic (14).log`
- Gitea application log: `app-git-logs.txt`
- Registry-host diagnostic: `piccolod-diagnostic (15).log` (later health only;
  the incident window had rolled out of its 50,000-line export)
- Registry-host targeted journal for 15:54:30-16:23:00 IST
- Nexus `nexus-proxy-server.service` journal for 10:20-10:58 UTC
- Nexus `/var/log/sysstat/sa03` and deployed binary build metadata
- Read-only Registry V2 HEAD for the affected blob size

These artifacts were inspected during the incident but are not committed to
the repository. The excerpts and identifiers required to preserve the RCA's
claims are included above.

## Remediation Status

- [x] Original update completed and app readiness recovered without manual
  mutation of transaction state.
- [x] Slow blob, size, two affected ClientIDs, and end-to-end durations
  correlated across consumer, Gitea, registry-host Piccolo portal, and Nexus.
- [x] Gitea failure, `pasta` health-probe noise, Nexus host resource pressure,
  configured Nexus bandwidth limiting, and pure Podman unpack rejected as the
  primary explanation.
- [ ] Same-app update single-flight/idempotent attachment not implemented.
- [ ] Duplicate planning/mutation image downloads not eliminated.
- [ ] Task-submission/HTTP lifetime mismatch not resolved.
- [ ] Per-connection transport attribution not implemented.
- [ ] Final TCP-versus-flow-control mechanism awaits recurrence evidence.
