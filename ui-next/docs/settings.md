# Settings Activity Playbook

Goal: Deliver a split-view Settings app (desktop) and stacked drill-down (mobile) that embodies “Smart defaults, total control” without overwhelming the user.

## Layout & Navigation
- Two-pane split view on ≥768px: sidebar categories on the left, detail pane on the right.
- Stack on mobile: landing page lists categories; tapping opens the detail view, back/gesture returns to the list.
- Categories (MVP): Profile (password + recovery key), Appearance (theme/wallpaper/density), Remote access (piccolo.link), Updates (OS version/policy).

## Status & Safety Patterns
- Surface live status inline at the top of the first card in each section (pills/text), not in a separate hero bar.
- Optimistic toggles: apply immediately, revert + toast on failure; keep inline error/lastError visible when the backend rejects input.
- Destructive actions live in confirmation modals that require typing a phrase (danger zone pattern).

## Data Sources (current)
- Profile: `/auth/session`, `/crypto/recovery-key`, `/crypto/recovery-key/generate`, `/auth/staleness/ack`.
- Appearance: uses `preferencesStore`; **temporary local-storage persistence** until server preferences API lands.
- Remote access: **stubbed** in `lib/api/remote.ts` pending backend endpoint.
- Updates: **stubbed** in `lib/api/updates.ts` pending `/updates/*` API.

## States to Cover
- Profile: recovery key present/missing/stale; generated words; acknowledgement success/failure.
- Remote access: connected / connecting / disconnected / error; domain validation failures.
- Updates: up-to-date vs. update available; auto-update policy toggle success/failure.
- Mobile vs. desktop rendering paths.

## Open Questions / TODO
- Wire Appearance to server preferences endpoint when available; remove local-storage shim.
- Replace remote/updates stubs with real APIs and add Svelte Query caches.
- Add screenshot coverage for Settings (light/dark) once flows stabilize.
- Decide on “canonical” breakpoint; currently using `min-width: 768px` per tablet 8-col grid.
- Consider reducing shadow spread or increasing container padding if future components add larger elevations.

## Test Notes
- Add Playwright coverage for: mobile stack navigation, optimistic toggle revert-on-error, danger modal confirmation, recovery key generation.
- Contrast: ensure hero pills and chips meet AA on light/dark.

Keep this doc updated as endpoints land or clusters expand (Storage, Backups, Power).
