import 'dart:js_interop';

/// Reads the browser's resolved IANA timezone via the `piccoloBrowserTimezone`
/// global (defined in web/index.html, which wraps
/// `Intl.DateTimeFormat().resolvedOptions().timeZone`). The backend's
/// X-Browser-Timezone middleware uses this hint to seed the device's
/// timezone the first time the operator opens the portal, before they
/// reach the Settings → System screen.
///
/// Cached on first call so repeated HTTP requests don't re-cross the JS
/// boundary. Returns null on any failure — the header is omitted, no harm.
@JS('piccoloBrowserTimezone')
external JSString _jsPiccoloBrowserTimezone();

String? _cached;
bool _resolved = false;

String? resolvedBrowserTimezone() {
  if (_resolved) return _cached;
  _resolved = true;
  try {
    final zone = _jsPiccoloBrowserTimezone().toDart;
    if (zone.isNotEmpty) _cached = zone;
  } on Object catch (_) {
    // piccoloBrowserTimezone may be missing on legacy index.html builds.
  }
  return _cached;
}
