# RFC: Installed App Config Update

**Date:** 2026-05-30
**Status:** Draft - review fixes applied

## Scope block

**Problem:** Declared app input/config values are effectively frozen after install, so changing operator-entered values such as provider choices, hostnames, and secrets requires reinstall or an unrelated manifest update workflow.
**In scope:** A first-class installed-app config ledger and admin edit flow for declared app inputs/config values on installed catalog and custom apps, including secret redaction, dry-run rendering, safe apply/recreate behavior, and reuse by catalog sync and custom manifest update.
**Out of scope:** Arbitrary app YAML/spec mutation, image updates, service additions/removals, listener topology changes, storage removals/renames, resource policy changes, init-script replay, app data snapshot/rollback, background sync for custom apps, and hot-reload contracts beyond the restart/recreate behavior Piccolo already owns.

## Relationship to existing RFCs

This RFC supersedes the narrow runtime `app_config` update shape in `docs/rfc/20260317-runtime-app-config.md` for new work. That draft correctly identified the problem of frozen runtime config, but its `PATCH /api/v1/apps/:name/config` shape updates only stored `app_config`. The target behavior here is broader and narrower at the same time:

- broader because declared install inputs can render into service environment, `app_config`, listener names, and other manifest fields;
- narrower because only declared values are editable, not arbitrary rendered YAML.

This RFC also closes the explicit v1 gap left by `docs/rfc/20260530-custom-app-manifest-update.md`: custom manifest update does not persist custom install inputs, so it requires re-entry. Installed app config update makes the persisted declared-value ledger a deliberate platform feature instead of a catalog-sync implementation detail.

## Decision summary

Introduce an installed-app config ledger that records the current values for an app's declared inputs, plus the render context needed to deterministically re-render the app definition. Admins can view redacted current config state, propose declared value edits, dry-run the rendered effect, and apply only changes that pass the same identity and structural safety constraints used by installed-app update paths.

The ledger is secret-bearing state. It lives on the encrypted control volume with restrictive file permissions, is never embedded in generic app metadata responses, and is exposed through config-specific APIs only in redacted form. Secret updates are write-only from the UI/API perspective: callers may keep an existing value, replace it, or explicitly regenerate a generated value, but reads never return raw secret material.

Catalog apps and custom apps share the same installed config model. Catalog sync consumes the ledger instead of treating `install_state.json` as an isolated catalog-only shape. Custom manifest update consumes the ledger to prefill keep/replace/regenerate choices when the new manifest declares the same inputs, while still requiring explicit entry for newly-declared required values.

V1 decisions:

- **Single authority:** `install_state.json` becomes the schema-versioned installed config ledger. There is no second config-state file and no dual-read fallback after migration. Catalog sync, custom manifest update, and config edit all read and write through one compare-and-swap ledger API with a monotonically increasing revision.
- **App modes:** service-mode installed apps are the v1 apply target. Workspace Edit Config is disabled/read-only in v1 until workspace runtime recreation has its own policy.
- **Secret-at-rest posture:** encrypted control volume plus `0700` app directory and `0600` ledger file permissions remain the v1 storage posture. Field-level encryption is deferred; API redaction and write-only secret updates are required in v1.

## Product behavior

### Operator flow

1. The operator opens an installed app and chooses **Edit Config**.
2. Piccolo returns the declared input schema, current value metadata, and per-field editability. Secret values are redacted; non-secret values may be displayed when safe.
3. The operator changes declared values. For each secret or generated value, the UI offers explicit keep, replace, or regenerate choices.
4. Piccolo runs a dry run using the stored manifest source, stored system context, preserved app identity, preserved OIDC credentials, and the proposed value set.
5. Dry run returns a redacted change summary with one of:
   - no-op
   - config-only update
   - applicable runtime recreate
   - rejected update with a blocking reason
6. If applicable, the operator confirms apply with the dry-run token.
7. Piccolo revalidates against current app state, persists the updated ledger and rendered manifest, and restarts/recreates the affected runtime state according to the accepted diff class.

### Field behavior

- Declared non-secret inputs can be read and edited.
- Declared password inputs are displayed as present/absent only. The default action is `Keep existing`; `Replace` reveals an empty entry field; `Clear` is available only for optional string/password inputs where an explicit empty value passes validation. A blank field never means "keep existing." True absence is deferred until the input schema supports nullable/absent values distinctly from defaults.
- Declared generated inputs are displayed as present/absent only. The default action is `Keep existing`; `Regenerate` is a secondary action with consequence text because generated values may invalidate sessions, tokens, or external integrations.
- Sensitivity is not determined by `type: password` alone. Input schema may declare explicit sensitivity, and the server also conservatively redacts risky names containing terms such as `secret`, `token`, `key`, `password`, or `credential`.
- Each stored field records provenance: `operator`, `catalog_default`, `generated`, `system`, or `legacy_unknown`. Catalog sync may adopt a new default only for untouched `catalog_default` fields; operator-entered, generated, and system values are preserved unless the operator explicitly changes them. If schema evolution invalidates a preserved value, sync blocks with "catalog schema changed; review config."
- `__app_address__` remains locked to the installed app identity and cannot be changed through this flow.
- System-derived values such as domain, architecture, timezone, and OIDC issuer remain frozen from the install context unless a separate context-refresh path updates them.
- Newly-declared required inputs are marked missing and must be supplied before dry run can become applicable.
- Removed inputs remain in ledger history only long enough to support migration/audit; they are not passed into future renders.
- Inputs referenced by init or init_script material are install-only in v1. Edits to those inputs are rejected with "reinstall required" because app config update does not replay one-shot initialization.

### Apply policy

Allowed in v1:

- updates that only change the installed config ledger and input schema metadata;
- rendered `app_config` changes that can be materialized by rewriting `/piccolo/config/app.yaml` and restarting/recreating the app;
- existing service environment changes caused by declared input edits;
- no-op canonical manifest changes caused by schema/default normalization.

Rejected in v1:

- app identity changes, including rendered primary listener identity drift;
- image reference changes;
- service additions/removals;
- listener topology or auth changes;
- top-level environment changes;
- storage additions, removals, renames, or mount-path changes;
- startup-order, init, init-script, OIDC client declaration, resource, permission, health check, lifecycle, extension, and workspace identity changes;
- workspace-mode runtime-affecting changes;
- any candidate that requires re-running one-shot install/init semantics.

Dry-run summaries must name changed fields, keys, services, listeners, and change classes only. They may display old/new values for non-sensitive declared inputs so the operator can verify intent. They must not include password values, generated secrets, sensitive inferred values, environment values, app config values, rendered YAML, or signed blobs containing secret-bearing material.
For runtime-affecting config edits, dry-run summaries must state that persistent data is preserved but not snapshotted. For secret replacement, secret clearing, or generated-value regeneration, the summary must include non-secret consequence text such as session invalidation or external integration breakage.

## API shape

### Read installed config

`GET /api/v1/apps/:name/config`

Returns:

- app identity and source kind;
- declared input schema snapshot;
- per-field current value metadata: present, redacted display value when non-secret, provenance, sensitivity, required/default status, and editability;
- ledger health: complete, missing values, legacy-backfill, or unrecoverable;
- warnings for unsupported app modes or apply policies.

### Dry run installed config edit

`POST /api/v1/apps/:name/config/dry-run`

Accepts:

- field updates for declared inputs;
- per-secret actions: keep, replace, or clear;
- per-generated actions: keep or regenerate;
- optional client-side form version/fingerprint for stale edit detection.

`clear` is valid only for optional string/password inputs where an explicit empty value passes validation. Required secrets reject clear during dry run. Because the renderer backfills missing optional inputs today, v1 clear is not modeled as omission.

Returns:

- opaque dry-run token;
- base source hash, input schema hash, ledger revision, manifest hash, config/runtime fingerprints;
- normalized field-action summary;
- redacted change summary grouped as Will change, Will restart, Will preserve, and Rejected when applicable;
- missing required values and blocking reason when rejected.

Dry run materializes generated values exactly once. The generated values, normalized candidate inputs, rendered manifest, ledger revision, hashes, and summary are stored server-side under the opaque dry-run token. The token must be secret-free; if the candidate expires or the process loses it, apply rejects and the operator must run dry run again.

### Apply installed config edit

`POST /api/v1/apps/:name/config/apply`

Accepts:

- dry-run token;
- candidate/request digest;
- base manifest/config/runtime fingerprints;
- non-secret confirmation metadata.

Behavior:

- admin-only and unlocked;
- task-backed for runtime-affecting changes;
- serialized against app install, sync, manifest update, image update, and uninstall operations;
- is not a secret-bearing request; replacement secrets and generated material are submitted only to dry run;
- consumes the exact server-side dry-run candidate and byte-compares the candidate digest instead of regenerating generated values;
- re-renders and reclassifies only to verify the request still matches the stored candidate;
- rejects with conflict when source hash, input schema hash, ledger revision, manifest hash, runtime fingerprint, or dry-run token no longer matches;
- returns final app state and redacted applied summary.

## Backend design

### Installed config ledger

Add a first-class persisted ledger for declared installed app config values by versioning `install_state.json`. The filename remains stable to avoid two authorities; the schema becomes an explicit installed config ledger:

- it is the authoritative source for declared installed input values;
- it carries the raw template source bytes used to render the installed app;
- it carries catalog or custom source reference metadata and source hash only as provenance and drift detection, not as the render authority;
- it carries the frozen install system context needed by `RunInstallPipeline`;
- it carries OIDC credentials when deterministic re-render requires them;
- it records per-field metadata for sensitivity, provenance, generation behavior, and schema version/hash;
- it stores enough source-kind metadata for catalog sync and custom manifest update to share it safely.

All modern readers and writers go through the ledger API. The API owns revision checks, file permissions, redaction projections, and v1-to-v2 migration. Direct access to legacy install inputs is removed from catalog sync and custom manifest update during this feature.

Ledger value invariants:

- `install_inputs` is sparse and may be empty. A missing or omitted map in `install_state.json` means no operator-entered values were stored, not an incomplete ledger.
- A request-omitted field means keep the current effective value. For absent optional inputs, the effective value comes from the raw template default or type zero value during render/read; it is not materialized into the ledger until an operator explicitly replaces or clears it.
- Sensitive inputs are write-only. Secret reads never return raw values, blank replacements are rejected, and optional blank secret intent must be sent as `clear`.
- Ledger revisions are monotonic across Edit Config, catalog sync, and custom manifest update. A manifest/source hash may advance only after the matching ledger source/value write succeeds.
- Post-commit transaction cleanup is best-effort: cleanup failure must not report an already-committed update as failed.

For existing catalog installs, migrate current `install_state.json` content into the versioned ledger only when raw source bytes can be recovered byte-identically to the installed `CatalogManifestHash`. If a fetch of the current catalog reference returns bytes whose hash matches the installed hash, those bytes may seed the v2 ledger. If the catalog has moved, is unavailable, or returns bytes that do not match the installed hash, the app stays in legacy/partial health: catalog sync may continue using its legacy behavior, but Edit Config is unrecoverable until reinstall, import-source, or a successful future catalog sync stores committed raw source bytes. Tests must cover a catalog install whose reference moved before migration.

For new catalog and custom installs after this RFC lands, v2 ledger persistence is part of the install commit. A failure to persist the ledger fails or rolls back install rather than producing a running app that should be config-editable but cannot be deterministically re-rendered.

For existing custom installs without stored raw source/input values, Edit Config fails closed as unrecoverable. Re-entering values alone is not enough because Piccolo cannot deterministically re-render from an already-rendered `app.yaml`. The operator must use Apply Manifest YAML, or a future import-source flow, to store the raw source and populate the ledger.

Malformed modern ledger state is fail-closed. Once an app has a v2 ledger, parse errors, hash mismatches, or corrupt required fields block sync, manifest update, and config apply with an unrecoverable health state; they must not fall back to defaults, legacy mode, or partial rendering.

The raw source bytes are updated only at successful install, successful catalog sync, successful custom manifest update, or successful config apply. Config edit renders from the ledger's stored raw source bytes, never by fetching the latest catalog reference. Catalog fetches are used to produce a candidate source for sync; once sync commits, that candidate source is stored in the ledger as the new render authority.

### Rendering and classification

Config dry run reuses `RunInstallPipeline` with:

- stored raw template bytes from the ledger;
- candidate declared input values after keep/replace/regenerate normalization;
- stored install system context;
- existing app instance ID;
- preserved OIDC credentials when present.

After rendering, Piccolo compares the candidate definition with the current rendered definition and classifies the diff through the same source-agnostic app apply machinery used by catalog sync and custom manifest update. Config update supplies its own strict allowed-diff policy: ledger/schema metadata, `app_config`, and existing service environment changes are allowed; identity, topology, image, storage, startup, OIDC declaration, and one-shot-init changes are rejected.

Dry-run and apply both carry source hash, input schema hash, ledger revision, manifest hash, and runtime fingerprint. If catalog sync or another operation changes any of those between form load, dry run, and apply, the operator gets a clear conflict such as "catalog changed; reload config form." Operator-edited values are never silently overwritten with catalog defaults during sync.

If catalog sync discovers a candidate source that declares new required inputs without valid ledger values, sync records a pending source/schema hash and missing input list, then blocks without applying the manifest/runtime change. Edit Config can load that pending schema, collect the missing values, and retry the sync candidate through the same dry-run/apply flow. Defaults are not silently adopted for new required fields unless the field is optional or explicitly marked safe as a catalog default.

### Apply and recovery

Applicable updates persist the ledger and rendered manifest with a durable config-update transaction record. The record lives next to app metadata and includes operation ID, phase, prior/candidate ledger and manifest hashes/revisions, runtime fingerprint, and restore metadata required for boot recovery.

Phases:

- `prepared`
- `candidate_manifest_persisted`
- `recreating_runtime`
- `ledger_committing`
- `committed`
- `restoring_previous`
- `restore_failed`

The commit point is the successful atomic write of the candidate v2 ledger after the candidate manifest has been persisted and runtime recreation has succeeded. Until that commit point, the previous ledger remains authoritative. If apply fails or the device reboots before commit, boot recovery restores the previous manifest, recreates previous runtime state when needed, and discards the candidate ledger values. If the device reboots after ledger commit but before transaction cleanup, boot recovery verifies the candidate manifest hash and ledger revision, completes cleanup, and surfaces fail-closed if the hashes do not reconcile.

This transaction invariant applies to every writer of the authoritative ledger, not only Edit Config. Catalog sync and custom manifest update must use the same ledger/manifest/runtime commit protocol when they change source hash, schema hash, OIDC credentials, declared values, rendered manifest, or runtime state. No path may commit a rendered manifest/runtime change while the ledger source/schema/OIDC/value state fails to commit.

Install system context is also a render input covered by the ledger-writer rules. Context refresh for v2 ledgers must either stage the new context through the same pending/dry-run/transaction path when it changes the rendered manifest, or remain blocked with a clear conflict until it can be applied atomically.

Runtime-affecting updates use the existing task/progress pattern. If runtime recreation fails after the candidate manifest is persisted, Piccolo restores the previous rendered manifest and preserves the previous ledger values unless and until the candidate runtime is successfully committed.

Disabled or stopped service apps are conservative in v1: ledger/schema-only no-op updates may be recorded, but any runtime-affecting apply is rejected with a "start app before applying runtime config" message. Config edit does not persist a runtime-affecting candidate for next start in v1.

Catalog sync must continue to distinguish legacy backfill from modern deterministic render. Its source of current declared values becomes the installed config ledger. Sync must not silently overwrite operator-edited values with catalog defaults.

Custom manifest update must use the installed config ledger to offer keep/replace/regenerate choices for inputs shared between old and new manifests. It still requires explicit operator input for new required values and still treats the new YAML as a manual transaction, not background sync.

## UI behavior

Add **Edit Config** on installed app detail pages when the app has a declared input schema, a recoverable ledger, or a custom manifest source that can be paired with newly-entered values.

The UI flow mirrors the manifest-update dry-run posture:

- form fields show current value status and provenance;
- non-sensitive declared input changes may show old/new values in review; sensitive values remain redacted;
- secrets default to `Keep existing`; replacement fields appear only after the operator chooses `Replace`, and optional clears are explicit;
- generated values require an explicit regenerate action with consequence copy and confirmation before apply;
- dry run precedes apply;
- dry-run submission shows an always-visible local status/result strip near the triggering control, then moves focus or scroll position to the full result summary when the summary would otherwise render below the fold;
- conflict or token-expiry recovery preserves unsaved form entries in memory where possible, offers "Run dry run again," and warns before any reload that discards typed replacement secrets;
- legacy custom apps without stored source show a disabled/read-only Edit Config state with a next action to Apply Manifest YAML to store source and enable config editing;
- workspace apps show a disabled/read-only Edit Config state explaining that workspace config apply is not supported in v1;
- field review rows may show old/new values for non-sensitive declared inputs; rendered environment, app config, and runtime summaries show changed keys and restart expectations, not values;
- secret replacement, secret clearing, and generated regeneration all require explicit redacted confirmation before apply;
- applying shows immediate visible progress near the Apply control or moves focus to task progress when the progress panel would otherwise render below the fold;
- runtime-affecting summaries state that persistent data is preserved but not snapshotted;
- rejected changes explain the offending class and next action.

The existing **Apply Manifest YAML** action remains custom-app-only and continues to mean "change the manifest source." **Edit Config** means "change declared values for the currently-installed app source."

## Site list

- `internal/app/install_state.go`: existing secrets-bearing install state, migration source, and possible versioned ledger storage.
- `internal/app/install_pipeline.go`: deterministic re-render path for install, sync, manifest update, and config update.
- `internal/app/catalog_sync.go`: install-time ledger write and catalog metadata persistence.
- `internal/app/catalog_sync_apply.go`: catalog sync re-render and diff/apply behavior using ledger values.
- `internal/app/custom_manifest_update.go`: manual custom YAML update integration with stored declared values.
- `internal/app/app_manager.go`: app operation serialization, dry-run token storage, and task coordination.
- `internal/app/filesystem.go`: persisted app manifest, metadata, ledger, and operation transaction files.
- `internal/app/multi_container.go`: materialization of rendered `app_config` and runtime recreation effects.
- `internal/app/parser.go`: declared input schema validation and sensitivity/type handling.
- `internal/server/gin_app_handlers.go`: install/configure/custom install handler writes and config edit handlers.
- `internal/server/gin_app_sync_handlers.go`: context refresh and sync interactions with installed declared values.
- `internal/server/gin_server.go`: route registration and admin/unlocked middleware.
- `internal/server/openapi_middleware.go`: request validation behavior for new config routes.
- `internal/api/types.go`: API request/response types for config read, dry run, and apply.
- `ui/lib/features/apps/app_detail_view.dart`: installed app actions and config edit entry point.
- `ui/lib/features/apps/installed_config_wizard.dart`: planned config edit form, dry-run summary, apply task progress, and redaction states.
- `ui/lib/features/apps/manifest_update_wizard.dart`: existing custom manifest dry-run/apply feedback pattern kept aligned with the new config flow.
- `docs/api/openapi.yaml`: authoritative API schema for config read, dry run, and apply.
- `docs/api/openapi_validation_test.go`: OpenAPI validation coverage for the new route surface.
- RFC relationship notes: mark `docs/rfc/20260317-runtime-app-config.md` superseded/narrowed and link `docs/rfc/20260530-custom-app-manifest-update.md` to ledger-backed input reuse.

## Test plan

- Unit tests for ledger store/load, permissions expectations, v1-to-v2 migration from catalog `install_state.json`, redaction, missing-value states, CAS revision conflicts, and malformed modern ledger fail-closed behavior.
- Pipeline tests for keep/replace/clear/regenerate behavior, conservative sensitivity inference, provenance/default adoption, init/init_script input rejection, locked `__app_address__`, preserved OIDC credentials, exact generated-value reuse from dry run to apply, and stale dry-run token conflicts.
- App manager tests for config-only apply, service environment apply, `app_config` materialization, rejected topology/image/storage/init changes, failed-runtime rollback, boot recovery across every transaction phase, existing catalog migration only when source bytes match installed hash, moved-catalog migration staying unrecoverable for Edit Config, install rollback on v2 ledger persistence failure, context refresh transaction handling, and catalog sync/custom manifest update preserving authoritative ledger transaction invariants.
- Server tests for admin/unlocked guards, redacted responses, missing required inputs, secret write-only semantics, OpenAPI route coverage, and source/schema/ledger/runtime conflict responses.
- UI tests for non-secret edits with old/new review values, secret replacement, optional secret clear, generated regeneration warning, conflict retry without losing typed entries, disabled legacy-custom/workspace entry states, dry-run status/result visible near the button, below-fold result focus/scroll, rejected states, and apply task progress visibility.
- Device smoke test with Piclu: update a declared provider/hostname/secret value, verify the rendered env/config takes effect after recreate, and verify the value persists across reboot.

## Deferred

- Workspace-mode post-install config apply.
- Field-level encryption for the installed config ledger.
- Import-source flow for legacy custom installs that no longer have raw template/input state.
- Hot-reload app contracts and data-snapshot rollback.
