# Piccolo UI (Flutter) Foundations

## North Star
*   **Charter:** "The Digital Sanctuary." Simple, personal, and beautiful.
*   **Strategy:** **Adaptive Shells**. We do not build a single responsive layout. We build dedicated shells for **Desktop** (Windowed) and **Mobile** (Fullscreen) that share core business logic and widgets.
*   **Tech Stack:** pure Flutter SDK. No heavy external state management libraries (BLoC/Riverpod) unless strictly necessary. We use `ChangeNotifier` + `ListenableBuilder`.

## Architecture

### 1. Folder Structure
```
ui/lib/
├── core/           # Shared business logic, API clients, models
├── theme/          # Design system (Cobalt Neutral)
├── shared/         # Reusable atomic widgets (Buttons, Chips, Logos)
├── shells/         # The Adaptive Entry Points
│   ├── desktop/    # 3-Layer Desktop Environment (Windows, Dock)
│   └── mobile/     # (Future) Touch-first Mobile Environment
└── main.dart       # Entry point (Selects shell based on platform/screen)
```

### 2. State Management
*   **Pattern:** `ChangeNotifier` (Controller) -> `ListenableBuilder` (View).
*   **Rule:** Logic lives in Controllers. Views are dumb and reactive.

## Design System: "Cobalt Neutral"

**Reference:** Mapped from `ui/docs/theme-brief.md`.
**Implementation:** `lib/theme/piccolo_theme.dart`, `lib/theme/piccolo_icons.dart`.

### Color Tokens (`PiccoloTheme`)

| Token | Value | Usage |
|---|---|---|
| `porcelain` | `#FFFFFF` | Card / surface background |
| `mist` | `#F4F6FB` | Page / stage background |
| `ink` | `#141821` | Primary text |
| `inkMuted` | `#141821` @ 55% | Secondary text, placeholders |
| `cobalt600` | `#2F5AF3` | Primary accent, links, filled buttons |
| `cobalt300` | `#7EA2FF` | Tints, hover states |
| `success` | `#22C55E` | Healthy / online states |
| `warning` | `#F59E0B` | Caution states |
| `critical` | `#EF4444` | Errors, destructive actions |
| `outline` | `#141821` @ 14% | Borders, dividers |
| `hairline` | `#141821` @ 8% | Subtle dividers |
| `overlay` | `#141821` @ 6% | Hover overlays |
| `scrim` | `#000000` @ 40% | Modal scrims |
| `disabledBg` | `#141821` @ 6% | Disabled backgrounds |
| `disabledFg` | `#141821` @ 38% | Disabled text |
| `terminalBg` | `#1E1E1E` | Terminal / code blocks |

### Spacing (`Spacing`)

| Token | px | Usage |
|---|---|---|
| `xs` | 4 | Tight internal gaps |
| `sm` | 8 | Button internal, chip gaps |
| `md` | 12 | Card internal padding |
| `base` | 16 | Standard content padding |
| `lg` | 24 | Section spacing |
| `xl` | 32 | Page margins |
| `xxl` | 40 | Major section breaks |

### Border Radius (`Radii`)

| Token | px | Usage |
|---|---|---|
| `xxs` | 4 | Tiny badges, protocol chips |
| `xs` | 6 | Tooltips, small badges |
| `sm` | 10 | Inputs, chips, menus |
| `md` | 14 | Cards, buttons, windows |
| `lg` | 20 | Dialogs, large panels |
| `xl` | 28 | Full-page overlays |
| `pill` | 999 | Pills, rounded buttons |

### Elevation (`Elevation`)

| Token | Usage |
|---|---|
| `elev0` | Flat (no shadow) |
| `elev1` | Subtle card lift |
| `elev2` | Popup menus |
| `elev3` | Windows, floating panels |
| `elev4` | Modals, dialogs |

### Motion (`Motion`)

| Token | Duration | Usage |
|---|---|---|
| `fast` | 120ms | Micro-interactions (hover, focus) |
| `medium` | 180ms | State changes (expand, select) |
| `slow` | 240ms | Window open/close, page transitions |

Curves: `Motion.standard` (general), `Motion.emphasized` (entrances/exits).

### Typography (`PiccoloTheme.textTheme`)

| Slot | Font | Size | Weight | Usage |
|---|---|---|---|---|
| `headlineLarge` | Comfortaa | 28 | Bold | Hero headings |
| `headlineMedium` | Comfortaa | 24 | Bold | Page titles |
| `headlineSmall` | Comfortaa | 20 | Bold | Card titles |
| `titleMedium` | Inter | 16 | SemiBold | Sub-section titles |
| `titleSmall` | Inter | 14 | SemiBold | Row labels |
| `bodyLarge` | Inter | 16 | Regular | Primary body text |
| `bodyMedium` | Inter | 14 | Regular | Default body text |
| `bodySmall` | Inter | 13 | Regular | Description text |
| `labelLarge` | Inter | 14 | Medium | Large button text |
| `labelMedium` | Inter | 12 | Medium | Button labels, chips |
| `labelSmall` | Inter | 11 | Medium | Captions |

Monospace: `PiccoloTheme.mono` (JetBrains Mono, 12px) for terminals, recovery keys, code blocks.

### Icons (`PiccoloIcons`)

All icons use **Phosphor Icons** (`phosphor_flutter` package) through a semantic mapping layer in `lib/theme/piccolo_icons.dart`. Never reference `PhosphorIconsRegular` directly — always use `PiccoloIcons.xxx`.

Categories: navigation, status, actions, window controls, content, system, files.

### Component Themes

Material component themes are configured in `PiccoloTheme.lightTheme` so standard widgets inherit correct styling automatically:

| Widget | Theme |
|---|---|
| `FilledButton` | Cobalt600 bg, white fg, `Radii.md` corners |
| `OutlinedButton` | Ink fg, outline border, `Radii.md` corners |
| `TextButton` | Cobalt600 fg |
| `TextField` | Filled, porcelain bg, `Radii.sm` corners |
| `Card` | Porcelain bg, hairline border, `Radii.md` corners |
| `Dialog` | `Radii.lg` corners |
| `PopupMenu` | `Radii.sm` corners |
| `TabBar` | Cobalt600 indicator |
| `Divider` | Hairline color |

### Shared Widgets

| Widget | File | Usage |
|---|---|---|
| `PiccoloCard` | `shared/widgets/piccolo_card.dart` | Unified content card with optional padding/elevation |
| `StatusDot` | `shared/widgets/status_dot.dart` | Colored dot with optional label (health indicators) |
| `StatusBanner` | `shared/widgets/status_banner.dart` | Inline status bar with severity (info/warning/error) |
| `InfoRow` | `shared/widgets/info_row.dart` | Key-value display row |

### Usage Rules

1. **Never use raw `Colors.xxx`** for theme colors — use `PiccoloTheme` tokens.
2. **Never use raw `BorderRadius.circular(n)`** — use `Radii` constants.
3. **Never use `Icons.xxx`** — use `PiccoloIcons.xxx`.
4. **Prefer `FilledButton`** for primary actions, `OutlinedButton` for secondary.
5. **Remove inline button/input styling** — component themes handle it.
6. **Layout-specific spacings** (e.g., stage visual balance) may remain as numeric literals; these are not design tokens.

## Workflow Guidelines
*   **Backlog:** Before every commit, developers must check if they have introduced any technical debt or deferred any features. If so, update `docs/backlog.md` immediately.

## Next Steps
*   Implement Setup Wizard (`docs/setup-wizard.md`).
*   Port Settings Logic.
*   See `docs/backlog.md` for the full list of deferred items and technical debt.
