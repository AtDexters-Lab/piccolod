# RFC: Unified App Identity Model

- **Status:** Ready for Implementation
- **Date:** 2026-01-30
- **Authors:** Engineering Team
- **Reviewers:** @piccolo-os/core

## 1. Summary

This RFC proposes a simplification of Piccolo's app identity model by:

1. **Removing the `name` field** from app.yaml entirely
2. **Introducing `__primary` magic listener name** that is always collected from user input at install time
3. **Introducing `workspace_name`** for workspace apps without listeners
4. **Deriving instanceID from primary listener name** (or workspace_name)
5. **Using listener name for hostname derivation** instead of instanceID
6. **Removing auto-suffixing on collision** - fail with clear error instead
7. **Removing `display_name`** - instanceID becomes the sole display identifier

These changes eliminate confusion between "app type" vs "subdomain" and provide a consistent, predictable identity model.

## 2. Motivation

### 2.1 Current Confusion

The current model has multiple identity concepts that cause confusion:

| Field | Purpose | Example |
|-------|---------|---------|
| `name` | App manifest name, seeds instanceID | `"wordpress"` |
| `instanceID` | System identifier, storage/container naming | `"wordpress"` or `"wordpress-a7b2"` |
| `display_name` | Optional UI-friendly name | `"My Blog"` |
| `listener.name` | Listener identifier | `"web"`, `"blog"` |

**Problem observed:** User installs WordPress with subdomain input "blog":
- Expected: URL = `blog-piccolo.local`
- Actual: URL = `wordpress-piccolo.local` (because `DeriveHostLabel` uses instanceID for primary)
- Cert notification shows "blog" (listener name)

### 2.2 Design Assumption Mismatch

The current design assumes:
- **App name = user's desired subdomain** (app.yaml has `name: "{{ .Inputs.subdomain }}"`)

But common app.yaml patterns use:
- **App name = app type** (e.g., `name: wordpress`)
- **Listener name = user's desired subdomain** (e.g., `name: "{{ .Inputs.subdomain }}"`)

### 2.3 Unnecessary Complexity

- `display_name` adds another identity layer without clear benefit
- Auto-suffixing (`blog-a7b2`) creates surprising, hard-to-remember identifiers
- `inputs.subdomain` is a convention, not enforced by the system

## 3. Goals & Non-Goals

### 3.1 Goals

- **Single source of truth:** One identifier (primary listener name or workspace_name) drives everything
- **Predictable URLs:** User chooses "blog" → URL is `blog-piccolo.local`, always
- **Clear collision handling:** Fail with actionable error, not auto-suffix
- **Simplified schema:** Remove redundant fields (`name`, `display_name`)
- **Enforced UX:** System guarantees user can always set their subdomain

### 3.2 Non-Goals

- Backward compatibility with existing installed apps (breaking change)
- Supporting multiple primary listeners
- Changing the hostname derivation format (still `<label>-<base>.local`)

## 4. Proposed Design

### 4.1 Identity Derivation

```
┌─────────────────────────────────────────────────────────┐
│                    APP INSTALLATION                      │
├─────────────────────────────────────────────────────────┤
│                                                         │
│  Has listeners?                                         │
│       │                                                 │
│       ├── YES ──→ Must have exactly one "__primary"     │
│       │           └── User provides value at install    │
│       │               └── instanceID = user value       │
│       │                                                 │
│       └── NO ───→ Must have "workspace_name" field      │
│                   └── instanceID = workspace_name       │
│                                                         │
└─────────────────────────────────────────────────────────┘
```

### 4.2 The `__primary` Magic Listener Name

**Rule:** If app has ≥1 listeners, exactly ONE must be named `__primary`.

```yaml
# app.yaml
listeners:
  - name: __primary          # Magic marker - REQUIRED
    guest_port: 80
    protocol: http
  - name: api                # Additional listeners use normal names
    guest_port: 8080
    protocol: http
```

**Install flow:**
1. Backend parses app.yaml, finds `__primary` listener
2. UI shows input field: "App Address" (pre-filled with smart default if available)
3. User enters "blog"
4. Backend validates "blog" is available (no collision)
5. Backend replaces `__primary` with "blog" in the manifest
6. Proceeds with install: instanceID = "blog"

#### 4.2.1 UI API Contract (Backend Concern)

The `__primary` marker is purely a backend concern. The UI does not need awareness of this magic name.

**Configure endpoint behavior** (`GET /api/v1/catalog/:name/configure`):

When backend detects a `__primary` listener, it injects a synthetic input into the schema response:

```json
{
  "inputs": {
    "__app_address__": {
      "type": "string",
      "label": "App Address",
      "description": "The subdomain for accessing this app (e.g., 'blog' → blog-piccolo.local)",
      "required": true,
      "validation": {
        "regex": "^[a-z][a-z0-9]{0,15}$",
        "message": "Lowercase letters and numbers only, must start with letter, max 16 chars"
      },
      "default": "<smart-default>"
    },
    // ... other inputs from app.yaml
  }
}
```

The UI renders this like any other input. On install submission, backend:
1. Extracts `__app_address__` from user inputs
2. Validates availability (collision check)
3. Replaces `__primary` with the user value in the manifest
4. Proceeds with install

#### 4.2.2 Validation Timing

The `__primary` magic name is exempt from listener name validation during initial parsing. Validation occurs **after** substitution:

```
Parse app.yaml → __primary detected (exempt from regex check)
     ↓
User provides value "blog"
     ↓
Replace __primary with "blog"
     ↓
Validate "blog" against ^[a-z][a-z0-9]{0,15}$ ✓
     ↓
Proceed with install
```

**Validation rules:**
- `__primary` MUST appear exactly once if listeners array is non-empty
- `__primary` MUST NOT appear if listeners array is empty
- Other listeners MUST NOT be named `__primary`

### 4.3 The `workspace_name` Field

**Rule:** If app has NO listeners, `workspace_name` is REQUIRED.

```yaml
# Workspace without listeners
workspace_name: mydev        # REQUIRED when no listeners
listeners: []                # Empty or omitted
x-piccolo:
  mode: workspace
```

**Validation rules:**
- `workspace_name` MUST be present if listeners is empty/omitted
- `workspace_name` MUST NOT be present if listeners is non-empty
- Same validation as listener names: `^[a-z][a-z0-9]{0,15}$`

### 4.4 Hostname Derivation Changes

**Current `DeriveHostLabel`:**
```go
func DeriveHostLabel(app, listener string, primary, eligible bool) string {
    if primary {
        return app       // Returns instanceID
    }
    return listener + "-" + app
}
```

**Proposed `DeriveHostLabel`:**
```go
func DeriveHostLabel(app, listener string, primary, eligible bool) string {
    if primary {
        return listener  // Returns listener name (which equals instanceID now)
    }
    return listener + "-" + app
}
```

**Result:** For apps with listeners, primary listener name = instanceID = host label. All three are identical.

### 4.5 Collision Handling

**Current:** `GenerateInstanceID("blog", existing)` tries "blog", then "blog-0000", "blog-1a2b"...

**Proposed:** Fail immediately with actionable error.

#### 4.5.1 Two-Step Collision Validation

Collision detection uses a two-step approach that guarantees uniqueness of all derived host labels:

**Step 1: Primary listener name uniqueness across all apps**
```go
func ValidatePrimaryNameAvailable(name string, existingPrimaryNames []string) error {
    for _, existing := range existingPrimaryNames {
        if existing == name {
            return fmt.Errorf("name '%s' is already in use", name)
        }
    }
    return nil
}
```

**Step 2: Secondary listener name uniqueness within the app**
- Enforced by existing parser validation (no duplicate listener names within an app)

#### 4.5.2 Why This Is Sufficient

Given these constraints:
1. Primary listener names are unique across all apps
2. Listener names are unique within an app
3. **No hyphens allowed in listener names** (existing validation: `^[a-z][a-z0-9]{0,15}$`)

Then all derived host labels are guaranteed unique:

| Label Type | Format | Example | Uniqueness Guarantee |
|------------|--------|---------|---------------------|
| Primary | `<primary>` | `blog` | Rule 1: primary names unique across apps |
| Secondary | `<secondary>-<primary>` | `api-blog` | Hyphen separator + Rule 1 + Rule 2 |

**Proof by contradiction:** Could `api-blog` (secondary) collide with `apiblog` (primary)?
- No: `apiblog` has no hyphen, `api-blog` has a hyphen. They are different strings.
- The no-hyphen constraint on names makes the hyphen an unambiguous separator.

**Cross-app secondary collision:** Could App A's `metrics-blog` collide with App B's `metrics-photos`?
- No: Different primary names (`blog` vs `photos`) guarantee different secondary labels.

**User experience:**
```
Error: Name 'blog' is already in use. Please choose a different name.
```

### 4.6 Removed Fields

| Field | Status | Replacement |
|-------|--------|-------------|
| `name` (app-level) | REMOVED | Primary listener name or `workspace_name` |
| `display_name` | REMOVED | instanceID is the display name |
| `inputs.subdomain` convention | REMOVED | `__primary` listener is the subdomain |
| `primary` (listener field) | REMOVED | `__primary` magic name replaces this |

#### 4.6.1 Removal of `listener.primary` Field from app.yaml

The current `primary: true` boolean on listeners is redundant with `__primary`:

**Current behavior:**
```yaml
listeners:
  - name: web
    primary: true      # Explicit primary marker
    guest_port: 80
  - name: api
    guest_port: 8080
```

**New behavior:**
```yaml
listeners:
  - name: __primary    # Magic name IS the primary marker
    guest_port: 80
  - name: api
    guest_port: 8080
```

The `__primary` magic name serves dual purpose:
1. Marks which listener is primary (replaces `primary: true`)
2. Indicates the name should be collected from user input

#### 4.6.2 Internal Primary Flag Handling

The `Primary` bool field is **removed from app.yaml input** but **retained internally**:

1. **Validation:** `primary: true` in app.yaml is rejected with error
2. **Substitution:** When `__primary` is replaced with user value, `Primary = true` is set on that listener programmatically
3. **Runtime:** `ResolvePrimaryListener()` continues to check `l.Primary` as before

```go
// During __primary substitution:
for i, l := range appDef.Listeners {
    if l.Name == "__primary" {
        appDef.Listeners[i].Name = userProvidedValue
        appDef.Listeners[i].Primary = true  // Set programmatically
        break
    }
}
```

This keeps runtime behavior unchanged while enforcing `__primary` as the only way to mark a primary listener.

**Migration:** Any existing `primary: true` usage is replaced by naming that listener `__primary`.

## 5. Schema Changes

### 5.1 app.yaml Schema

```yaml
# REMOVED: name field no longer exists
# name: exampleapp  ← DELETE THIS

# For apps WITH listeners:
listeners:
  - name: __primary           # REQUIRED magic name, replaced at install
    guest_port: 80
    protocol: http

# For workspace apps WITHOUT listeners:
workspace_name: myworkspace   # REQUIRED when no listeners
listeners: []

# Rest of schema unchanged
services:
  main:
    image: ubuntu:22.04
    bind_ports: [80]
x-piccolo:
  mode: service  # or workspace
```

### 5.2 AppDefinition Struct Changes

```go
type AppDefinition struct {
    // Name          string            `yaml:"name"`           // REMOVED
    WorkspaceName    string            `yaml:"workspace_name"` // NEW: required iff no listeners
    Type             string            `yaml:"type"`
    Services         map[string]...    `yaml:"services"`
    Listeners        []AppListener     `yaml:"listeners"`
    // ... rest unchanged
}

type AppListener struct {
    Name      string           `yaml:"name"`       // Can be "__primary" (magic) or normal name
    GuestPort int              `yaml:"guest_port"`
    Flow      ListenerFlow     `yaml:"flow,omitempty"`
    Protocol  ListenerProtocol `yaml:"protocol,omitempty"`
    Primary   bool             `yaml:"-" json:"primary,omitempty"` // Internal only, not from YAML
    Middleware  []AppProtocolMiddleware `yaml:"protocol_middleware,omitempty"`
    RemotePorts []int                   `yaml:"remote_ports,omitempty"`
    Auth        *ListenerAuth           `yaml:"auth,omitempty"`
}
```

### 5.3 AppMetadata Struct Changes

```go
type AppMetadata struct {
    InstanceID       string            `json:"instance_id"`
    // DisplayName   string            `json:"display_name"`   // REMOVED
    // AppName       string            `json:"app_name"`       // REMOVED (was for backward compat)
    Status           string            `json:"status"`
    // ... rest unchanged
}
```

### 5.4 Install API Changes

```go
// Current
func (m *AppManager) Install(ctx, appDef, displayName) (*AppInstance, error)

// Proposed
func (m *AppManager) Install(ctx, appDef) (*AppInstance, error)
```

The `displayName` parameter is removed. The `__primary` listener name (provided by user) becomes the identity.

## 6. Validation Rules Summary

| Condition | Rule |
|-----------|------|
| listeners non-empty | Exactly one listener MUST be named `__primary` |
| listeners non-empty | `workspace_name` MUST NOT be present |
| listeners empty | `workspace_name` MUST be present |
| listeners empty | `__primary` MUST NOT appear |
| Any name | Must match `^[a-z][a-z0-9]{0,15}$` |
| Any name | Must not be reserved (see below) |
| Install | Name must not already be in use (no auto-suffix) |

**Reserved names:** `api`, `www`, `admin`, `root`, `system`, `piccolo`, `piccoloos`, `__primary`

Note: The codebase currently has separate lists (`ReservedAppNames` and `ReservedListenerNames`). Since primary listener names now serve as both app identifiers and listener names, use the full `ReservedAppNames` list plus `__primary` for all validation. The `__primary` reservation prevents secondary listeners from using this magic name.

## 7. Migration

### 7.1 Breaking Change

This is a **breaking change**. Existing apps will not be compatible.

**Migration path:**
1. Export app data if needed
2. Uninstall existing apps
3. Update app.yaml to new schema
4. Reinstall apps

### 7.2 App Store Updates

All apps in piccolo-store must be updated:

**Before:**
```yaml
name: "{{ .Inputs.subdomain }}"
inputs:
  subdomain:
    type: string
    label: "Subdomain"
    default: "blog"
listeners:
  - name: web
    guest_port: 80
```

**After:**
```yaml
# name field removed
# inputs.subdomain removed
listeners:
  - name: __primary       # User will be prompted for this
    guest_port: 80
```

## 8. Implementation Plan

### 8.1 Backend Changes

1. **api/types.go:**
   - Add `WorkspaceName` to AppDefinition, remove `Name`
   - Change `Primary` field on AppListener: `yaml:"-"` (not parsed from YAML, set programmatically)
2. **app/parser.go:**
   - Update validation for new rules
   - Add `__primary` exemption during pre-substitution parsing
   - Add mutual exclusivity validation (listeners vs workspace_name)
   - Add validation to REJECT `primary: true` in YAML (use loose parsing to detect)
3. **app/app_manager.go:**
   - Remove `displayName` parameter from Install
   - Derive instanceID from primary listener name or workspace_name
4. **app/instance_id.go:**
   - Replace `GenerateInstanceID` with `ValidatePrimaryNameAvailable`
   - Check against existing primary listener names (instanceIDs)
5. **app/filesystem.go:** Remove `AppName`, `DisplayName` from metadata
6. **app/smart_defaults.go:**
   - Update to detect `__primary` listener and inject `__app_address__` synthetic input
   - Update `FindFreeSubdomain` to generate `"blog1"` format (not `"blog-1"`) to comply with no-hyphen constraint
7. **hostname/hostname.go:**
   - Update `DeriveHostLabel` to return listener name for primary
   - `ResolvePrimaryListener` unchanged (still checks `l.Primary` bool, which is set programmatically)
   - Add `__primary` to reserved names list (prevents secondary listeners using magic name)
   - Unify reserved names: use `ReservedAppNames` list for all listener name validation
8. **services/manager.go:**
   - `ServiceEndpoint.Primary` field KEPT (runtime computed, not configuration)
   - Compute primary from listener name == resolved primary name (from `__primary`)
   - Collision checks remain unchanged (already validates all derived host labels)
9. **server/gin_app_handlers.go:**
   - Configure endpoint: inject `__app_address__` input when `__primary` detected
   - Install endpoint: extract `__app_address__`, replace `__primary`, proceed

### 8.2 UI Changes

1. **No changes required** - UI just renders inputs from configure endpoint
2. **App display:** Use instanceID as display name (no separate display_name)

### 8.3 Documentation

1. Update `docs/app-platform/specification.yaml`
2. Update this RFC with implementation notes
3. Update piccolo-store app.yaml files

## 9. Alternatives Considered

### 9.1 Keep `name` but template it

**Rejected:** Still causes confusion; app authors might not template it correctly.

### 9.2 Auto-inject input for primary listener

**Rejected:** More complex than `__primary` magic name; harder to understand.

### 9.3 Keep auto-suffixing

**Rejected:** Leads to confusing identifiers like `blog-a7b2`; better to fail clearly.

### 9.4 Keep `display_name`

**Rejected:** Unnecessary complexity; instanceID is sufficient for display.

### 9.5 Use `primary: true` boolean instead of `__primary` magic name

Suggested alternative:
```yaml
listeners:
  - name: ""              # Empty - user provides at install
    guest_port: 80
    primary: true         # Marks as identity-providing
```

**Rejected:** This creates awkward developer experience with two tightly coupled fields (`name: ""` + `primary: true`). The `__primary` approach encodes both meanings in a single field, making the intent clearer and reducing the chance of misconfiguration.

## 10. Design Decisions

### 10.1 Workspace Listener Addition (Accepted Asymmetry)

When listeners are added to a running workspace (originally installed with `workspace_name`), the instanceID remains the workspace_name.

**Example:**
- Workspace installed with `workspace_name: mydev` → instanceID = "mydev"
- User later adds listener "web" as primary
- Host label becomes "web" → URL: `web-piccolo.local`
- UI shows app name as "mydev" (instanceID)

**This creates an asymmetry:**
- For listener-based apps: instanceID = primary listener name = host label (all identical)
- For evolved workspaces: instanceID ≠ primary listener name

**Decision:** This asymmetry is **accepted** because:
1. instanceID is an internal identifier, not user-facing in URLs
2. User explicitly chose both names (workspace_name at install, listener name when adding)
3. Code should not assume instanceID = primary listener name; they are conceptually separate
4. The alternative (renaming instanceID on listener addition) would break storage paths, container names, etc.

### 10.2 Smart Defaults

Pre-fill suggested names using `FindFreeSubdomain` for the `__app_address__` synthetic input.

If "blog" is taken, suggest "blog1", "blog2", etc. as the default value. User can still change this, and if they enter a taken name, installation fails with a clear error.

**Implementation note:** The current `FindFreeSubdomain` generates candidates with hyphens (`"blog-1"`, `"blog-2"`), but hyphens are prohibited in listener names. The implementation MUST change the format to `"blog1"`, `"blog2"` (no hyphen separator).

## 11. References

- RFC 20260114: Hostname Scheme & Routing
- RFC 20260122: Two-Level mDNS Domains
- `docs/app-platform/specification.yaml`
