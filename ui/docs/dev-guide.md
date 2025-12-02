# Development Guide

## Architecture
The UI is a **Flutter Web** application served by the Go backend (`piccolod`).

- **Production:** The Flutter build (`build/web`) is embedded into the `piccolod` binary. API calls use relative paths (e.g. `/api/v1/...`).
- **Development:** The Flutter app runs on a separate dev server (`localhost:random`) and talks to a running `piccolod` instance (`localhost:8080`).

## Prerequisites
- Flutter SDK
- Go 1.21+

## Running Development Environment

1.  **Start the Backend:**
    ```bash
    # In the root of piccolod repo
    make run
    ```
    This starts `piccolod` on http://localhost:8080.

2.  **Start the Frontend:**
    Open a new terminal:
    ```bash
    cd ui
    flutter run -d chrome --dart-define=API_BASE_URL=http://localhost:8080
    ```
    *Note: The `--dart-define` is crucial. Without it, the app assumes it's in production (embedded) and uses relative paths, which won't work on the dev server.*

## Building for Production
The `Makefile` handles the build process.

```bash
make build
```
This will:
1.  Run `flutter build web --release` in `ui/`.
2.  Copy artifacts to `web/`.
3.  Build `piccolod` with `go build` (embedding `web/`).

## Troubleshooting
- **CORS Errors:** `piccolod` dev server should have CORS enabled for localhost. If you see CORS errors, check `gin_server.go`.
- **API Errors:** Check the browser console and `piccolod` logs.
