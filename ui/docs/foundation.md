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
*   **Reference:** Mapped from `ui-next/docs/theme-brief.md`.
*   **Colors:** Mist (`#F4F6FB`) backgrounds, Cobalt (`#2F5AF3`) accents, Inter typography.
*   **Theme Data:** Defined in `lib/theme/piccolo_theme.dart`.

## Next Steps
*   Implement Setup Wizard (`docs/setup-wizard.md`).
*   Port Settings Logic.
