# RFC: App Maintenance UX Overhaul

**Date:** 2026-06-16
**Status:** Implemented locally; review green; full alpha green

## Scope block

**Problem:** App detail maintenance actions expose backend implementation terms and piecemeal review surfaces, leaving operators unsure what kind of update they are starting, when review is required, and whether existing config or credentials will be preserved.
**In scope:** Installed service-app maintenance UX for app detail actions, the unified `Update` action, `Modify App` full-YAML source replacement, `Edit Config` cohesion, shared input/secret controls, dry-run/review hierarchy, catalog-pending update routing/copy, and operation/readiness/repair wording.
**Out of scope:** Backend transaction semantics beyond data needed to present the UX, workspace app update support, storage migration/removal, source diff editing or source repository management, automatic application of high-risk catalog updates, a new manual access-repair command, and user-initiated rollback semantics after a successful update.

## Background

The app-maintenance backend has grown from separate needs:

- exact-tag image/rootfs refresh through **Update Image**;
- installed config/value changes through **Edit Config**;
- custom app YAML source replacement through manifest update;
- catalog updates that sometimes need missing config values and sometimes need
  high-risk service-app review;
- access repair and readiness observation after update-like operations.

Each piece has a reasonable local implementation, but the operator-facing
surface now feels assembled from backend terms: `Review App Update`,
`Review Catalog Update`, `Review Manifest YAML Update`, `Manifest YAML`,
`Prepare`, `Dry Run`, and `Apply`. The result is cognitive friction at exactly
the moment the operator is trying not to break a running app.

The product model should instead be calm and responsibility-oriented:

- **Update** is the normal way to bring an installed app forward.
- **Modify App** is the explicit way to replace a custom app's full definition
  from pasted YAML.
- **Edit Config** changes declared values for the currently installed app
  source.

Piccolo should interrupt only when an update crosses an operator responsibility
boundary: public exposure, auth, service shape, storage/data safety, sensitive
config, or another runtime risk that needs human acknowledgement.

## Decision summary

### One `Update` action

Installed service apps expose a single primary **Update** action. The action
branches by available candidate and risk:

| Situation | User-facing result |
| --- | --- |
| No pending catalog/source candidate; current image refs can be refreshed safely | Start the existing image/rootfs refresh without a review dialog. |
| Pending update has no responsibility-boundary review requirements | Start the update after concise submission feedback. |
| Pending update requires operator review | Open the review surface before applying. |
| Pending update needs missing/new declared values | Open the value-entry surface as part of update review. |
| Pending update is unsupported | Show the blocked reason and next action. |

`Review Update` is not a separate top-level action. It is a state that `Update`
may enter.

The button is paired with a visible update state before the operator clicks it:
`Refresh current image`, `Update available`, `Update needs config values`,
`Update needs review`, or `Update blocked`. The state names what the click will
do and shows source provenance when a candidate source exists.

### `Modify App` replaces the manual YAML entry point

Custom service apps expose **Modify App** as an advanced action. It means:

- the operator will paste a full replacement app manifest YAML;
- Piccolo renders that source against the installed app identity and stored
  config ledger where possible;
- Piccolo previews the candidate before modification is applied;
- if the candidate crosses a review boundary, the same review surface is used.

The UI must not imply that the operator is editing a patch or a partial diff.
The source step must say "paste the full replacement manifest."

### `Edit Config` remains distinct

**Edit Config** continues to mean "change declared values for the currently
installed app source." It does not change the source manifest, catalog version,
image references, listener topology, service shape, or storage declarations
except through the existing config-update policy.

### Credentials use explicit actions

Secret and generated fields must never appear as mysterious blank password
fields.

The shared credential model is:

- existing stored secret: default action is **Keep current value**;
- operator change: choose **Replace value**, then enter a new value;
- generated secret: choose **Keep current value** or **Regenerate value**;
- new required secret: operator must enter or generate it;
- legacy-missing secret: show an explicit one-time re-entry explanation.

Secrets are reused without display. Piccolo may render a candidate with stored
ledger secrets, but the browser does not prefill secret values.

### Calm-by-default review posture

The review surface appears only when the candidate needs operator judgement.
The most common successful update should feel like "Piccolo is updating this
app and watching readiness," not like a surgical procedure.

## Product model

### Operator verbs

The app detail surface uses these verbs:

| Verb | Meaning |
| --- | --- |
| **Edit Config** | Change declared values for the current app source. |
| **Update** | Bring the installed app forward or refresh current image/rootfs state. Piccolo decides whether review is needed. |
| **Modify App** | Replace a custom app's full source manifest from pasted YAML. |
| **Rollback** | Existing rollback surface, unchanged by this RFC. |
| **Start / Stop / Terminal** | Existing lifecycle/access actions, unchanged except for toolbar grouping. |

`Access repair pending` is a state, not a primary action in this RFC. Until a
real repair endpoint exists, the banner may offer `Check Again`/`Refresh` only;
it must not promise that the click will repair routing or auth.

### Source provenance

Every update/review surface shows a small provenance line:

| Candidate source | User-facing copy |
| --- | --- |
| Current app image refs | `Source: current app definition` |
| Catalog pending update | `Source: app catalog` |
| Pasted custom YAML | `Source: pasted manifest YAML` |
| Legacy custom app with no stored source | `Source: pasted manifest YAML required to store source for future edits` |

This gives operators the context they need without making "catalog" the action
name.

### Risk ladder

The UI classifies candidates into four UX outcomes:

1. **Runs immediately after `Update`**
   - exact image/rootfs refresh for current refs when the app has no persistent
     data or rollback snapshot safety can be preflighted successfully;
   - pending updates that do not change operator responsibility boundaries.
2. **Review required**
   - public exposure changes;
   - auth/middleware changes;
   - service additions/removals;
   - storage/data-safety changes that require human acknowledgement;
   - sensitive config replacement/regeneration;
   - any backend classification that returns required confirmations.
3. **Input required**
   - new required declared value;
   - new required secret;
   - legacy-missing stored value that cannot be safely inferred.
4. **Blocked**
   - unsupported storage mutation/removal;
   - unsupported workspace update;
   - persistent-data image/rootfs refresh when rollback snapshot safety is
     unavailable or cannot be preflighted;
   - corrupt/unrecoverable ledger;
   - candidate conflicts or expired dry-run token;
   - any backend policy reject.

The ladder is not a new trust model. It is a UX projection over existing and
planned backend classifiers.

## Flow design

### App detail actions

The primary operation toolbar should present:

- `Terminal`, when available;
- `Edit Config`, when applicable;
- `Update`, when the app is a service app and running or otherwise eligible;
- `Start` or `Stop`.

Advanced or lower-frequency actions live in overflow:

- `Modify App`, custom service apps only;
- `Rollback`;
- `Uninstall`.

The catalog-pending banner remains, but its action copy follows the same model:

| Pending flow | Banner title | Action |
| --- | --- | --- |
| available safe update | `Update available` | `Update` |
| missing/new values | `Update needs config values` | `Continue Update` |
| high-risk app/runtime change | `Update needs review` | `Continue Update` |
| blocked pending source | `Update blocked` | next-action copy from backend |

The banner may mention `Source: app catalog` in secondary text.

### Update action branching

The app detail view derives a normalized update state before enabling **Update**.
This is a presentation contract over the app-detail fields, not a new backend
transaction model.

| App detail state | Visible state | Click behavior | Fail direction |
| --- | --- | --- | --- |
| Mutating app operation active | disabled with current operation reason | no action | stays disabled |
| Workspace app | no app update action | no action | out of this RFC |
| Service app not eligible to update, such as stopped when the endpoint requires running | disabled with reason | no action | stays disabled |
| `catalog_update_pending=false` and image refresh has no persistent-data rollback risk | `Refresh current image` with `Source: current app definition` | call existing image/rootfs refresh endpoint | endpoint errors show existing rejection copy |
| `catalog_update_pending=false`, app has persistent data, and rollback snapshot safety is unavailable or unknown | `Update blocked` with data-safety reason | no action, or backend rejects before runtime switch | fail closed; never run a no-rollback image refresh |
| `catalog_update_pending=true`, `catalog_update_pending_flow=config` | `Update needs config values` with `Source: app catalog` | open `Edit Config` in pending-update context | config read/dry-run errors are shown as blocked/rejected |
| `catalog_update_pending=true`, `catalog_update_pending_flow=manifest_review` | `Update needs review` with `Source: app catalog` | open update review flow for the pending catalog source | configure/dry-run errors are shown as blocked/rejected |
| Future backend exposes a safe pending source with an explicit apply endpoint | `Update available` with `Source: app catalog` | call that explicit pending-update endpoint | missing endpoint or stale token blocks; never fall through to image refresh |
| `catalog_update_pending=true` with empty, unknown, or unsupported flow | `Update blocked` | show pending reason or generic unsupported-pending-state copy | fail closed; never call image/rootfs refresh |
| Access repair pending after a prior update | update state remains as above, plus warning banner | `Update` does not attempt repair | banner offers `Check Again`/`Refresh` only |

Access-repair-pending does not automatically disable later maintenance actions
in this RFC, because the backend may still allow safe refresh/config work. The
banner must be explicit that starting another `Update` will not repair the
existing access warning.

The operator should not have to choose "review" vs "update"; Piccolo chooses
based on candidate risk.

Implementation must keep this matrix centralized in the app-detail presentation
layer or model helper so toolbar labels, banners, and click routing cannot drift
apart.

The current image/rootfs refresh endpoint must participate in the same
fail-closed data-safety contract. For apps with persistent data, rollback
snapshot viability must be checked before the runtime switch. If the check
cannot be made or fails, the UI shows `Update blocked` and the backend rejects
the operation instead of proceeding without rollback capability.

### Modify App flow

`Modify App` has these steps:

1. **Source**
   - title: `Modify App`;
   - field label: `Full manifest YAML`;
   - helper: `Paste the full replacement manifest. Piccolo will preserve the installed app identity and stored config where possible.`;
   - primary button: `Continue`.
2. **Values**
   - uses the shared input/secret controls;
   - shared existing values default to keep/reuse;
   - new required values are clearly marked;
   - legacy-missing values explain one-time re-entry.
3. **Preview**
   - primary button: `Preview Modified App` or `Preview Changes`;
   - shows local running/rejected/ready status near the button;
   - scrolls to the review summary after preview.
4. **Review and Modify**
   - top decision summary first;
   - inline confirmations beside the relevant risk groups;
   - final action copy remains in the `Modify App` vocabulary, such as
     `Modify App` or `Apply Modification`;
   - final action is enabled only when all required confirmations are accepted.

### Edit Config flow

`Edit Config` keeps its current purpose but should share:

- input field rendering;
- secret action controls;
- dry-run status strip;
- decision summary layout;
- operation progress handoff to app detail.

The title remains `Edit Config` for direct entry. When entered from a pending
update that needs config values, the surface keeps continuity with the action
that opened it by showing persistent context such as `Update needs config
values`, `Source: app catalog`, and `Values for the update candidate`. The verb
remains config/value-oriented, but the user should not wonder whether they left
the update flow.

### Review surface

The review surface is a decision page, not a raw diff dump.

Order:

1. **Outcome**
   - `Ready to update`, `Review required`, or `Blocked`;
   - one-line reason when review is required.
2. **Why review is needed**
   - exposure/auth;
   - services;
   - storage/data;
   - sensitive config;
   - other required confirmations.
3. **What will change**
   - grouped human-readable sections.
4. **What Piccolo will preserve**
   - app identity;
   - current source or source provenance;
   - stored values kept;
   - persistent data handling;
   - rootfs/image behavior where relevant.
5. **Expected interruption and readiness**
   - restart/recreate expectation;
   - readiness observation or access repair-pending possibility.
6. **Technical details**
   - raw env keys, decision flags, hashes, or classifier output as expandable
     detail, not the first thing the operator sees.

Confirmations are inline. For example, an exposure change group has its own
checkbox: `I reviewed these exposure, routing, and auth changes`.

## Shared input and credential controls

All maintenance flows use a common field model.

The common model must be represented as field state, not inferred from
human-readable helper strings. Existing manifest-update and installed-config
responses may map into this model in the client, or backend responses may add
presentation-state fields if the existing data is not precise enough.

### Non-sensitive fields

| State | UI | Dry-run submission |
| --- | --- | --- |
| Stored current value exists | Editable field prefilled with current value; helper `Current value`. | submit only when changed |
| New manifest default | Field prefilled or shown as default; helper `New default from source`. | submit if required or edited; optional untouched defaults may be omitted only when backend applies the same default |
| New required value | Empty field; helper `Required for this update`. | must submit non-empty value |
| Locked system value | Read-only field with lock icon; helper `Locked current value`. | omit; backend supplies system value |
| Legacy missing value | Empty field; helper `Required once because this app predates stored config`. | must submit value |

### Sensitive/generated fields

| State | UI | Dry-run submission |
| --- | --- | --- |
| Stored current secret exists | Action selector defaults to `Keep current value`; helper `Stored secret will be kept and is not shown`. | omit value and secret action |
| Replace requested | Show password field labeled `New value`; helper `Replaces the stored secret on apply; clients using the old value may need updating`. | submit secret action `replace` and non-empty value |
| Optional stored secret clear requested | Show explicit `Clear value` action with warning copy; no secret value visible. | submit secret action `clear` |
| Generated current secret exists | Actions `Keep current value` and `Regenerate value`; regenerate helper `Regenerates on apply; clients using the old value may need updating`. | omit for keep, submit regenerate action for regenerate |
| New required secret | Password field or generate action, depending on schema; helper `Required for this update`. | must submit value or generate action |
| Legacy missing secret | Explain one-time re-entry before the field/action. | must submit value or generate action |

Dry run submission treats omitted shared values as keep/reuse when the backend
has ledger values. Explicit blank required secret replacement is invalid.

For `Modify App` and catalog source replacement, reuse-by-key is not enough when
the candidate changes the meaning of a stored value. If an input keeps the same
name but changes sensitivity, generated behavior, default, description, or
service/config usage, the review surface must call that out as a kept-value
review item. Sensitive/generated or data-impacting kept values require explicit
confirmation before apply.

If the implementation cannot confidently compare same-key value meaning or
usage between old and candidate sources, it must fail closed for the UI:
non-sensitive values become review-required kept-value items, while
sensitive/generated or data-impacting values block unless the review can explain
what changed well enough for the operator to accept it.

Source-replacement dry runs expose kept-value review items as structured data,
not only summary text. The exact JSON name can follow local API conventions, but
the contract must include:

| Field | Meaning |
| --- | --- |
| field key | input name whose stored value would be kept |
| old/new semantic delta | sensitivity, generated behavior, default, label/description, or schema changes that affect meaning |
| old/new usage paths | services, env keys, app config paths, listeners, or other manifest sites that consume the value |
| risk kind | non-sensitive, sensitive, generated, or data-impacting |
| confirmation id | stable id required to apply when the item needs acknowledgement |
| blocking reason | reason reuse is blocked when the value cannot be safely explained |

Backend dry-run production owns this structured kept-value review data for
`Modify App` and catalog source replacement. The shared review surface consumes
it beside other review groups, and sensitive/generated/data-impacting items must
gate apply through their confirmation ids.

## Operation handoff context

Backend task types are not enough to preserve the operator's mental model. A
single task type such as `update_service_app` may represent catalog update
review, pasted-YAML `Modify App`, or another app-forwarding flow.

When a wizard or toolbar action hands an operation back to app detail, it must
carry display context in addition to `task_id` and backend `task_type`:

| Entry point | Display context to preserve |
| --- | --- |
| Current image refresh | `Refresh current image`, `Source: current app definition` |
| Catalog update | `Update`, `Source: app catalog` |
| Pending catalog config values | `Update needs config values`, `Source: app catalog` |
| Pasted YAML source replacement | `Modify App`, `Source: pasted manifest YAML` |
| Direct config edit | `Edit Config`, `Source: current app definition` |

Progress panels, recent-operation recovery, readiness banners, and
access-repair-pending copy must use this display context when available, falling
back to task-type labels only for older persisted operations.

## Copy rules

- Use `update` for normal app-forward motion.
- Use `modify` only when the operator is replacing custom source.
- Use `review` as a state, not a separate top-level action.
- Use `source` for YAML/catalog/current-app provenance.
- Avoid showing backend nouns as primary labels, including `Catalog Update`,
  `Manifest Update`, `Review App Update`, `Apply Manifest YAML`, `Ledger`,
  `Runtime Fingerprint`, and `Repair Access` unless backed by an explicit repair
  operation.
- When a backend noun is needed for precision, put it in secondary/detail copy.

## Site list

### UI

- `ui/lib/features/apps/app_detail_view.dart`
  - action toolbar labels and grouping;
  - overflow menu actions;
  - catalog-pending banner copy and routing;
  - access-repair/readiness banner copy;
  - wizard entry points and operation handoff.
- `ui/lib/features/apps/app_operation_lifecycle.dart`
  - user-facing operation labels;
  - task type mapping for `update_image`, `update_config`, and
    `update_service_app`;
  - display context carried across wizard handoff, recent submissions, progress,
    readiness, and failure settlement;
  - readiness/settlement copy that appears after update-like operations.
- `ui/lib/shared/widgets/task_progress_panel.dart`
  - display labels for task types;
  - fallback behavior for unknown task types;
  - progress copy used by app-detail and wizard operations.
- `ui/lib/features/apps/manifest_update_wizard.dart`
  - becomes or is wrapped by the `Modify App` flow;
  - source/value/preview/review step structure;
  - shared review surface and inline confirmations.
- `ui/lib/features/apps/installed_config_wizard.dart`
  - keeps `Edit Config` purpose;
  - adopts shared input/secret controls and review summary hierarchy;
  - supports pending-update context copy.
- `ui/lib/features/apps/*` shared widgets to add or extract as needed:
  - maintenance action toolbar/overflow item copy;
  - input field renderer;
  - secret action control;
  - update decision summary;
  - review section with inline confirmation;
  - dry-run status strip shared by config and modify/update flows.
- `ui/lib/core/models/app_models.dart`
  - consume existing pending-flow fields;
  - expose a normalized app-detail update state helper;
  - consume manifest/config review result fields;
  - add presentation-only helpers if needed, not backend terms in widgets.
- `ui/lib/core/services/app_service.dart`
  - route `Update` branches to existing endpoints;
  - keep manifest/config dry-run/apply calls;
  - add only minimal service helpers if UI branching becomes too scattered.
- `ui/test/*`
  - action label/routing tests;
  - unknown pending-flow fail-closed test;
  - operation display context survives wizard handoff and recent-operation
    recovery;
  - shared secret control behavior;
  - optional secret clear action and review copy;
  - dry-run submission omits untouched values;
  - review confirmation enablement;
  - catalog pending routes to the correct update context;
  - task-progress label tests for update-like operations.

### Backend and API docs

- `internal/app/types.go`
  - existing catalog pending flow fields are the source for UI routing;
  - add only presentation data needed to distinguish update states that cannot
    be derived safely by the frontend.
- `internal/app/app_manager.go`
  - image/rootfs refresh must fail closed for persistent-data apps when
    rollback snapshot safety is unavailable or cannot be preflighted.
- `internal/app/custom_manifest_update.go`
  - ensure configure/dry-run field provenance supports keep/replace/regenerate
    presentation for custom source replacement;
  - produce structured kept-value review items for source replacement.
- `internal/app/installed_config_update.go`
  - continue to provide recoverable/unrecoverable ledger states and secret
    action support.
- `internal/server/gin_app_handlers.go`
  - no new update or repair endpoint is required for the first UI overhaul
    unless implementation proves the frontend cannot branch from existing app
    detail and pending-flow data;
  - existing image/rootfs refresh responses must expose or enforce the
    persistent-data rollback safety gate;
  - if a future safe pending catalog apply path is exposed, it must use an
    explicit endpoint/state and must not fall through to image refresh.
- `docs/api/openapi.yaml`
  - document new or changed API fields required by the UI contracts.

## Implementation plan

### Phase 1 - Vocabulary and action routing

- Rename the app detail action model:
  - primary `Update`;
  - overflow `Modify App`;
  - unchanged `Edit Config`.
- Route `Update` through existing app/pending state:
  - derive a normalized app-detail update state;
  - pending config update opens the config-value update context;
  - pending manifest review opens the review context;
  - unknown pending flow blocks instead of falling through;
  - image/rootfs refresh blocks for persistent-data apps when rollback snapshot
    safety is unavailable or unknown;
  - otherwise run image/rootfs refresh.
- Update operation labels so progress/readiness says `Updating app` only for
  broad update and `Refreshing image` or equivalent for exact image refresh.
- Preserve entry display context across wizard handoff and recent-operation
  recovery so `Modify App`, catalog `Update`, direct `Edit Config`, and image
  refresh do not collapse into raw task-type labels.

### Phase 2 - Shared input and credential controls

- Extract a shared input presentation model for config and modify/update flows.
- Convert custom YAML input fields to the keep/replace/regenerate model.
- Preserve the backend rule that secrets are reused without browser display.
- Preserve optional secret clearing as an explicit action.
- Add legacy-missing copy where ledger/source recovery is unavailable.
- Surface kept-value review items when a full source replacement changes the
  meaning or usage of a reused input key.
- Fail closed when same-key value meaning or usage cannot be confidently
  compared.

### Phase 3 - Modify App wizard shape

- Reframe the current manifest wizard as `Modify App`.
- Split the experience into source, values, preview, and review states.
- Make the source step explicit that the YAML is a full replacement manifest.
- Keep catalog-pending source review out of this source-entry step.

### Phase 4 - Decision-first review surface

- Replace the raw summary-first layout with a decision hierarchy:
  - outcome;
  - why review is required;
  - grouped change sections;
  - preserve/data/readiness;
  - technical details.
- Move required confirmations inline into their corresponding sections.
- Keep raw classifier output available as details for power users.

### Phase 5 - Catalog pending copy and context

- Replace `Review Catalog Update` primary copy with `Continue Update` or
  `Update needs review` depending on placement.
- Keep `Source: app catalog` as provenance.
- Ensure config-pending and manifest-review-pending candidates route to the
  correct UI without exposing the flow enum as a user concept.

### Phase 6 - Operation, readiness, and repair grammar

- Align task-progress panels, operation banners, readiness banners, and access
  repair copy with the same taxonomy.
- Use operation display context before backend task type when naming progress,
  readiness, failure, and access-repair-pending states.
- Show access repair as a follow-up state of an update, not as an unexplained
  backend failure.
- Do not expose `Repair Access` as an action in this RFC; use `Check Again` or
  `Refresh` unless a later backend/API change adds a real repair command.
- Preserve existing app-detail operation ownership from
  `20260609-app-detail-operation-state.md`.

### Phase 7 - Validation

- Add targeted Flutter tests for:
  - `Update` routing by pending-flow state;
  - `Update` fail-closed behavior for unknown pending-flow state;
  - `Modify App` source/value/review labels;
  - operation display context in progress/readiness/failure copy;
  - keep/replace/regenerate credential controls;
  - optional secret clear;
  - kept-value review for changed input meaning/usage;
  - ambiguous same-key value reuse fails closed;
  - inline confirmation gating;
  - catalog pending copy;
  - task-progress display labels.
- Run `flutter analyze` and targeted widget tests.
- Run backend tests touched by field provenance or pending-flow behavior.
- Perform a manual browser pass over:
  - app with no pending update;
  - app with no pending update and persistent data but failed rollback snapshot
    preflight;
  - app with config-pending update;
  - app with manifest-review pending update;
  - app with unknown/unsupported pending update state;
  - custom app `Modify App` with shared existing values;
  - legacy custom app missing stored source/values;
  - access repair pending after update.

## Invariants to preserve

- Apply still requires a successful dry run and exact dry-run token for
  config/source candidates.
- Required confirmations still gate apply for review-required candidates.
- Secrets are never displayed or prefilled in the browser.
- Stored ledger secrets may be reused by the backend without display.
- App detail remains the active-operation owner after wizard submission.
- Unknown or unsupported pending update state fails closed and never falls
  through to image refresh.
- Persistent-data image/rootfs refresh fails closed when rollback snapshot
  safety is unavailable or unknown.
- Access repair pending is presented as state until a real repair command
  exists.
- Existing backend update transactions and recovery semantics are unchanged.
- Catalog pending flow enums remain internal routing data, not user-facing
  vocabulary.

## Implementation Notes & Status

Draft created after operator review of the first service app update UI. Reviewed
with structured RFC, red-team, UX, and minimizer passes, then implemented in the
current change set.

Current validation status:

- Code review cadence is green for the app-maintenance diff and for the
  follow-up alpha fixture change.
- Local Go and affected Flutter tests pass on the current tree.
- `flutter analyze` remains non-green only on pre-existing unrelated lint issues
  outside the app-maintenance files.
- Full alpha on a fresh VM passes with `PASS: 165`, `FAIL: 0`, `SKIP: 4`.
