import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/error_reporter.dart';

/// Phases within the crypto setup operation (displayed by FinishingStep).
enum SetupPhase { encrypting, creatingAdmin, generatingKey }

/// Extract a structured error code from a JSON error response body.
String? extractErrorCode(String body) {
  try {
    final decoded = jsonDecode(body);
    if (decoded is Map && decoded['code'] is String) {
      return decoded['code'] as String;
    }
  } on Object catch (_) {}
  return null;
}

/// Extract a human-readable error from a JSON error response body.
/// Prefers "message" (more descriptive, used by emergency middleware),
/// falls back to "error" (LUKS handlers), then to the raw body.
String extractServerError(String body) {
  try {
    final decoded = jsonDecode(body);
    if (decoded is Map) {
      if (decoded['message'] is String) return decoded['message'] as String;
      if (decoded['error'] is String) return decoded['error'] as String;
    }
  } on Object catch (_) {}
  if (body.length > 200 || body.contains('<html>')) {
    return 'An unexpected error occurred. Please try again.';
  }
  return body;
}

/// Checks if an API exception represents a storage system error.
/// Covers LUKS data volume failures (500) and emergency middleware blocks (503).
bool isStorageSystemError(ApiException e) {
  final code = extractErrorCode(e.message);
  if (code == 'storage_init_failed' ||
      code == 'storage_unlock_failed' ||
      code == 'storage_emergency') {
    return true;
  }
  if (e.statusCode == 500 &&
      (e.message.contains('data volume initialization failed:') ||
          e.message.contains('data volume unlock failed:'))) {
    return true;
  }
  if (e.statusCode == 503 && e.message.contains('storage emergency')) {
    return true;
  }
  return false;
}

/// Format an ApiException into a user-friendly passkey/server error.
String friendlyApiError(ApiException e) {
  final serverMsg = extractServerError(e.message);
  if (serverMsg.contains('ceremony expired') ||
      serverMsg == 'ceremony expired or not found') {
    return 'Session expired. Please try again.';
  }
  if (e.statusCode == 401) {
    return 'Passkey sign-in failed. Please try again or sign in with your password.';
  }
  // Benign duplicate on /register/finish — the authenticator already has a
  // credential for this account and we correctly refused to insert a second
  // row. Surface a non-destructive message.
  if (e.statusCode == 409 && serverMsg == 'passkey_already_registered') {
    return 'This device already has a passkey for this account. Try signing '
        'in with your existing passkey, or use a different device.';
  }
  return 'Server error (${e.statusCode}). Please try again.';
}

/// Format a WebAuthn/platform error into a user-friendly message.
///
/// The JS bridge (`ui/web/webauthn.js`) normalizes thrown browser errors to
/// `"<Name>: <message>"` before re-throwing, so `.toString()` here reliably
/// carries the DOMException name as a substring.
///
/// Also reports the raw error via telemetry so operators can diagnose — the
/// user-facing copy is intentionally lossy. Without this, caught-and-
/// presented errors never leave the browser.
String friendlyPasskeyError(Object e) {
  final msg = e.toString();
  String? stack;
  if (e is Error) stack = e.stackTrace?.toString();
  ErrorReporter().report(
    type: 'passkey_error',
    message: msg.length > 512 ? msg.substring(0, 512) : msg,
    stack: stack,
    flushImmediate: true,
  );
  if (msg.contains('InvalidStateError') || msg.contains('already registered')) {
    return 'This device already has a passkey registered. Try signing in with it, or use a different device to add another.';
  }
  if (msg.contains('NotAllowedError') || msg.contains('cancelled')) {
    // Chrome on Android also returns NotAllowedError when excludeCredentials
    // matches an existing credential (privacy-preserving). The three paths
    // are indistinguishable at this layer; keep the copy action-first.
    return "Passkey setup didn't complete. If you cancelled, try again. "
        'If this device already has a passkey for this account, sign in with it instead. '
        'On a phone, make sure Bluetooth is on.';
  }
  if (msg.contains('SecurityError')) {
    return "Passkey setup failed a security check. Make sure you're using the "
        'correct address and try again.';
  }
  if (msg.contains('ConstraintError')) {
    return "This device can't create the kind of passkey Piccolo needs. Try a "
        'different device (phone, laptop, or a hardware security key).';
  }
  if (msg.contains('NotReadableError') || msg.contains('credential manager')) {
    // Android Credential Manager / Google Password Manager refused the
    // request. Usually means the phone has no screen lock, or Play Services /
    // Credential Manager is disabled / out of date.
    return "Your phone's passkey system refused the request. Make sure "
        'you have a screen lock set (PIN, pattern, or biometric), Google '
        'Play Services is up to date, and then try again.';
  }
  if (msg.contains('NotSupportedError')) {
    return 'Passkeys are not supported in this browser. Try Chrome, Safari, or Edge.';
  }
  if (msg.contains('TimeoutError') || msg.contains('not found') || msg.contains('expired')) {
    return 'Session expired. Please try again.';
  }
  debugPrint('Unexpected passkey error: $msg');
  return 'An unexpected error occurred. Please try again.';
}

/// Detect if we're on the remote domain (HTTPS, not .local, not localhost, not an IP).
/// Used to determine if the setup wizard is running after a redirect from the LAN
/// to the Namek-assigned public domain.
bool isOnRemoteDomain() {
  try {
    final host = Uri.base.host;
    if (Uri.base.scheme != 'https') return false;
    if (host.endsWith('.local') || host.startsWith('localhost')) return false;
    // Exclude raw IP addresses (LAN HTTPS via IP is not a remote domain).
    if ((Uri.tryParse('http://$host')?.hasAbsolutePath ?? false) &&
        RegExp(r'^[\d.:[\]]+$').hasMatch(host)) {
      return false;
    }
    return true;
  } on Object catch (_) {
    return false;
  }
}
