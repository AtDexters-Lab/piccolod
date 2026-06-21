import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/error_reporter.dart';

/// Phases within the crypto setup operation (displayed by FinishingStep).
enum SetupPhase { encrypting, creatingAdmin, generatingKey }

/// Polls `/api/v1/crypto/status` until `setup_in_progress` becomes false, the
/// caller signals cancellation, or [maxIterations] elapses. Returns true on
/// completion, false on timeout or cancellation. Caller distinguishes by
/// re-checking [isCancelled] after the call.
///
/// Each iteration's individual error is swallowed: the lock contention this
/// loop polls through routinely surfaces transient 5xx, and bailing out on
/// the first error would defeat the loop's purpose.
Future<bool> pollSetupCompletion({
  required bool Function() isCancelled,
  Duration interval = const Duration(seconds: 3),
  int maxIterations = 60,
}) async {
  final api = ApiClient();
  for (var i = 0; i < maxIterations; i++) {
    await Future<void>.delayed(interval);
    if (isCancelled()) return false;
    try {
      final status =
          await api.get('/api/v1/crypto/status') as Map<String, dynamic>;
      if (status['setup_in_progress'] != true) return true;
    } on Object catch (_) {}
  }
  return false;
}

/// Server-emitted error codes consumed by the UI. Mirror of Go consts in
/// internal/server/gin_crypto_handlers.go (ErrRecoveryKeyAlreadyAcked etc.)
/// — keep both sides in sync; a typo on either fails open silently.
class ServerErrorCode {
  static const recoveryKeyAlreadyAcked = 'recovery_key_already_acked';
  static const ackStateUnknown = 'ack_state_unknown';
  static const passkeyAlreadyRegistered = 'passkey_already_registered';
  static const staleRecoveryKeyID = 'stale_recovery_key_id';
}

/// Recovery-key generate response — words alongside their version anchor.
/// REC-4: keyId binds /generate to /ack so a multi-tab rotation race or a
/// delayed fire-and-forget ack from a stale tab cannot mark a *rotated*
/// key as acked. Callers MUST propagate keyId verbatim into ackRecoveryKey.
/// REC-3: wasRotated=true when this generation replaced an unack'd previous
/// key — the UI raises a banner so the operator notices the words changed.
class RecoveryKeyMaterial {
  const RecoveryKeyMaterial({
    required this.words,
    required this.keyId,
    this.wasRotated = false,
  });
  final List<String> words;
  final String keyId;
  final bool wasRotated;
}

/// Why the recovery-key screen is being shown right now. The screen surfaces
/// from 5 call sites across setup_router/auth_controller/first_run_controller
/// but collapses to 3 contexts: post-passkey-login intentionally shares
/// postLogin (same operator mental state — "I just signed in, now I'm here"),
/// and partial-setup-after-unlock shares postSetup (operator is finishing
/// initial setup, regardless of unlock-screen entry).
enum RecoveryKeyEntryContext {
  postSetup,
  postLogin,
  postPasskeyRegister,
}

/// Matches an ApiException by both HTTP status and structured error string.
/// Centralizes the two-clause `e.statusCode == X && extractServerError(...) == Y`
/// pattern that previously duplicated across call sites — a typo on either
/// clause used to fail-open silently (rethrow path), now compile-checked.
bool isApiError(ApiException e, int status, String code) {
  return e.statusCode == status && extractServerError(e.rawBody) == code;
}

/// Records that the operator has seen and saved the current recovery-key words.
/// Without this, recovery_key_pending stays true on next boot only by file
/// presence — but a UI timeout between the server writing keyset.json and the
/// words reaching the browser leaves a "ghost" recovery key the user never saw.
///
/// Retries a few times with backoff before giving up: a transient ack-write
/// failure → next boot returns recovery_key_pending=true → auth controller
/// calls /recovery-key/generate on unlock → ROTATES the freshly-saved key,
/// silently invalidating the words the operator just wrote down. The retry
/// loop closes that window for plausible blip durations (~2.5 seconds total).
/// Persistent failure beyond the retry budget logs and gives up; the operator
/// will be re-prompted on next reload, which is the correct fail-closed
/// posture even though it costs them an extra recovery-key save.
///
/// REC-4: keyId binds the acknowledgement to the specific recovery-key
/// version the operator was shown (returned by /recovery-key/generate). The
/// server rejects with 409 stale_recovery_key_id when the supplied id
/// doesn't match the current SDEKRK fingerprint — closes the multi-tab
/// rotation race + the silent-ack-failure resurrection class. An empty
/// keyId is rejected by the server (400 key_id required); callers must
/// propagate the value from /generate verbatim.
Future<void> ackRecoveryKey(String keyId) async {
  if (keyId.isEmpty) {
    // Caller bug — keyId was never populated (controller path displayed
    // words without going through _generateRecoveryKey, or the material
    // was discarded between capture and ack). The server would 400 on
    // every retry; short-circuit to surface the bug in telemetry instead
    // of burning the retry budget on a deterministic failure.
    ErrorReporter().report(
      type: 'recovery_key_ack_missing_key_id',
      message: 'ackRecoveryKey called with empty keyId — caller plumbing bug',
      flushImmediate: true,
    );
    return;
  }
  const attempts = 3;
  const backoff = [Duration(milliseconds: 500), Duration(seconds: 2)];
  for (var i = 0; i < attempts; i++) {
    try {
      await ApiClient().post(
        '/api/v1/crypto/recovery-key/ack',
        body: <String, dynamic>{'key_id': keyId},
      );
      return;
    } on ApiException catch (e) {
      // 409 stale_recovery_key_id is deterministic — another tab rotated
      // the key between /generate and /ack, or the keyset on disk does
      // not match the words this tab is showing. Retrying loses the retry
      // budget against an answer that will not change. Short-circuit to
      // telemetry; operator is re-prompted on next reload with current
      // words. Distinct UX (a banner explaining the rotation rather than
      // a silent re-prompt) is REC-3 territory; not in cluster 8 scope.
      if (isApiError(e, 409, ServerErrorCode.staleRecoveryKeyID)) {
        ErrorReporter().report(
          type: 'recovery_key_ack_stale',
          message: 'ack rejected — key rotated between generate and ack',
          flushImmediate: true,
        );
        return;
      }
      if (i == attempts - 1) {
        debugPrint(
          'Recovery-key ack failed after $attempts attempts '
          '(will re-prompt on reload): $e',
        );
        return;
      }
      await Future<void>.delayed(backoff[i]);
    } on Object catch (e) {
      if (i == attempts - 1) {
        debugPrint(
          'Recovery-key ack failed after $attempts attempts '
          '(will re-prompt on reload): $e',
        );
        return;
      }
      await Future<void>.delayed(backoff[i]);
    }
  }
}

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
  final code = extractErrorCode(e.rawBody);
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
  if (e.statusCode == 409 &&
      serverMsg == ServerErrorCode.passkeyAlreadyRegistered) {
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
  if (msg.contains('TimeoutError') ||
      msg.contains('not found') ||
      msg.contains('expired')) {
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
