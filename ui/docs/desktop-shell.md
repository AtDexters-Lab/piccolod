# Desktop Shell Architecture

## Overview
The Desktop Shell implements the "3-Layer Architecture" defined in the interaction model. It is an **Adaptive Shell** built specifically for mouse/keyboard interaction.

## The Layers

### Layer A: The Frame (Top Bar)
*   **Widget:** `TopBar`
*   **Height:** Fixed 48px.
*   **Content:** `PiccoloWordmark` (Left), Global Search (Left-Center), System Health & User Profile (Right).
*   **Behavior:** Persistent, always on top (z-index 0 in stack, visually top).

### Layer B: The Stage
*   **Widget:** `Stage`
*   **Visuals:** "Aurora" animated gradient.
    *   **Animation:** 10-second cycle, sweeping alignment change from top-left to right-middle for a "breathing" effect.
*   **Behavior:** The background workspace. Future home for widgets.

### Layer C: The Launcher (Dock)
*   **Widget:** `Dock`
*   **Visuals:** Floating glass pill at bottom center.
*   **Behavior:**
    *   **Launch/Toggle:** Opens app if closed. Toggles Minimize/Restore if open.
    *   **Indicators:**
        *   **Active:** Subtle background tint.
        *   **Open (Running):** Small dot indicator below the icon.

### Layer D: Windows (The Manager)
*   **Widget:** `DesktopShell` (Stack) + `WindowFrame`.
*   **Controller:** `DesktopController`.

#### Window Lifecycle & Logic
*   **Open:** Windows cascade (offset by 30px). Animation: Scale (0.9 -> 1.0) + Fade In.
*   **Close:** Animation: Scale (1.0 -> 0.9) + Fade Out -> Removal.
*   **Minimize:** Hides window from view (state preserved). Dock icon remains "Open".
*   **Maximize:** Expands to fill the "Safe Area" (Screen minus Top Bar and Dock area). Square corners applied. Double-tap header to toggle.

#### Constraints & Physics
*   **Dragging:**
    *   **Top:** Hard stop at Top Bar (48px).
    *   **Bottom/Sides:** "Soft stop". Keeps at least 30px-50px of the title bar visible so windows cannot be lost off-screen.
*   **Resizing:**
    *   **Handle:** Bottom-right corner (mouse cursor aware).
    *   **Min Size:** 300x200.
    *   **Max Size:** Clamped to screen bounds (cannot resize drag handle off-screen).

## Key Components
*   `WindowFrame`: The container wrapping app content. Handles the title bar (caption buttons with hover effects), border, shadow (`elev-3`), and resize gestures.
*   `DesktopWindow` (Model): Mutable state for position, size, status (min/max/closing), and content.