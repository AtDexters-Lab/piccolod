# Staged Modify App rootfs attachment failure

## Observed incident

On 26 September 2026, landing's Modify App operation on v0.2.46 attempted
to replace its main image while retaining the network anchor. Diagnostic
`piccolod-diagnostic (21).log` records two candidate failures at 21:45:49
and 21:49:53 IST:

```text
exec: "/pause": stat /pause: no such file or directory
```

Both operations subsequently reattached the previous anchor and main rootfs,
recreated the previous containers, restored their HTTP proxy, and returned
HTTP 500. This was a runtime-switch failure before candidate application
startup. The earlier memory relief remains a separate concern.

## Cause and history

`stageManifestUpdateRootfs` attaches current volumes and saves mount handles
alongside newly staged volumes. The installed-app transaction then quiesces
the previous runtime. Quiescence detaches its active rootfs, including the
unchanged network anchor. Staged recreation handed the earlier handles to
`installContainerGroup`, which trusted their mount paths without reattachment.

Commit `c375166` (19 July 2026) changed transaction stopping to full runtime
quiescence, invalidating an assumption in the existing staged path. Regular
image updates and generic recreation already reacquired unchanged volumes.
The same-digest alias case also matters: a newly selected image reference can
reuse a previous active volume, so quiescence can invalidate that selected
service handle as well as the anchor.

The storage manager already has the required operation: `AttachRootfs`
serializes per volume, reloads metadata, probes kernel state, validates mounts,
and attaches detached volumes. A `RootfsHandle` is a current observation of a
mount, not an attachment lease.

## Required contract and repair

- Callers choose exact rootfs volume identities and retain their ownership,
  transaction, and rollback responsibilities.
- The shared group installer reacquires every consumed prebuilt volume by its
  selected `VolumeID` before artifact preparation, local rootfs preparation,
  or candidate container creation.
- Reacquisition returns installer-local handles. The caller's saved handles,
  image configuration, and selected volumes are not rewritten.
- Missing selection identity, attachment errors, mismatched returned identity,
  or missing returned mount paths fail the operation. There is no fallback to
  stale paths, image-tag resolution, or a different rootfs volume.
- Prebuilt volumes remain caller-owned. Partial attachment does not authorize
  the installer to detach or destroy borrowed volumes; existing rollback and
  recovery handle the failure.

This boundary serves staged Modify, config recreation, regular image updates,
rollback/recovery, listener replacement, and workspace cloning. It uses existing
lifecycle serialization and storage authority; it does not add a mount lease,
new journal, retry owner, or lifecycle state machine.

### Candidate cleanup and transaction-owned listener allocations

The VM's induced candidate-start failure exposed a second cleanup boundary:
`removeUncommittedContainerGroup` called unconditional `DeactivateApp` while
the installed-app transaction held a publication suspension. That withdrew
the already-suspended endpoints from the registry and released their ports.
Rollback correctly restored the previous volumes but allocated different
listener ports (`35002`/`15002` became `35003`/`15003` in the observed run).

Candidate cleanup must preserve allocations owned by the suspended transaction,
while still removing candidate containers and transient identity. The existing
`DeactivateAppUnlessSuspended` operation supplies this contract. Ordinary
unsuspended cleanup still deactivates endpoints; the transaction retains its
existing responsibility for resuming publication or final failure cleanup.
The rollback gate must preserve the original listener bindings and serve the
old HTTP marker and data through the original port, rather than hiding port
drift by probing a newly allocated endpoint.

## Validation

The regression fixture models real attachment lifetime: detach removes mounted
contents, and reattach returns a fresh path. On the unchanged implementation,
both changed-main/unchanged-anchor-and-service and same-digest-alias Modify
cases fail with missing `/pause`, followed by successful old-runtime rollback.
Both pass with the shared installer repair.

Boundary tests also check complete selection preflight, attachment failure
before any preparation or candidate creation, fresh writable workspace handles,
preserved image config, unchanged caller maps, and caller-owned partial
attachment/candidate-failure cleanup. Full app/container suites and focused
race checks pass.

Real-VM acceptance uses the alpha `modify-app-image-update` stage: controlled
OCI fixtures, changed main plus unchanged sidecar/anchor, exact digest and
same-digest alias, live raw/idmapped/container mounts, HTTP version marker,
persistent sentinel, and induced OCI startup failure followed by rollback. The
existing service/workspace and image-update/rollback stages provide adjacent
coverage. The results and their proof boundaries are recorded below.

The real alpha VM uses QEMU/KVM, kernel `7.0.2-1-default`, and Podman `5.7.1`.
The released v0.2.46 baseline installs the fixture, serves v1, and passes
configure/dry-run. The image switch returns HTTP 500 with the same missing
`/pause` network-anchor error as the incident. The failed-switch snapshot
confirms rollback restored the previous volume identities, image digests,
and live mounts. Earlier attempts that failed fixture validation or access
checks are excluded from this reproduction evidence.

The local test binary is built from base `a66da4e` plus this change, with version
`v0.2.46-rootfs-boundary`. Its SHA-256 is
`54e69f6ed291f0c86643dbed96ff8155084d42c9aba0e44838a6164c8f40b540`.
The VM-only build disables automatic VCS stamping because Go's VCS probe
resolves the parent workspace instead of the worktree; the explicit base,
diff, version, and binary hash identify the tested code.

### Final acceptance results

The final alpha run `1790447023-ef9f2a` passes all 26 checks with both repairs.
A changed main image gets a new volume; the unchanged sidecar and anchor retain
their exact volumes. A same-digest alias reuses the selected volumes. A candidate
whose `/proc` is a regular file fails before PID 1 with the expected OCI mount
error. Rollback restores the prior alias, selected volumes, digests, and live
mounts. The original primary HostBind `15000` and PublicPort `35000` remain
unchanged through every operation; HTTP on that original port serves v2 and
retains the persistent sentinel. Transaction, fixture app, registry process,
configuration, and run-owned retained rollback volume cleanup all pass.

Adjacent alpha stages pass: service app 11 checks, workspace app 10 checks,
current-image update/no-op refresh 13 checks, and post-reboot functional/storage
checks 6 and 3 respectively. The workspace VM stage covers installation and
uninstallation; fresh writable clone-handle behavior is unit-test evidence.
The existing image-update stage does not prove a changed-image switch; the new
Modify stage supplies that proof.

The final app, container, service-manager, and fixture parser suites pass,
along with focused rootfs/publication race checks. Baseline publication tests
fail with listener allocation drift, including a full Modify rollback test;
the repaired tests preserve original bindings and resume publication. Independent
implementation review and the final Codex CLI review report no actionable
findings. The CLI sandbox could not bind sockets for its full service suite;
the primary ran that suite successfully with actual socket access.

### Production-image smoke

A separate QEMU/KVM guest uses the published Piccolo OS VirtualBox image
(SHA-256 `d13e92784d4f30406e8c6cea2baca4f1dfb0fe0244f2d8b020bc3b0fee1f1a11`)
with a task-owned writable overlay. Only the selected boot snapshot's piccolod
binary was replaced with the final binary above. Its owner, mode, and SELinux
label were preserved, the snapshot was restored read-only, and the health-check
policy files were unchanged. This is production-image component evidence,
not a package release or installed-server proof.

The production harness still calls the authenticated health-detail endpoint
before setup. A temporary harness copy uses the current public health-ready
endpoint and an isolated cookie/log directory; the repository's production
harness was not changed. Boot passes 8 checks, pre-setup 6, and setup 7.
The initial post-setup smoke passes apps/session/version checks but fails
readiness: persistence stays at `control store unlocked; recovery finalizing`
for over 120 seconds, even though lifecycle is Ready and storage/app-manager
are healthy. The harness intentionally repeats setup; its second POST returns
409 and emits another raw unlocked event after the first setup reached Ready.
This event-order explanation is a source/log inference, not a proven cause.
The initial readiness failure remains recorded rather than counted as a pass.

A clean authenticated OS reboot returns HTTP 200 and the guest auto-unlocks.
After that reboot, health-ready is true with every component OK and all four
post-setup checks pass (apps, authenticated session, version, readiness).
The earlier failed sample is not replaced by this pass. The setup/repeated-event
health observation needs separate investigation; it is distinct from the
candidate application-process readiness limitation below. No startup-health
or application-readiness policy was changed by this repair.

### Separate readiness observation

An early negative fixture selected an image whose native entrypoint was
`/missing-fixture`, yet Apply returned HTTP 200. This is not accepted rollback
evidence. Source inspection shows service containers use `--init`, Podman start
success is accepted without waiting for its child command, and transaction
readiness probes the anchor's TCP forwarding socket. Those checks may accept a
candidate whose application process immediately exits. The actual candidate
state was not captured before cleanup, so that explanation remains an inference.

Changing readiness semantics is outside this attachment repair. A separate
investigation should capture the candidate image config, container state/exit
code and logs, then distinguish container start, process survival, and application
readiness. The attachment regression's rollback gate instead uses a failure
before PID 1 starts and requires evidence of that exact OCI fault.

## Release and rollback

No persisted-state migration is required. The binary uses the existing volume
identities, transaction records, and storage attach implementation. Deployment
must retain current manifests, ledgers, and volumes. Reverting the binary
restores the older behavior and its staged Modify limitation; it does not
require deleting or reconstructing app data.
