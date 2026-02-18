import 'dart:convert';
import 'dart:js_interop';
import 'package:web/web.dart' as web;

/// Downloads a text file using the browser's anchor tag download attribute.
void downloadTextFile(String content, String filename) {
  final bytes = utf8.encode(content);
  final blob = web.Blob([bytes.toJS].toJS);
  final url = web.URL.createObjectURL(blob);

  (web.document.createElement('a') as web.HTMLAnchorElement)
    ..href = url
    ..download = filename
    ..click();

  web.URL.revokeObjectURL(url);
}

/// Downloads a file from a URL using the browser's anchor tag.
/// The browser handles auth cookies automatically for same-origin URLs.
void downloadFromUrl(String url, String filename) {
  (web.document.createElement('a') as web.HTMLAnchorElement)
    ..href = url
    ..download = filename
    ..click();
}
