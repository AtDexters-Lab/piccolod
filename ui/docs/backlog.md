# Engineering Backlog

This document tracks features, technical debt, and architectural decisions that were deferred during the initial "Foundation" phase.

## 1. Backend Integration
*   **Current State:** `SetupController` and `DesktopController` use `Future.delayed` to simulate network calls.
*   **Task:** Implement a real `ApiService` in `core/services/api` using the `http` package.
*   **Endpoints:**
    *   `GET /crypto/status` (Check initialization)
    *   `POST /crypto/setup` (Initialize encryption)
    *   `POST /auth/login` (Admin authentication)

## 2. Native Desktop Features
*   **File Download:** The `downloader` utility uses `dart:html` for Web. The Desktop stub currently only logs to console.
*   **Task:** Implement real file saving for Linux/Windows/macOS using `file_selector` or `path_provider`.

## 3. App Implementation (Layer D)
*   **Current State:** "Settings", "Files", and "Terminal" launch generic placeholder windows.
*   **Task:** Implement the actual UI for these core applications.
    *   **Settings:** Tabbed interface for User, Network, and Storage management.
    *   **Files:** File explorer interfacing with the backend VFS.

## 4. Mobile Shell
*   **Current State:** `ui/lib/shells/mobile/` is empty.
*   **Task:** Implement the Touch-first shell (Fullscreen app launcher, Swipe navigation) reusing the same `core` logic and `SetupWizard`.

## 5. Window Manager Polish
*   **Snapping:** Add ability to snap windows to left/right halves of the screen.
*   **Z-Ordering:** Currently just moves to end of list. Ensure "Always on Top" windows (like Dialogs) stay above standard windows.

## 6. Asset Management
*   **Icons:** We are using `Icons.search` etc. standard Material icons. Eventually should import the custom icon set from `ui-next` or use Phosphor icons as per design brief.
*   **Logo:** Currently using a `CustomPainter` for the logo. Evaluate using `flutter_svg` if asset complexity grows.
