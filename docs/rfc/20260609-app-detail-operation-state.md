# App Detail Operation State and Responsive Controls

**Date:** 2026-06-09
**Status:** Draft

## Scope

**Problem:** App detail becomes unusable when custom installed apps expose many controls, and long-running app operations can appear finished before containers and listener readiness have actually settled.
**In scope:** Responsive app-detail layout, action grouping, durable active-operation tracking, update-progress reattachment, and post-update readiness presentation for installed app detail views.
**Out of scope:** Rewriting the app image/rootfs transaction, changing app install/update semantics, redesigning the full desktop shell, changing deployed app homepages, and adding a new backend job system.

## Background

The installed app detail header currently composes app identity, image text, service selection, terminal access, configuration controls, image update, stop/start, rollback, and uninstall into one horizontal header. Custom installed apps add enough controls that the title can be squeezed into a vertical stack while the action row remains visually dominant.

The update flow has a separate lifecycle mismatch. The backend already emits task progress phases and keeps active task state, while the frontend treats a short HTTP timeout or brief progress-stream grace window as enough to dismiss the updater. A user can therefore see the updater close while the image pull, rootfs creation, container stop/remove/recreate, or listener readiness phase is still practically relevant.

The product expectation is simple: the app detail view should remain readable at normal desktop window sizes, and it should be the authoritative place to understand whether an app update is still running, complete, or waiting for the app listener to become healthy.

## Goals

- Keep app identity readable regardless of how many app actions are available.
- Make long-running app operations durable across dialog dismissal, app-detail remount, and browser reload while the daemon still has task-progress state.
- Reuse the existing task-progress reporter, progress stream, and active task endpoint unless a concrete gap appears during implementation.
- Distinguish backend operation completion from user-facing app readiness.
- Preserve the existing app update transaction and failure behavior.

## Non-Goals

- No new queue or scheduler for app operations.
- No change to image digest/rootfs/container replacement semantics.
- No change to listener health probing semantics.
- No global desktop-shell navigation redesign.
- No attempt to solve unrelated app-store or catalog sync UX.

## Current Surfaces

- `ui/lib/features/apps/app_detail_view.dart` owns the app detail header, tabs, action buttons, status banners, and app-specific SSE subscriptions.
- `ui/lib/core/services/app_service.dart` owns app lifecycle/update HTTP calls and active task lookup.
- `ui/lib/shared/widgets/task_progress_panel.dart` and `ui/lib/core/services/task_progress_client.dart` render and follow a single task progress stream.
- `ui/lib/core/models/task_progress.dart` models progress events consumed by the frontend.
- `ui/lib/features/apps/manifest_update_wizard.dart` and `ui/lib/features/apps/installed_config_wizard.dart` currently generate task IDs and own modal progress for manifest/config updates.
- `ui/lib/core/services/event_stream_client.dart` streams app status and listener health events.
- `ui/lib/core/models/listener_health.dart` models full listener-health state for frontend presentation.
- `internal/server/gin_app_handlers.go` exposes app start, stop, update, rollback, and active task endpoints.
- `internal/server/gin_progress_stream.go` replays the last progress event for a task and streams future task events.
- `internal/events/task_progress.go` stores active and recently completed task progress.
- `internal/services/manager.go` emits listener-health change hints and exposes full listener health through app/listener detail reads.
- `internal/services/health.go` defines listener-health status meanings and backend-health debounce behavior.

## Decisions

### D1 - Split App Identity From App Actions

The app detail header should no longer place identity and all actions in one row.

The stable top area shows:

- app icon;
- display title;
- lifecycle status badge;
- primary image/reference metadata;
- optional selected service context for multi-container apps.

Actions move to a separate operation toolbar below or beside the identity area depending on available width. The toolbar may wrap, but identity text must not collapse into per-character vertical layout. The app title uses bounded lines and truncation when needed.

Primary actions remain directly visible:

- terminal, when the app is running and terminal access is available;
- start or stop;
- update image, when supported;
- the most relevant configuration action for the current app mode.

Secondary or destructive actions may move into an overflow/menu surface when width is constrained:

- apply YAML;
- edit listeners or other advanced configuration;
- rollback;
- uninstall.

Overflow grouping must preserve consequence hierarchy. Ordinary configuration actions appear first. A visual divider separates danger-zone actions. Rollback and uninstall remain visually distinct and keep precise confirmation labels.

The responsive target is not merely "no horizontal overflow." At normal app-window widths, the identity area remains readable and the operation band should not consume the whole first viewport. At constrained widths, lower-priority actions move to overflow before the operation band grows into a tall stack.

The implementation must validate at least these fixtures:

- long app title plus long image reference;
- multi-container service selector visible;
- terminal, edit config, apply YAML, update image, rollback, stop/start, and uninstall all available;
- active operation banner visible at the same time as the toolbar;
- narrow desktop window and normal desktop window sizes.

The multi-container service selector remains context selection, not a primary lifecycle action. Its selected value must continue to drive service-specific terminal and environment/log contexts.

### D2 - Make Active Operations First-Class State in App Detail

App detail should maintain an `activeOperation` concept derived from task progress rather than from a local modal alone.

The view should discover active tasks on load and after app list refreshes. It should select tasks whose `instance_id` matches the current app and whose task type is relevant to app detail:

- `update_image`;
- `update_config`;
- `update_manifest`;
- `update_listeners`;
- `start_app`;
- `stop_app`;
- `rollback_app`;
- `uninstall_app`.

If more than one matching task exists, the UI should prefer the newest non-complete task. The implementation should not create a new concurrency policy; backend operation locking remains authoritative.

The active operation should be visible inline in the app detail view. A modal progress panel may still be used for the action the user just triggered, but closing or losing the modal must not hide the operation from the app detail view.

Durability is intentionally bounded to the current progress system:

- app detail survives dialog dismissal and remount by reattaching to known task IDs;
- browser reload survives when `/tasks/active` or progress replay still knows the task;
- daemon restart and progress eviction are not solved by this RFC.

To make browser reload meaningful at the update-completion boundary, the frontend keeps a bounded recent-submission record for same-app operations it starts. The record shape is:

- app instance ID;
- task ID;
- task type;
- submitted-at timestamp;
- expiry timestamp.

The record is cleared on immediate request rejection, on no-progress submission timeout, or after app detail consumes task completion and finishes the relevant refresh/readiness handoff. The expiry should cover the longest supported app operation window plus the progress replay grace period; the first implementation target is 35 minutes from submission.

If app detail cannot find an active task or replay event for a recently submitted same-app operation, it must refresh app detail/listener state and present an unknown/checking state rather than declaring success from absence of task progress.

Same-app mutating operations are mutually exclusive in the UI. While an active operation exists, conflicting lifecycle, update/configuration, rollback, and uninstall controls are disabled or moved behind an explanation that names the active operation. Read-only surfaces such as Overview, Network, Configuration, and Logs remain available. A later accepted same-app mutation cancels any prior post-update readiness observation because the app is no longer in the post-update state being observed.

### D3 - Reattach Progress by Task ID

When the app detail view knows an active task ID, it should be able to subscribe to that task's progress stream and receive the last event replay plus future events.

The single-task `TaskProgressPanel` remains a valid building block, but it should not be the only owner of completion state. App detail owns the current active operation and uses the progress panel or a compact variant to present progress.

Task completion clears the active operation only after the app detail view has consumed the completion event and refreshed app detail state.

Manifest/config update wizards remain valid task-progress observers, but they do not become the sole owner of their task IDs. When a wizard starts `update_manifest` or `update_config`, app detail must learn the task ID and track it as the same `activeOperation` concept used for image updates. Wizard completion may close the wizard, but it must not erase app-detail operation/readiness state before app detail refreshes.

### D4 - Treat HTTP Response as Submission Feedback, Not Operation Truth

For long app operations, the HTTP response tells the frontend whether the operation request was accepted or failed immediately. It is not the sole truth for operation completion.

Frontend action handling should therefore:

- generate and pass a task ID before invoking the operation;
- show a local `submitting` state immediately;
- promote the task to `accepted/running` only after first progress, active-task confirmation, or a successful immediate backend response for a short operation;
- surface immediate HTTP errors as request failures;
- keep tracking progress if the HTTP call times out or the connection closes after progress has started;
- clear the local submitting state with an error or unknown/checking presentation if no progress or active-task confirmation appears within a bounded no-progress window;
- avoid declaring success solely because the POST returned or timed out.

This preserves the existing backend shape where long operations run on a server-scoped operation context and emit progress independently of the browser connection.

### D5 - Add a Post-Update Readiness Presentation

After an `update_image` task completes successfully, app detail should refresh app detail state and enter a short readiness observation state for the app's primary listener when one exists.

Readiness observation starts when app detail consumes a successful `update_image` completion event. It also starts when app detail reloads with a recent-submission record for an `update_image` task but no active/replay task is available, because the task may have completed before the view reattached.

Those two entry paths have different copy. A successful completion event or replayed completion can say the update completed and then report readiness. A recent-submission fallback without completion evidence must preserve uncertainty: the UI may report that the app is currently ready/checking/needs attention, but the operation outcome is "update status unknown" until a later refresh or user action confirms the installed image/version.

Listener-health events are treated as hints. During readiness observation, app detail refreshes full app/listener detail after update completion and after relevant listener-health changes. A listener-health sample can prove readiness only if it is fresh relative to the readiness observation start, either by timestamp or by a full detail refresh performed during the observation.

The readiness presentation has these outcomes:

- **Ready:** app status is running and primary listener health is OK.
- **Ready with warnings:** app status is running and the primary listener is degraded, or the primary listener is OK/degraded while one or more exposed secondary listeners are degraded, recovering, or error.
- **Running, checking listener:** app status is running, but relevant listener health is missing, stale, unknown, recovering, or still refreshing within the observation window.
- **Needs attention:** app status is error, a fresh primary listener sample is error, or the observation window elapses without a fresh OK/degraded primary-listener result.

The observation window is bounded by existing health cadence: backend checks run every 15 seconds, and unhealthy reporting requires three consecutive failures. The frontend should not declare "Needs attention" solely from missing health before a window that covers that cadence plus margin. The target window for the first implementation is 75 seconds from readiness observation start.

Raw-only apps or apps without a primary browser listener should not be presented as browser-ready. Their successful update state is "Updated" with Network and Logs as the relevant next surfaces.

Readiness wording must account for multi-listener apps. A healthy primary listener may make the app's main browser surface ready, but app detail must not hide unhealthy exposed secondary listeners. Secondary listener problems appear as warnings or attention items in the operation/readiness band and Network tab.

This is a UI composition over existing app status and listener health. A backend API addition is only justified if implementation proves the frontend cannot distinguish these outcomes with current data.

### D6 - Keep Tabs Stable and Independent

The existing tabs remain `Overview`, `Network`, `Configuration`, and `Logs`.

The fix should not add new top-level tabs for app actions. Operation state belongs in the header/operation band because it affects the whole app, not one tab.

## Implementation Plan

1. Refactor app detail header composition.
   - Separate identity/status/image metadata from app operations.
   - Add responsive constraints so title, image metadata, and controls cannot force each other into unreadable layouts.
   - Keep tabs and tab content behavior unchanged.

2. Introduce app-detail active operation state.
   - Load active tasks through the existing app service.
   - Filter by app instance ID and app-detail-relevant task types.
   - Reattach progress display to the selected task.
   - Bound durability to current daemon task-progress state and define the unknown/checking fallback when replay is absent.

3. Adjust action handling around task ownership.
   - Generate task IDs before invoking lifecycle/update operations.
   - Register a local submitting state as soon as the action begins.
   - Promote to active operation only after backend acceptance, first progress, or active-task confirmation.
   - Treat modal progress as an observer of active operation state, not as the owner of completion.
   - Share manifest/config wizard task IDs with app-detail active-operation state.

4. Add post-update readiness presentation.
   - After update task completion, refresh app detail.
   - Treat listener-health events as refresh hints, not standalone truth.
   - Observe app status, primary listener health, and exposed secondary listener warnings.
   - Show an inline readiness/attention banner until the app is clearly ready, ready with warnings, not browser-checkable, or clearly needs attention.

5. Keep backend changes conditional.
   - First attempt implementation using existing task progress replay, active task listing, app status events, and listener health events.
   - Add backend fields or endpoint behavior only if the frontend cannot faithfully compose required state from current surfaces.

6. Validate with focused tests and runtime checks.
   - Flutter analysis/test coverage for app-detail state selection and responsive layout where practical.
   - Existing Go tests only if backend changes are made.
   - Manual or screenshot validation at narrow and normal desktop window widths using the responsive fixture matrix.
   - Runtime validation of a slow `update_image` path showing progress through completion and readiness.

## Invariants

- Existing app operation authorization remains unchanged.
- Existing update, rollback, and stop/start backend semantics remain unchanged.
- Task progress events remain keyed by task ID.
- Active task listing remains a discovery mechanism, not a concurrency authority.
- Durable operation tracking does not survive daemon restart or progress eviction.
- App detail refreshes do not drop active operation state while the task remains active.
- Absence of progress is never treated as proof of operation success.
- Same-app mutating actions are disabled or explained while another same-app mutation is active.
- Listener-health events are hints; readiness uses fresh full app/listener detail during observation.
- Destructive actions remain explicit and visually separated from ordinary operations.
- Multi-container selected-service behavior remains stable.

## Site List

- `ui/lib/features/apps/app_detail_view.dart`
- `ui/lib/features/apps/manifest_update_wizard.dart`
- `ui/lib/features/apps/installed_config_wizard.dart`
- `ui/lib/core/services/app_service.dart`
- `ui/lib/core/services/event_stream_client.dart`
- `ui/lib/shared/widgets/task_progress_panel.dart`
- `ui/lib/core/services/task_progress_client.dart`
- `ui/lib/core/models/task_progress.dart`
- `ui/lib/core/models/listener_health.dart`
- `internal/server/gin_app_handlers.go`
- `internal/server/gin_progress_stream.go`
- `internal/events/task_progress.go`

Backend reference surfaces, to be edited only if frontend composition proves insufficient:

- `internal/app/app_manager.go`
- `internal/services/manager.go`
- `internal/services/health.go`

## Deferred

- Global stage/dock visibility for non-install app operations is intentionally deferred. This RFC keeps the authoritative operation surface inside app detail; global operation awareness can be designed separately if operators need to leave app detail during long updates.
