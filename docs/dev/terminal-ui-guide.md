# Terminal Access UI Implementation Guide

This document serves as the comprehensive reference for the frontend team implementing the Piccolo OS internal debugging terminal.

## 1. Overview

The Terminal feature allows authenticated administrators to access a secure, containerized shell (`/bin/bash`) on the host system directly from the browser. This is critical for low-level debugging, log inspection, and emergency maintenance.

**Security Constraints:**
*   **Local Access Only:** The backend rigorously enforces that this endpoint is only accessible to clients on the local network (LAN) or the secure internal loopback. Access via the Nexus remote tunnel is **blocked** by the server.
*   **Admin Session:** Requires a valid, authenticated admin session.

**Base Endpoint:** `GET /api/v1/terminal` (WebSocket Upgrade)

---

## 2. Protocol Specification

The WebSocket uses a strict JSON-based sub-protocol. All payloads (input and output) are Base64-encoded to safely transport binary data and control characters without JSON escaping issues.

### 2.1 Connection Lifecycle
1.  **Connect:** Client opens `ws://<host>/api/v1/terminal`.
2.  **Authenticate:** Handled automatically via the browser's session cookie.
3.  **Initialize:** Server spawns a PTY (Pseudo-Terminal) running `/bin/bash` with `TERM=xterm-256color`.
4.  **Exchange:** Bi-directional JSON message stream.
5.  **Teardown:**
    *   If the client disconnects, the shell process is killed.
    *   If the shell process exits (e.g., user types `exit`), the WebSocket is closed by the server.

### 2.2 Message Format: Client → Server

The client sends standard JSON messages to send input or control the terminal window.

#### A. Input (Keystrokes/Paste)
Sends raw user input to the shell's standard input.

```json
{
  "type": "stdin",
  "data": "<base64_encoded_string>"
}
```
*   `type`: Must be `"stdin"`.
*   `data`: The raw input string (e.g., "ls -la\n") encoded as Base64.

#### B. Resize (Window Geometry)
Informs the backend that the terminal viewport size has changed. This is critical for correct text wrapping and full-screen applications (like `vim` or `htop`).

```json
{
  "type": "resize",
  "cols": 80,
  "rows": 24
}
```
*   `type`: Must be `"resize"`.
*   `cols`: Number of columns (integer).
*   `rows`: Number of rows (integer).
*   **Best Practice:** Send a `resize` message immediately upon connection open, and subsequently whenever the `xterm.js` `onResize` event fires.

### 2.3 Message Format: Server → Client

The server sends output from the shell's standard output/error.

#### A. Output (Stdout/Stderr)
Raw terminal output to be rendered by the frontend.

```json
{
  "type": "stdout",
  "data": "<base64_encoded_string>"
}
```
*   `type`: `"stdout"`.
*   `data`: The raw output (including ANSI color codes) encoded as Base64.

---

## 3. Implementation Example (JavaScript/xterm.js)

```javascript
import { Terminal } from 'xterm';
import { FitAddon } from 'xterm-addon-fit';

const term = new Terminal();
const fitAddon = new FitAddon();
term.loadAddon(fitAddon);

// 1. Establish Connection
// Note: Use wss:// if on HTTPS
const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
const ws = new WebSocket(`${protocol}//${location.host}/api/v1/terminal`);

ws.onopen = () => {
  // 2. Initial Resize
  // Synchronize backend PTY size with frontend render area
  const dims = fitAddon.proposeDimensions();
  if (dims) {
    ws.send(JSON.stringify({
      type: "resize",
      cols: dims.cols,
      rows: dims.rows
    }));
    fitAddon.fit();
  }
};

// 3. Handle Incoming Data (Server -> Client)
ws.onmessage = (event) => {
  try {
    const msg = JSON.parse(event.data);
    if (msg.type === 'stdout' && msg.data) {
      // Decode Base64 and write to xterm
      const raw = atob(msg.data);
      term.write(raw);
    }
  } catch (e) {
    console.error("Failed to parse terminal message", e);
  }
};

// 4. Handle User Input (Client -> Server)
term.onData((data) => {
  if (ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({
      type: "stdin",
      data: btoa(data) // Encode to Base64
    }));
  }
});

// 5. Handle Resize Events
window.addEventListener('resize', () => {
  fitAddon.fit();
  const dims = fitAddon.proposeDimensions();
  if (dims && ws.readyState === WebSocket.OPEN) {
     ws.send(JSON.stringify({
      type: "resize",
      cols: dims.cols,
      rows: dims.rows
    }));
  }
});
```

---

## 4. Error Handling & Edge Cases

### Connection Rejected (403 Forbidden)
If the connection closes immediately with a 403 error, it is likely due to the **Local Access Policy**.
*   **Cause:** The user is accessing the dashboard via the remote tunnel (Nexus).
*   **UI Guidance:** Display a clear error message: "Terminal access is restricted to local network connections only. Please access the dashboard via the device's local IP address."

### Connection Closed
If the socket closes cleanly:
*   **Cause:** User typed `exit` or the shell process terminated.
*   **UI Guidance:** Show a "Connection Closed" overlay with a "Reconnect" button.

### Encoding Errors
If `atob()` or `btoa()` fails, ensure that you are handling UTF-8 strings correctly before encoding. Standard `btoa` in JS handles Latin1; for full UTF-8 support (emojis, specialized shell characters), consider a robust utility:

```javascript
// Robust Base64 Encode
function utf8_to_b64(str) {
  return window.btoa(unescape(encodeURIComponent(str)));
}

// Robust Base64 Decode
function b64_to_utf8(str) {
  return decodeURIComponent(escape(window.atob(str)));
}
```

