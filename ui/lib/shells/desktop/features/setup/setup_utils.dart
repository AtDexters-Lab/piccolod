import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';

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
  return 'Server error (${e.statusCode}). Please try again.';
}

/// Format a WebAuthn/platform error into a user-friendly message.
String friendlyPasskeyError(Object e) {
  final msg = e.toString();
  if (msg.contains('InvalidStateError') || msg.contains('already registered')) {
    return 'This authenticator already has a passkey registered. Try a different authenticator or use your existing passkey to sign in.';
  }
  if (msg.contains('NotAllowedError') || msg.contains('cancelled')) {
    return 'Passkey operation was cancelled or timed out. If using a phone, ensure Bluetooth is on and devices are nearby.';
  }
  if (msg.contains('NotSupportedError')) {
    return 'Passkeys are not supported in this browser. Try Chrome, Safari, or Edge.';
  }
  if (msg.contains('not found') || msg.contains('expired')) {
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
