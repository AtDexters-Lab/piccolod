# Piccolo OS: Interaction Model
**Metaphor:** "The Digital Sanctuary."

## 1. The Layout Anatomy
* **Layer A (The Frame):** A persistent Top Bar for global context (Search, System Health, User).
* **Layer B (The Stage):** The desktop area. Contains Widgets (Storage, Memories) when idle.
* **Layer C (The Launcher):**
    * **The Dock:** A floating glass bar at the bottom for pinned apps (Files, Settings, Photos).
    * **The App Drawer:** A full-screen frosted overlay launching all installed containers.

## 2. Smart Defaults Strategy (Iconography)
We do not ask users to manually upload icons for Docker containers. We use a **Waterfall Resolution**:
1.  **Label:** Check `piccolo.ui.icon` label from the container.
2.  **Catalog:** Check internal map for known apps (e.g., Plex, Nextcloud).
3.  **Heuristic:** Try fetching `/favicon.ico`.
4.  **Fallback:** Generate a pastel tile with the app's initials.

## 3. Navigation Philosophy
* Apps open as **Full Screen Activities** (overlaying the Stage), not as new pages.
* Closing an app reveals the Stage exactly as it was left.
