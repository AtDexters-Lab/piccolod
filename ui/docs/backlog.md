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

## 1. App Implementation (Layer D)
*   **Current State:** "Settings", "Files", and "Terminal" launch generic placeholder windows.
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
