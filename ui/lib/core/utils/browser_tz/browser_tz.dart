// Conditional-import shim for the browser's resolved IANA timezone.
// Web → reads `Intl.DateTimeFormat().resolvedOptions().timeZone`.
// Non-web (analyzer / unit tests) → returns null (header is omitted).
export 'browser_tz_stub.dart'
    if (dart.library.js_interop) 'browser_tz_web.dart';
