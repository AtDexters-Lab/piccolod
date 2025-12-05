import 'dart:js_interop';
import 'package:web/web.dart' as web;

Future<void> copyText(String text) async {
  // 1. Try Modern Clipboard API (Secure Contexts: HTTPS, localhost)
  // Note: Accessing navigator.clipboard might be restricted, but checking for null is usually safe.
  final clipboard = web.window.navigator.clipboard;
  if (clipboard != null) {
    try {
      await clipboard.writeText(text).toDart;
      return;
    } catch (e) {
      // Fall through to legacy method
    }
  }

  // 2. Legacy Fallback (Non-Secure Contexts like http://piccolo.local)
  final body = web.document.body;
  if (body == null) {
    throw Exception("Document body is null, cannot use legacy copy");
  }

  final textArea =
      web.document.createElement('textarea') as web.HTMLTextAreaElement;
  textArea.value = text;

  // Ensure it's not visible but part of the DOM
  textArea.style
    ..position = 'fixed'
    ..left = '-9999px'
    ..top = '0';

  body.appendChild(textArea);
  textArea.focus();
  textArea.select();

  try {
    final successful = web.document.execCommand('copy');
    if (!successful) {
      throw Exception("execCommand copy failed");
    }
  } finally {
    body.removeChild(textArea);
  }
}
