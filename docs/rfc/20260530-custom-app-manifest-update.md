# RFC: Custom App Manifest Update

**Date:** 2026-05-30
**Status:** Reviewed - hybrid v1 ready for implementation

## Scope block

**Problem:** Custom installed apps can receive new images without receiving newer YAML wiring, so changes such as env vars and shared storage mounts require uninstall/reinstall today.
**In scope:** A manual admin action that applies topology-neutral YAML wiring changes to an already-installed custom service-mode app, preserving the app identity, persistent volumes, active rootfs volumes, service set, image references, and listener topology.
**Out of scope:** Post-install input editing, background sync for custom apps, persistent custom input ledgers, workspace apps, service additions/removals, image reference changes, listener topology changes, service startup-order changes, resource policy changes, init-script migrations, service-level OIDC client migration, app data snapshot/rollback, and user-initiated rollback of a successfully-applied manifest update.

## Decision summary

Add an explicit **Apply Manifest YAML** flow for custom apps. The operator supplies the new manifest template and the input values needed to render it. Piccolo reuses the existing install pipeline to render and validate the new definition, then reuses the catalog-sync structural apply path to persist the new rendered definition, recreate the app containers in place, and roll the manifest/runtime state back if the runtime apply fails before commit.

Manifest update rollback uses an operation-specific transient backup, not `app.prev.yaml`. The existing `app.prev.yaml` slot remains owned by image/listener rollback flows so a successful custom manifest update cannot corrupt an existing image rollback snapshot.

V1 is intentionally **manifest/runtime-only recovery**. It does not snapshot or roll back app data. The confirmation UI must say that persistent data is preserved but not snapshotted: if the candidate app mutates data before failure, restoring the previous manifest may not restore the previous app semantics. Data-snapshot rollback is deferred to v2.

This is not custom app background sync. There is no external source of truth to poll, and custom installs deliberately do not persist `install_state.json` today because it would retain plaintext install inputs without enabling any existing sync behavior. V1 therefore treats each custom YAML update as an explicit operator transaction.

## Product behavior

### Operator flow

1. The operator opens an installed custom app and chooses **Apply Manifest YAML**.
2. The operator pastes or uploads the new manifest.
3. Piccolo runs an early eligibility precheck for input-independent v1 rejects such as image-reference changes, service additions/removals, listener topology changes, storage removals/renames, startup-order changes, resource changes, service-level OIDC, and init-script drift when they are visible from the parsed candidate. Template-dependent checks are deferred to dry run.
4. Piccolo parses the manifest schema and returns the input form, using the same system-default preparation used by install, with update-mode changes listed below.
5. Before the input form, Piccolo lists any password/generated inputs that must be provided or explicitly regenerated. The YAML and any entered values are preserved if the operator backs out.
6. The operator reviews every declared input for the new template. Password fields are blank; Piccolo does not prefill or infer secrets from the old rendered manifest. Optional inputs are still materialized into the normalized update request so omitted values cannot silently zero-fill. Each field shows provenance: `Locked current value`, `Re-enter required`, `New manifest default`, or `Entered now`.
7. The `__app_address__` value is pinned to the installed app ID. The UI displays it as locked/read-only, and the backend overrides any supplied value before render.
8. Piccolo runs a dry run and returns a change summary with one of:
   - no-op
   - applicable structural update
   - rejected update with the blocking reason
9. If applicable, the operator confirms apply using the dry-run token for that exact rendered candidate.
10. Piccolo applies the new rendered manifest as a task-backed operation and recreates affected runtime state. If apply fails before commit after the new manifest is persisted, Piccolo restores the previous `app.yaml` from the transient backup and recreates containers from the previous definition.

### V1 apply policy

Allowed in v1:

- service environment changes for existing services
- additive storage/shared-volume changes that existing services can mount at new names/paths
- comment/format/input-schema-only changes that canonicalize to no runtime change

Service-field delta policy for existing services:

- `environment`: allowed; summaries name keys only and never values
- `storage`: add-only; existing storage names and mount paths must stay stable
- `bind_ports`: must be unchanged
- `after`: must be unchanged
- `image`: must be unchanged
- `init`, `oidc_client`, and `init_script`: must be unchanged or absent as required by the rejection rules below

Top-level field delta policy:

- `inputs`: may change; metadata-only changes persist without runtime restart
- `storage`: add-only; existing entries and mount paths must be unchanged
- `listeners`: topology must be unchanged, including listener names, primary marker resolution, guest ports, port claims, remote/public exposure, flow, protocol, and middleware. Listener auth changes are deferred to v2.
- `primary_service`, `resources`, `permissions`, `healthcheck`, `auth`, `x-piccolo`, `type`, and `workspace_name`: must be unchanged in v1
- `app_config`: rejected in v1; runtime config update is covered by the separate app config RFC track

Rejected in v1:

- catalog-backed apps, because they already use catalog manifest sync
- disabled apps, because recreating containers would contradict the stopped intent
- workspace-mode apps
- image reference changes
- listener topology or auth changes
- added services, because new service rootfs materialization requires image pull semantics, not manifest-only recreate
- removed services, because teardown/data-retention semantics need explicit operator confirmation beyond v1
- storage removals, renames, or mount-path changes for existing storage, because preserving bytes is not the same as preserving the app's semantic data location
- resource, permission, health check, auth, extension, app config, primary service, bind port, or startup-order changes
- current or candidate service-level `oidc_client`, because custom updates do not have persisted plaintext OIDC credentials for deterministic re-render
- service `init` changes, because changing startup/init mode composes with rootfs and one-time initialization semantics outside manifest-only v1
- init-script additions, removals, content changes, or config changes, because app updates do not re-run one-shot init scripts

Piclu's immediate diagnostics case fits the allowed path when the updated YAML keeps the same service image references, existing services, and listener topology, while adding env/storage wiring that should be materialized by container recreation.

## API shape

### Prepare inputs

Reuse the existing configure shape, adding an optional installed-app context:

`POST /api/v1/apps/:name/manifest/configure`

Input:

```json
{
  "app_definition": "...yaml..."
}
```

Output:

- the new manifest's input schema after update-mode smart defaults and system defaults
- any input-independent eligibility rejection that can be proven before values are entered
- a secret/generated-input preflight summary listing values that must be re-entered or explicitly regenerated
- an indication that password and generated values are not recovered from the existing install
- `__app_address__` marked locked to the installed app ID when present
- provenance labels for each input field and a banner stating previous custom input values are not remembered

### Dry run

`POST /api/v1/apps/:name/manifest/dry-run`

Input:

```json
{
  "app_definition": "...yaml...",
  "inputs": {},
  "regenerate_inputs": []
}
```

Output:

- base manifest hash used for stale-confirmation detection
- active rootfs/runtime fingerprint used for stale-confirmation detection
- opaque non-decodable dry-run token covering app ID, base manifest hash, active runtime fingerprint, canonical rendered candidate digest, normalized input presence, regeneration choices, and policy decision
- rendered app identity
- diff kind
- applyability
- blocking reason when rejected
- operator-facing summary of runtime-affecting changes, grouped as:
  - Will change
  - Will restart
  - Will preserve
  - Expected interruption
  - Rejected, when applicable

Dry-run rejection messages must identify the offending diff item and the next action. Examples: image reference changed -> keep the same image reference or reinstall; new service added -> reinstall required; init script changed -> unsupported in v1.

Dry-run summaries are redacted artifacts. They must not include raw input values, password values, generated secrets, env values, app config values, or full rendered YAML. They name fields, keys, services, listeners, and change classes only. Dry-run tokens must also be redacted artifacts: opaque identifiers or handles, not signed blobs containing rendered YAML or secret-bearing values.

### Apply

`POST /api/v1/apps/:name/manifest/update`

Input:

```json
{
  "app_definition": "...yaml...",
  "inputs": {},
  "regenerate_inputs": [],
  "base_manifest_hash": "...",
  "runtime_fingerprint": "...",
  "dry_run_token": "..."
}
```

Behavior:

- admin-only and unlocked
- task-backed, using the existing task/progress stream pattern used by install/update
- serialized against app reconcile/sync/update operations
- re-renders the request and recomputes the dry-run token before apply
- rejects with conflict if the current manifest hash differs from the dry-run `base_manifest_hash`
- rejects with conflict if active rootfs/runtime state differs from the dry-run `runtime_fingerprint`
- rejects with conflict if the recomputed dry-run token differs from the supplied `dry_run_token`
- returns the final app state and the applied change summary through task completion

All three endpoints are admin-only and require unlocked storage. Configure and dry-run read installed app state and may receive secret-bearing input material, so they fail closed while locked instead of accepting requests against partial state.

## Backend design

### Render source model

Custom manifest update uses `RunInstallPipeline` with:

- `RawTemplate`: the operator-supplied YAML
- `UserInputs`: the request inputs after backend normalization
- `SystemContext`: freshly built from the current host context, with identity drift blocked by post-render checks
- `InstanceID`: the existing app instance ID, so self-collision checks skip the installed app
- `ExistingOIDC`: nil in v1; current or candidate service-level OIDC apps are rejected before apply

Custom manifest update runs the pipeline with OIDC generation disabled. Current or candidate service-level `oidc_client` is rejected before calling any path that can generate or persist OIDC credentials.

Update-mode normalization:

- overwrite `__app_address__` with the existing instance ID before render
- suppress automatic generation for generated inputs in update mode
- require every declared input to be present in the normalized request before dry run/apply
- require an explicit operator value or an explicit `regenerate_inputs` entry for generated inputs
- make `regenerate_inputs` a secondary, warning-gated path because regenerating a value can break existing sessions, data, or auth
- reject any candidate whose primary listener identity differs from the installed app ID after render
- reject identity-bearing drift that cannot be explained by the pinned app ID

The output is a rendered, canonical `AppDefinition`, not a stored custom install state. V1 does not create or update `install_state.json` for custom apps.

Generated-value materialization:

- dry run materializes any values requested through `regenerate_inputs`
- the materialized candidate is stored server-side under the dry-run token until apply, cancel, expiry, or replacement by a newer dry run for the same wizard session
- apply consumes the exact stored candidate for that token rather than generating new values
- the token never exposes generated values to the client

### Identity guard

The rendered candidate must preserve:

- app instance ID
- primary listener name
- primary listener marker resolution
- workspace name is irrelevant because workspace apps are rejected in v1

Service-level OIDC redirect material would also be identity-bearing, but current or candidate service-level `oidc_client` makes the app ineligible for v1 before render/apply. Non-primary listener names, listener auth rules, port claims, flow/protocol, and remote/public exposure must also remain unchanged in v1.

V1 uses a narrow definition of app identity: instance ID plus primary listener identity. Other externally visible or identity-adjacent rendered fields, such as public URL strings in environment values, are runtime/config changes rather than identity-preservation guarantees. Dry-run summaries must flag such key-level changes without values under Will change and Expected interruption so the operator can decide whether to proceed.

### Source-agnostic manifest apply

Factor catalog sync so the following can be called by both catalog sync and custom manifest update:

1. accept current app instance, current definition, newly-rendered definition, and an apply source label
2. canonicalize and classify the diff
3. enforce the caller's allowed diff policy
4. write an operation-specific transient backup of the current `app.yaml`
5. persist the new rendered definition
6. for runtime no-op but manifest-metadata changes, persist the canonical rendered definition without recreating containers
7. recreate containers in place for structural-no-image deltas
8. on failure before commit after persist, restore the previous manifest and recreate containers from it

Catalog sync keeps its own fetch/hash/throttle/install-state/legacy-backfill logic around that shared core. Custom manifest update supplies the already-rendered candidate and does not touch catalog sync metadata fields.

The shared core must keep backup ownership explicit:

- catalog sync may continue using its existing rollback material
- image rollback keeps ownership of `app.prev.yaml`
- custom manifest update uses a separate transient file
- successful apply deletes the transient backup after the final result is recorded
- failed apply or failed restore retains the transient backup until successful restore, successful retry, or explicit operator cleanup

### Durable transaction record

Before persisting the candidate manifest, custom manifest update writes a durable transaction record next to the app metadata. The record includes:

- operation ID and phase
- previous manifest hash
- candidate manifest hash
- dry-run token
- runtime fingerprint
- transient backup path
- timestamps and last error

Phases:

- prepared
- candidate_persisted
- recreating_runtime
- committed
- restoring_previous
- restore_failed

Boot-time recovery runs before normal app reconcile. If a transaction record exists and phase is not `committed`, Piccolo restores the previous manifest from the transient backup and reconciles runtime from that previous definition. If restore fails, the app is marked error, the backup and transaction record are retained, and normal reconcile must not silently promote the candidate. If phase is `committed`, startup may clean up the completed transaction and backup.

### Diff classification

Reuse the catalog-sync classifier as the starting point:

- `none`
- `oidc_library_only`
- `structural_no_image`
- `image_only`
- `structural_with_image`

For custom v1, only `none` and `structural_no_image` are eligible after the stricter custom policy checks pass. `oidc_library_only` is a catalog-sync apply class only, because custom v1 rejects current and candidate service-level `oidc_client`. `image_only` and `structural_with_image` are rejected with guidance that image-reference changes require reinstall or a future combined image+manifest update.

### Container recreation semantics

Structural apply uses the existing in-place recreate path:

- reuse the existing app volume layout
- reuse active rootfs volumes for existing services
- remove containers according to the definition currently materialized
- assert listener topology is unchanged before container removal
- install the container group from the new definition
- persist new container IDs and primary service metadata

Persistent app data volumes are not purged. V1 does not add, remove, or rename rootfs volumes. Storage changes are add-only: existing storage names and mount paths must remain stable.

### Task and recovery states

Apply emits progress phases that the UI can surface:

- validating manifest
- applying manifest
- recreating containers
- verifying runtime
- restoring previous manifest after apply failure
- restored previous manifest
- restore failed, operator action needed
- complete

The v1 success gate is runtime-level only:

- the container group must be recreated and reach the same running/ready condition used by the existing install/recreate path
- manifest-declared health checks are not executed by this RFC
- listener/backend health links may be shown after apply, but they are informational and do not drive automatic restore

V1 does not provide a manual manifest rollback after a technically successful but app-semantically-bad update. The result screen must make the verification level explicit: container readiness was verified, app-level health was not. It must keep the redacted dry-run/apply summary available for troubleshooting.

## UI design

Add an **Apply Manifest YAML** action on installed custom app detail pages only. Rename or relabel the existing image action as **Update Image** anywhere both actions can appear.

The flow mirrors install rather than image update:

1. YAML editor/upload
2. secret/generated-input preflight
3. generated input form
4. dry-run summary
5. confirmation
6. apply progress/result

The dry-run summary must be explicit when inputs are re-entered rather than recovered. Password inputs should show as blank required fields. Generated inputs must not be silently regenerated. The default path is "enter existing value"; "generate new value" is a secondary action with warning copy and confirmation. The confirmation step should call out that containers will be recreated using existing images/rootfs, no image pull will happen, listener topology is unchanged, persistent app data is preserved but not snapshotted, and restore is manifest/runtime-only.

Dry-run summaries use the same grouping as the API: Will change, Will restart, Will preserve, Expected interruption, and Rejected. The summary must name affected services, added storage, unchanged image refs/rootfs, persistent data preservation, existing exposed listeners that may disconnect during service restart, and "no image pull." The Will preserve section must include listener topology unchanged: same listener names, primary listener, ports/claims, public exposure, flow/protocol, auth/middleware, and routing.

Failure states must be visible: applying manifest, apply failed/restoring previous manifest, restored previous manifest and containers/persistent data not rolled back, restore failed/action needed. Rejection states must include actionable recovery guidance.

The result state must distinguish runtime changes from metadata-only applies. For metadata-only applies, use result copy equivalent to: `Manifest metadata stored; no runtime changes; no services restarted.` For runtime applies, state that container readiness passed and app-level health was not verified. Result actions include opening logs and rerunning dry run/apply with corrected YAML.

Secret-bearing form values persist only while navigating inside the wizard. They are cleared on cancel, successful apply, terminal failure dismissal, or page/session close.

Do not show this action for catalog apps in v1; catalog apps keep their existing sync status/actions.

## Site list

- `internal/server/gin_server.go`: route registration under the existing app admin group.
- `internal/server/gin_app_handlers.go`: request parsing, configure/dry-run/apply handlers, admin/unlocked error mapping, and task/progress integration.
- `docs/api/openapi.yaml`: request/response schemas and paths for configure, dry-run, and update.
- `docs/api/openapi_validation_test.go` and `internal/server/openapi_middleware.go`: route coverage and validation behavior for the new API surface.
- `internal/app/install_pipeline.go`: render pipeline reused with existing app identity and caller-supplied inputs.
- `internal/app/catalog_sync_apply.go`: diff classifier, structural apply, rollback, and container recreate are factored into source-agnostic pieces.
- `internal/app/catalog_sync.go`: catalog loop and sync metadata remain catalog-only; shared apply must not advance catalog hashes for custom updates.
- `internal/app/filesystem.go`: `app.yaml` persist/load semantics, custom manifest-update transient backup, and durable manifest-update transaction record.
- `internal/app/tuple.go` and tuple-state persistence: active rootfs/runtime fingerprint for dry-run/apply stale-confirmation checks.
- `internal/app/container_group_install.go`: container recreation continues to skip init replay; v1 blocks init-script drift.
- `internal/app/multi_container.go`: new rendered service env/storage state materializes during recreate.
- `internal/app/task_progress_constants.go`: task type and phases for manifest apply.
- `internal/app/app_manager.go`: update/reconcile serialization, app status transitions, rootfs reuse, and existing image-update behavior remain separate.
- `internal/services`: existing listener registry is used to validate listener topology remains unchanged.
- `ui/lib/core/services/app_service.dart`: client methods for configure, dry run, and apply.
- `ui/lib/features/apps/app_detail_view.dart`: custom-app-only Apply Manifest YAML action entry point.
- `ui/lib/features/apps/install_wizard.dart` or a sibling wizard: reusable YAML editor/input form/dry-run confirmation flow.
- `ui/lib/core/models/app_models.dart`: expose enough app metadata to distinguish catalog vs custom apps if not already available.
- task/progress UI surfaces: redacted phase/status payloads and final verification level.
- `docs/app-platform/README.md` and operator docs: document custom YAML update limits and recommended image-then-YAML flow.
- Tests under `internal/app`, `internal/server`, and `ui/test`: behavior and UI coverage listed below.

Reference-only anchors: `internal/server/gin_app_sync_handlers.go` for manual-sync timeout/error conventions, and `internal/oidc` for the reject-before-generation boundary. V1 should not reuse those routes or add custom service-level OIDC credential migration.

## Invariants

- App identity does not change. The existing instance ID remains the app ID and primary listener identity for the update.
- The backend pins `__app_address__` to the instance ID and rejects rendered identity drift.
- Persistent data volumes are preserved and never purged by manifest update.
- Persistent data volumes are not snapshotted or rolled back by manifest update.
- Existing storage names and mount paths remain connected to the same semantic data locations; v1 storage changes are add-only.
- Image/rootfs state is not mutated by manifest update v1.
- Listener topology and app resource policy are not mutated by manifest update v1.
- Catalog sync state is not created or advanced for custom apps.
- A failed apply does not leave `app.yaml` permanently diverged from materialized containers.
- A crash/restart during apply does not silently promote an uncommitted candidate manifest.
- Custom manifest apply does not write `app.prev.yaml`; image rollback semantics keep their existing backup slot.
- Dry-run/apply is guarded by a dry-run token and runtime fingerprint so the exact confirmed candidate cannot apply over an intervening manifest, image, or rootfs mutation.
- Dry-run and task artifacts are redacted; secrets and rendered YAML are never emitted in summaries, progress, active-task state, or logs.
- Disabled apps stay stopped; update requires re-enable first.
- Init scripts remain exactly-once install-time behavior.
- Generated inputs are not silently regenerated during update.
- Existing app update/rollback buttons keep their image-rootfs tuple semantics; this RFC does not redefine them as manifest rollback controls.

## Testing plan

Backend tests:

- custom app env-only update succeeds and recreates containers
- custom app additive storage/env update succeeds and preserves instance ID and data volume
- storage addition succeeds; storage removal, rename, and mount-path change are rejected
- dry run reports no-op for canonical-equivalent YAML
- runtime no-op but manifest-metadata change persists the canonical rendered definition without container recreation
- dry run returns base manifest hash, runtime fingerprint, and dry-run token; apply conflicts when manifest changed after dry run
- apply conflicts when active rootfs/runtime state changed after dry run
- apply conflicts when YAML/inputs/regeneration choices differ from the dry-run token
- regenerated input values are materialized at dry run and reused exactly at apply without exposing values in the token
- `__app_address__` input is overridden/pinned and primary listener drift is rejected
- catalog app manifest update is rejected
- disabled app manifest update is rejected
- workspace app manifest update is rejected
- image reference change is rejected
- listener topology/auth change is rejected
- added service is rejected
- service removal is rejected
- current or candidate service `oidc_client` is rejected
- generated input without explicit operator value or explicit regenerate choice is rejected
- optional inputs must be materialized into the normalized request before dry run/apply
- init-script drift is rejected
- service-field matrix: environment allowed/redacted, storage add-only, bind_ports rejected, after rejected, image/init/OIDC/init_script rejected, primary_service rejected
- resource, permission, healthcheck, auth, x-piccolo, type, workspace_name, and app_config changes are rejected
- metadata-only apply stores the manifest without restarting services
- runtime apply result reports container-readiness-only success
- apply failure after new `app.yaml` persist restores previous `app.yaml` and previous containers
- failed apply/restore retains the transient backup until recovery or explicit cleanup
- crash after transaction record or candidate persist restores previous manifest/runtime before normal reconcile
- custom manifest update does not create `install_state.json`, does not mutate catalog sync hash fields, and does not write `app.prev.yaml`

UI tests:

- custom app detail shows Apply Manifest YAML and catalog app detail does not
- image update action is labeled Update Image anywhere both actions can appear
- secret/generated-input preflight lists required values before the main form
- wizard requires password inputs to be re-entered
- generated-value regeneration is secondary and warning-gated
- provenance labels distinguish locked current values, re-entry, new defaults, and entered-now values
- secret form state is cleared on cancel, completion, terminal dismissal, or page/session close
- dry-run rejection surfaces the blocking reason before apply
- dry-run summary has Will change, Will restart, Will preserve, Expected interruption, and Rejected sections
- Will preserve summary states listener topology is unchanged
- apply progress/result returns to app detail with refreshed app state
- final result distinguishes metadata-only apply from runtime apply and states app-level health was not verified
- failure recovery states are visible during restore and after terminal failure

Device verification:

- On a real device, update a custom multi-service app with unchanged image refs and new env/storage wiring; verify recreated containers see the new env and mounts.
- Verify app data survives update and reboot.
- Verify listener/proxy routing remains unchanged after apply.
- Force an apply failure and confirm the app returns to the previous manifest/container definition.
- Reboot during apply and confirm boot-time recovery restores the previous manifest/runtime or fails closed with transaction/backup retained.
- Verify an available image rollback snapshot still restores using its own `app.prev.yaml` after a successful custom manifest update.

## Migration and rollout

No automatic migration. Existing custom apps become eligible for manual YAML update once the feature is available. Operators still need the newer manifest template and the input values required to render it.

For apps that also need new image bits, the v1 recommended flow is:

1. keep image references stable in the manifest, or publish the new image behind the existing tag
2. run the existing app image update path
3. apply the custom YAML update for non-image wiring

Apps that require image reference changes, new services, listener topology changes, app config changes, resource changes, or data-safe rollback still require reinstall or a future v2 flow.

## Alternatives considered

### Persist a custom install input ledger first

This would improve ergonomics because updates could reuse previous inputs and generated values, but it turns the feature into a broader post-install secret-management project. It also changes the current custom-install security posture by retaining plaintext user inputs. Deferred to the separate input/config update track.

### Treat custom apps as catalog apps with a local catalog source

This would reuse more catalog machinery, but it introduces a fake source of truth and background semantics where the operator expects an explicit one-shot update. It also makes conflict behavior unclear when an operator pastes a one-off manifest. Rejected for v1.

### Combine manifest update and image update

This is the eventual complete solution for service additions and image-reference changes, but it must compose manifest rollback with rootfs tuple update/rollback. The current urgent Piclu case only needs manifest wiring against existing services and image references, so v1 stays manifest-only.

### Transactional data snapshot for manifest update

Snapshotting app data would make failed applies safer for stateful apps, but it expands the feature into tuple/data rollback semantics. V1 documents manifest/runtime-only recovery and keeps data snapshot rollback for v2.

### Broad listener and resource mutation

Listener topology, resource policies, auth, and `x-piccolo` changes are valid future manifest-update needs, but each has a separate rollback surface. V1 rejects them so the Piclu diagnostics path can ship without broad transactional routing/resource work.

## Future / v2 questions

- Masked "unchanged secret" sentinels require a persistent input ledger or secret-reference model. V1 uses explicit re-entry only.
- User-initiated rollback of a successful manifest update requires durable manifest history and probably data snapshot semantics. V1 rollback is failure-path only.
- Broad listener topology, resource policy, app config, and data-snapshot rollback can be designed as a v2 manifest-update track.
