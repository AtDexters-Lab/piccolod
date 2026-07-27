# Engineering Backlog

This document tracks features, technical debt, and architectural decisions.

## Completed Items
*   **Backend Integration:** Replaced mocks with real `ApiClient` talking to `piccolod`.
*   **Authentication:** Implemented Setup, Login, Logout, and Reset Password flows.
*   **Web/WASM Support:**
    *   Implemented `Downloader` using `package:web` for WASM compatibility.
    *   Implemented `Clipboard` utility with fallback for insecure contexts.
*   **UI Components:** Added reusable `PasswordSetForm` and `PasswordStrengthIndicator`.
*   **Settings - Remote Access:** Implemented the Nexus Remote Access tab (Setup Wizard, Preflight Checks, Dashboard, Secret Rotation).
*   **Terminal App:** Implemented full `xterm` integration with WebSocket backend (JSON/Base64 protocol).

## 1. App Implementation (Layer D)
*   **Current State:** "Settings" and "Files" launch generic placeholder windows. "Terminal" is functional.
*   **Task:** Implement the actual UI for these core applications.
    *   **Settings:** Tabbed interface for User and Storage management (Remote Access/Network completed).
    *   **Files:** File explorer interfacing with the backend VFS.

## 2. Mobile Shell
*   **Current State:** `ui/lib/shells/mobile/` is empty.
*   **Task:** Implement the Touch-first shell (Fullscreen app launcher, Swipe navigation) reusing the same `core` logic and `SetupWizard`.

## 3. Native Desktop Features (Native Targets)
*   **File Download (Native):** The current `downloader` supports Web/WASM. We need to implement `file_selector` or `path_provider` logic for Linux/Windows/macOS targets if we distribute native binaries.

## 4. Window Manager Polish
*   **Snapping:** Add ability to snap windows to left/right halves of the screen.
*   **Z-Ordering:** Currently just moves to end of list. Ensure "Always on Top" windows (like Dialogs) stay above standard windows.

## 5. Asset Management
*   **Icons:** We are using `Icons.search` etc. standard Material icons. Eventually should import the custom icon set from `ui-next` or use Phosphor icons as per design brief.
*   **Logo:** Currently using a `CustomPainter` for the logo. Evaluate using `flutter_svg` if asset complexity grows.

## 6. Capability Default Provider
*   **Current State:** Piccolod exposes capability inventory and acknowledged
    default-provider selection, but the UI has no affordance for changing the
    selected provider.
*   **Task:** On a provider's App Details surface, show whether it is the
    current default and offer **Set as default** only for enabled, non-default
    providers. For a disabled provider, explain that it must be enabled first.
    Reuse `GET /api/v1/capabilities` and
    `PUT /api/v1/capabilities/:capability/default`; show the server-provided
    interruption/state-migration disclosure before acknowledging a switch,
    and surface the existing selection task progress. Keep first-provider
    selection automatic and do not add a separate capability settings page
    for V1.
