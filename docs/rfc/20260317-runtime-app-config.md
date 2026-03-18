# RFC: Runtime App Config Updates & Array Inputs

**Date:** 2026-03-17
**Status:** Draft

## Problem

App config (`app_config` in app.yaml) is frozen at install time. To change any config value, the app must be uninstalled and reinstalled. This is unacceptable for operational config that changes over the app's lifetime.

**Motivating example:** The Namek server app has `nexus.trustedDomainSuffixes` — a list of trusted relay domains that operators need to add/remove as they provision new Nexus relays. Today this requires reinstalling Namek, losing uptime.

A secondary gap: the input system only supports scalar types (`string`, `password`, `boolean`). There is no `array` or `list` type, so fields like `trustedDomainSuffixes` that are naturally lists must be hacked as comma-separated strings.

## Scope

Two related features:

### 1. Runtime Config Update API

- `PATCH /api/v1/apps/:name/config` — accepts partial config updates (JSON merge-patch semantics)
- Merges into stored `AppDefinition.AppConfig`, persists, and rewrites `/piccolo/config/app.yaml`
- App notification strategy: container restart by default; apps may opt into file-watch reload (future)

### 2. Array Input Type

- New input type `array` (or `list`) for `app_config` template rendering
- UI: dynamic add/remove list of strings
- Template usage: `{{ .Inputs.trusted_suffixes }}` renders as YAML list

## Open Questions

- Should runtime config updates require app restart or support hot-reload signaling (e.g., SIGHUP)?
- Should the PATCH API accept arbitrary JSON or be constrained to declared config schema?
- How to handle merge semantics for nested arrays (replace vs append)?

## Out of Scope

- Per-service config overrides (all services share one app_config today)
- Config rollback / history (can be added later, builds on app snapshot tuples)
