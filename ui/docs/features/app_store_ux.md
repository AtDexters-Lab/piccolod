# App Store & Management UX Design

## 1. Philosophy: "The Soul of Piccolo"
The App Store is the bridge between the user's desire for utility and the system's guarantee of stability. It must feel like a **curated boutique**, not a raw package manager.

*   **Calm Control:** Installing an app is a commitment. We slow down the "Install" click just enough to show the user what they are agreeing to (Permissions, Storage), but not enough to be annoying.
*   **Android-like Simplicity:** Complex container orchestration is hidden behind friendly toggles and clear status indicators.
*   **Safety First:** "Custom" apps are treated with slightly more friction (validation steps) than "Curated" apps.

## 2. Information Architecture

The "Apps" feature is a top-level destination in the Desktop Shell (Left Rail).

### Views
1.  **Library (Default):** Grid of installed applications.
2.  **Store (Tab):** Discovery surface for new apps (Community & Curated).
3.  **App Detail (Route):** The single source of truth for a specific app (whether installed or in-store).
4.  **Custom Install (Modal/Wizard):** The power-user flow for `app.yaml`.

## 3. User Flows & Wireframes

### A. The "Store" Tab
*   **Layout:**
    *   **Hero Section:** "Featured App" (Banner style with Icon, Title, One-line blurb).
    *   **Search Bar:** "Search catalog..." (Global search spanning both **Installed** and **Store** content).
    *   **Grid:** Cards showing App Icon (Squircle), Name, and brief description.
    *   **FAB / Action Button:** "Install Custom App" (Top right or floating).

### B. App Detail View (The "Manifesto")
This view adapts based on the app's state.

#### State: Not Installed (Catalog Item)
*   **Header:** Large Icon, Name, Developer/Maintainer (if available).
*   **Body:** Markdown description (Features, Screenshots).
*   **Sidebar/Right Panel:**
    *   **Permissions:** "Needs access to: Internet, Local Network".
    *   **Storage:** "Est. Size: ~200MB".
    *   **Preflight Checks:** (Implicit) System checks for port availability and disk space before "Install" becomes active.
    *   **Primary Action:** "Install" Button.
    *   **Secondary Action:** "Advanced Install" (Edit YAML before install).

#### State: Installed
*   **Header:** Status Badge (Running/Stopped), Uptime, Version.
    *   *Visual:* **Progress Ring** around the icon during Installing/Updating states.
*   **Primary Actions:** Open (Dropdown for endpoints), Stop/Start, Restart.
*   **Tabs:**
    *   **Overview:** CPU/RAM usage graphs, Volume usage, SSO Status.
    *   **Configuration:** Read-only view of `app.yaml` or simplified "Settings" (Env vars).
    *   **Network:** List of exposed ports and URLs (Local/Remote).
    *   **Logs:** Live tail of container logs.
    *   **Versions:** Update to new tag / **Revert** to previous tag.
    *   **Snapshots:** (Future) Backup/Restore controls.

#### Uninstall Flow (Critical)
*   **Action:** Click "Uninstall" (Kebab menu or Danger Zone).
*   **Modal:** "Remove [App Name]?"
*   **Choice:**
    *   "**Keep Data**" (Default): Removes container but keeps volumes.
    *   "**Delete Data**": Destructive action, requires explicit confirmation.

### C. The "Custom Install" Wizard
A modal dialog for the power user.

*   **Step 1: Definition**
    *   Tabs: "Upload File", "Paste YAML", "URL".
    *   Code Editor with syntax highlighting for YAML (`JetBrainsMono`).
*   **Step 2: Validation**
    *   Backend call to `/apps/validate`.
    *   Visual feedback: "Valid Manifest" (Green check) or detailed error parsing.
*   **Step 3: Review (The "Contract")**
    *   **Permissions Card:** "This app requests: Rootless execution, Internet Access".
    *   **Storage Card:** "Will create volume `piccolo-data` (Limit: 10GB)".
    *   **Network Card:** "Will expose ports: 8080 (Web)".
    *   **Resource Check:** "Ports 8080 available. Disk space adequate."
*   **Step 4: Install**
    *   Progress indicator (Pulling image -> Creating Container -> Starting).

## 4. Visual Language (Cobalt Neutral)

*   **Icons:** Squircle mask (12px radius on 48px/64px icons).
*   **Badges:**
    *   Running: Cobalt (Blue) or Success (Green) dot.
    *   Stopped: Neutral (Grey) ring.
    *   Error: Critical (Red) with "Fix Now" link.
    *   *Progress:* Animated ring during install/update.
*   **Cards:** Elevation-1 on hover.

## 5. Technical Integration

### API Requirements
*   **Catalog:** Need to mock `GET /catalog` with rich data until backend catches up.
*   **Install:** `POST /apps` accepts raw YAML.
*   **Lifecycle:** `POST /apps/{name}/{action}` handles the state changes.
*   **Logs:** `GET /apps/{name}/logs` needs to be polled or streamed.

### State Management
*   `AppStoreController`: Fetches catalog, handles global search.
*   `AppLifecycleController`: Polls status of installed apps, handles Start/Stop/Uninstall actions.

## 6. Implementation Plan (Backlog)

1.  **Scaffold Views:** Create `AppStoreWindow` with the "Library" (Installed) and "Store" (Catalog) tabs.
2.  **Mock Data:** Create a `MockAppService` to return rich catalog data for development.
3.  **YAML Editor:** Integrate a lightweight code editor widget for the Custom Install flow.
4.  **Wiring:** Connect `AppDetailView` to the real `ApiClient` for installed apps.