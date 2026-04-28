import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/webauthn_service.dart';
import 'package:piccolo_os/shells/desktop/features/setup/setup_utils.dart';
import 'package:web/web.dart' as web;

enum AuthMode { unlock, login, passkeyRequired }
enum AuthStep { main, forgotPassword, finishing, recoveryKey }

class AuthController extends ChangeNotifier {
  AuthController({
    required this.mode,
    required this.onComplete,
    required this.reRoute,
    required this.onSystemError,
    required this.generateRecoveryKey,
    this.recoveryKeyPending = false,
    this.authRequestId,
    this.nextUrl,
  }) {
    if (mode == AuthMode.login || mode == AuthMode.passkeyRequired) {
      unawaited(fetchLoginOptions());
    }
  }

  final AuthMode mode;
  final VoidCallback onComplete;
  final VoidCallback reRoute;
  final void Function(String error) onSystemError;
  bool recoveryKeyPending;
  final String? authRequestId;
  final String? nextUrl;
  /// Router-owned guard: returns words on success, null on skip/timeout.
  final Future<List<String>?> Function() generateRecoveryKey;

  final ApiClient _api = ApiClient();
  bool _disposed = false;

  AuthStep _step = AuthStep.main;
  AuthStep get step => _step;

  String? _error;
  String? get error => _error;

  String? _passkeyError;
  String? get passkeyError => _passkeyError;

  SetupPhase? _setupPhase;
  SetupPhase? get setupPhase => _setupPhase;

  List<String>? _loginMethods;
  List<String>? get loginMethods => _loginMethods;

  List<String> _recoveryWords = [];
  List<String> get recoveryWords => _recoveryWords;

  @override
  void dispose() {
    _disposed = true;
    super.dispose();
  }

  // --- Login options ---

  Future<void> fetchLoginOptions() async {
    for (var attempt = 0; attempt < 3; attempt++) {
      try {
        final result = await _api
            .getLoginOptions()
            .timeout(const Duration(seconds: 5));
        if (_disposed) return;
        _loginMethods = List<String>.from(result['methods'] as List);
        notifyListeners();
        return;
      } on Object catch (_) {
        if (attempt < 2) {
          await Future<void>.delayed(Duration(seconds: 1 << attempt));
        }
      }
    }
    if (_disposed) return;
    _loginMethods = ['password'];
    notifyListeners();
  }

  // --- Unlock ---

  Future<bool> unlock(String password) async {
    try {
      debugPrint('Unlock attempt');
      final unlockResp = await _api.post(
        '/api/v1/crypto/unlock',
        body: {'password': password},
      ) as Map<String, dynamic>?;

      if (unlockResp?['warning'] != null) {
        onSystemError(unlockResp!['warning'] as String);
        return true;
      }

      if (unlockResp?['setup_complete'] == false) {
        await _completeSetupAfterUnlock(password);
      } else {
        await _chainLoginAfterUnlock(password);
      }
      if (!_disposed) notifyListeners();
      return true;
    } on ApiException catch (e) {
      _error = e.toString();
      if (!_disposed) notifyListeners();
      return false;
    } on Object catch (e) {
      debugPrint('Unexpected in unlock: ${e.runtimeType}: $e');
      _error = e.toString();
      if (!_disposed) notifyListeners();
      return false;
    }
  }

  /// Partial-setup-after-unlock: handled internally (no controller switch).
  Future<void> _completeSetupAfterUnlock(String password) async {
    debugPrint('Partial setup detected after unlock, completing via /crypto/setup');
    _step = AuthStep.finishing;
    _setupPhase = SetupPhase.encrypting;
    notifyListeners();
    try {
      await _api.post('/api/v1/crypto/setup', body: {'password': password});
    } on ApiException catch (e) {
      final code = extractErrorCode(e.message);
      if (e.statusCode == 409 && code == 'setup_in_progress') {
        await _waitForSetupCompletion();
        return;
      } else if (e.statusCode == 409 && code == 'setup_complete') {
        // Already done — fall through to recovery key.
      } else {
        rethrow;
      }
    }
    try {
      await _api.fetchCsrfToken();
      final words = await generateRecoveryKey();
      if (words != null) {
        _recoveryWords = words;
        _setupPhase = null;
        _step = AuthStep.recoveryKey;
        return;
      }
      _setupPhase = null;
      reRoute();
    } on Object catch (e) {
      debugPrint('Recovery key after partial setup failed: $e');
      _setupPhase = null;
      reRoute();
    }
  }

  Future<void> _chainLoginAfterUnlock(String password) async {
    try {
      await _completeLoginFlow('admin', password);
    } on ApiException catch (e) {
      if (e.statusCode == 423) {
        _error = 'Unlock partially failed, please try again';
        return;
      }
      debugPrint('Chained login after unlock failed (${e.statusCode}), '
          'falling back via reRoute');
      reRoute();
    } on Object catch (e) {
      debugPrint('Chained login after unlock failed: $e, '
          'falling back via reRoute');
      reRoute();
    }
  }

  /// Poll /crypto/status until setup handler releases mutex.
  Future<void> _waitForSetupCompletion() async {
    _error = null;
    if (_step != AuthStep.finishing) {
      _step = AuthStep.finishing;
      _setupPhase = SetupPhase.encrypting;
      notifyListeners();
    }
    for (var i = 0; i < 60; i++) {
      await Future<void>.delayed(const Duration(seconds: 3));
      if (_disposed) return;
      try {
        final status = await _api.get('/api/v1/crypto/status')
            as Map<String, dynamic>;
        if (status['setup_in_progress'] != true) {
          debugPrint('Setup no longer in progress, recovering via reRoute');
          reRoute();
          return;
        }
      } on Object catch (_) {}
    }
    if (_disposed) return;
    _setupPhase = null;
    reRoute();
  }

  // --- Login ---

  Future<bool> login(String username, String password) async {
    _passkeyError = null;
    try {
      await _completeLoginFlow(username, password);
      if (!_disposed) notifyListeners();
      return true;
    } on ApiException catch (e) {
      debugPrint('Login failed: $e');
      _error = extractServerError(e.message);
      if (!_disposed) notifyListeners();
      return false;
    } on Object catch (e) {
      debugPrint('Unexpected in login: ${e.runtimeType}: $e');
      final msg = e.toString();
      _error = msg.length > 200 ? 'Login failed. Please try again.' : msg;
      if (!_disposed) notifyListeners();
      return false;
    }
  }

  /// Shared post-login flow: authenticate, handle passkey, recovery key,
  /// redirects, and set final state.
  Future<void> _completeLoginFlow(String username, String password) async {
    final resp = await _api.post(
      '/api/v1/auth/login',
      body: {'username': username, 'password': password, 'next': nextUrl},
    );
    await _api.fetchCsrfToken();

    // First-run: generate recovery key before any routing. Server is
    // authoritative when present; constructor fallback only for older builds.
    if (_resolvePending(resp is Map ? resp.cast<String, dynamic>() : null)) {
      final words = await generateRecoveryKey();
      if (words != null) {
        _recoveryWords = words;
        _step = AuthStep.recoveryKey;
        return;
      }
    }

    if (resp is Map && resp['must_register_passkey'] == true) {
      reRoute();
      return;
    }

    if (await _completeAuthAndRedirect(resp)) return;

    onComplete();
  }

  // --- Passkey login ---

  Future<bool> loginWithPasskey() async {
    _error = null;
    _passkeyError = null;
    notifyListeners();
    try {
      final beginResult = await _api.beginPasskeyLogin();
      final sessionId = beginResult['session_id'] as String;
      final options = beginResult['publicKey'] as Map<String, dynamic>;

      final credential = await WebAuthnService.getCredential(options);

      final finishResp = await _api.finishPasskeyLogin(sessionId, credential);
      await _api.fetchCsrfToken();

      // Locked-then-unlock-via-passkey didn't go through /system/boot since
      // login, so the constructor-time recoveryKeyPending may be stale —
      // _resolvePending centralizes the server-authoritative-with-fallback
      // rule.
      if (_resolvePending(finishResp)) {
        final words = await generateRecoveryKey();
        if (words != null) {
          _recoveryWords = words;
          _step = AuthStep.recoveryKey;
          if (!_disposed) notifyListeners();
          return true;
        }
      }

      final session = await _api.get('/api/v1/auth/session') as Map<String, dynamic>;
      if (session['must_register_passkey'] == true) {
        reRoute();
        return true;
      }

      if (await _completeAuthAndRedirect(null)) return true;

      onComplete();
      return true;
    } on ApiException catch (e) {
      WebAuthnService.maybeSignalFromApiError(e);
      _passkeyError = friendlyApiError(e);
      if (!_disposed) notifyListeners();
      return false;
    } on Object catch (e) {
      _passkeyError = friendlyPasskeyError(e);
      if (!_disposed) notifyListeners();
      return false;
    }
  }

  // --- Passkey registration ---

  Future<bool> registerPasskey() async {
    _error = null;
    _passkeyError = null;
    notifyListeners();
    try {
      final beginResult = await _api.beginPasskeyRegistration();
      final sessionId = beginResult['session_id'] as String;
      final options = beginResult['publicKey'] as Map<String, dynamic>;

      final credential = await WebAuthnService.createCredential(options);

      final finishResp = await _api.finishPasskeyRegistration(sessionId, credential);

      // The MustRegisterPasskey forcing path lands here and used to skip
      // the recovery-key gate entirely. If the server says the key is
      // unack'd, route through the recovery-key step before completing.
      // generateRecoveryKey() returns null on transient 409/503 (handled
      // at router level) so a server-side false-positive on the gate or a
      // staleness-read failure won't misattribute as a passkey error.
      if (finishResp['recovery_key_pending'] == true) {
        final words = await generateRecoveryKey();
        if (words != null) {
          _recoveryWords = words;
          _step = AuthStep.recoveryKey;
          if (!_disposed) notifyListeners();
          return true;
        }
      }

      if (await _completeAuthAndRedirect(null)) return true;

      onComplete();
      return true;
    } on ApiException catch (e) {
      // Benign-duplicate during the bootstrap PasskeyRequired flow: server
      // cleared the forcing-gate server-side (the user demonstrably owns a
      // passkey for this RP), so treat 409 as a soft success and transition
      // forward. Without this the user stays on PasskeyRequiredStep with an
      // error message despite the gate being cleared.
      if (isApiError(e, 409, ServerErrorCode.passkeyAlreadyRegistered)) {
        if (await _completeAuthAndRedirect(null)) return true;
        onComplete();
        return true;
      }
      _passkeyError = friendlyApiError(e);
      if (!_disposed) notifyListeners();
      return false;
    } on Object catch (e) {
      _passkeyError = friendlyPasskeyError(e);
      if (!_disposed) notifyListeners();
      return false;
    }
  }

  // --- Forgot password ---

  void startRecovery() {
    _step = AuthStep.forgotPassword;
    _error = null;
    _passkeyError = null;
    notifyListeners();
  }

  void cancelRecovery() {
    reRoute();
  }

  Future<bool> resetPassword(String recoveryKey, String newPassword) async {
    try {
      await _api.post(
        '/api/v1/crypto/reset-password',
        body: {'recovery_key': recoveryKey, 'new_password': newPassword},
      );
      _step = AuthStep.main;
      _error = null;
      if (!_disposed) notifyListeners();
      return true;
    } on Object catch (e) {
      _error = e.toString();
      if (!_disposed) notifyListeners();
      return false;
    }
  }

  /// Server-authoritative-with-fallback rule for the recovery-key gate. When
  /// the server includes recovery_key_pending in its response, that is the
  /// definitive answer (the server re-evaluates the gate at login time, so
  /// it sees post-unlock state the constructor-time boot snapshot misses).
  /// The recoveryKeyPending fallback handles the "older server" case only.
  bool _resolvePending(Map<String, dynamic>? resp) {
    if (resp != null && resp.containsKey('recovery_key_pending')) {
      return resp['recovery_key_pending'] == true;
    }
    return recoveryKeyPending;
  }

  // --- Recovery key proceed ---

  void proceedAfterRecovery() {
    // Await the ack before rerouting — reRoute → _checkBoot → _routeDesktop
    // can land on _completeDesktopRedirectOrFinish, which assigns
    // window.location.href and aborts in-flight requests on the page. A
    // fire-and-forget ack would race the navigation and may never reach
    // the server, leaving RecoveryAckAt=zero and the operator re-prompted
    // with rotated words on next sign-in.
    unawaited(() async {
      await ackRecoveryKey();
      if (_disposed) return;
      reRoute();
    }());
  }

  // --- Redirect helpers ---

  /// Centralized redirect handling for OIDC and ?next= flows.
  /// Returns true if a redirect was performed.
  Future<bool> _completeAuthAndRedirect(Object? loginResp) async {
    if (authRequestId != null) {
      debugPrint('OIDC flow detected, completing auth request...');
      await _completeOidcAuthRequest();
      return true;
    }

    if (loginResp is Map && loginResp['redirect_url'] is String) {
      final redirectUrl = loginResp['redirect_url'] as String;
      if (redirectUrl.isNotEmpty) {
        debugPrint('Redirecting to login next URL: $redirectUrl');
        web.window.location.href = redirectUrl;
        return true;
      }
    }

    if (nextUrl != null && nextUrl!.isNotEmpty) {
      if (await _redirectToNextIfValid()) return true;
    }

    return false;
  }

  Future<void> _completeOidcAuthRequest() async {
    try {
      debugPrint('Completing OIDC auth request: $authRequestId');
      final response = await _api.post(
        '/api/v1/oauth/resume',
        body: {'auth_request_id': authRequestId},
      ) as Map<String, dynamic>;

      final data = response['data'] as Map<String, dynamic>?;
      final redirectUrl = data?['redirect_url'] as String?;
      if (redirectUrl != null && redirectUrl.isNotEmpty) {
        debugPrint('OIDC redirect to: $redirectUrl');
        web.window.location.href = redirectUrl;
      } else {
        onComplete();
      }
    } on Object catch (e) {
      debugPrint('OIDC resume failed: $e');
      onComplete();
    }
  }

  Future<bool> _redirectToNextIfValid() async {
    try {
      final response = await _api.get(
        '/api/v1/auth/validate-next',
        queryParameters: {'next': nextUrl},
      );
      if (response is Map && response['valid'] == true) {
        final redirectUrl = response['redirect_url'] as String?;
        if (redirectUrl != null && redirectUrl.isNotEmpty) {
          debugPrint('Redirecting to validated next URL: $redirectUrl');
          web.window.location.href = redirectUrl;
          return true;
        }
      }
    } on Object catch (e) {
      debugPrint('Next URL validation failed: $e');
    }
    return false;
  }
}
