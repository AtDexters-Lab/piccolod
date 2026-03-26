import 'dart:async';
import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/webauthn_service.dart';
import 'package:web/web.dart' as web;

enum SetupState {
  loading, // Checking status
  onboarding, // USB boot: Try Piccolo / Install to Disk choice
  installDisk, // Disk selection + download + write progress
  installComplete, // Reboot prompt after successful install
  welcome, // First run intro
  credentials, // Set password
  recovery, // Recovery key
  security, // CA download for HTTPS
  finishing, // Finalizing setup
  unlock, // Already initialized, just need password
  login, // Unlocked but needs session
  forgotPassword, // Reset password flow
  invite, // Invite token flow (passkey registration for invited user)
  passkeyRequired, // Must register passkey before accessing dashboard
  complete, // Done, go to desktop
  error, // API error (connectivity)
  systemError, // LUKS / system failure
}

class SetupController extends ChangeNotifier {

  SetupController() {
    _parseAuthRequestFromUrl();
    _parseNextFromUrl();
    _parseInviteFromUrl();
    unawaited(_checkStatus());
  }
  SetupState _state = SetupState.loading;
  SetupState get state => _state;

  // True when this wizard session started on an uninitialized device.
  // Used by the Desktop shell to show first-run UX only once.
  bool _isFirstSetupFlow = false;
  bool get isFirstSetupFlow => _isFirstSetupFlow;

  String? _error;
  String? get error => _error;

  List<String> _recoveryWords = [];
  List<String> get recoveryWords => _recoveryWords;

  // OIDC auth request ID from URL (for SSO flow)
  String? _authRequestId;
  String? get authRequestId => _authRequestId;

  // Redirect target after login (for proxy-driven login flow)
  String? _nextUrl;

  // Onboarding / Install state
  List<Map<String, dynamic>> _disks = [];
  List<Map<String, dynamic>> get disks => _disks;

  String? _installTaskId;
  String? get installTaskId => _installTaskId;

  String? _bootMode;
  String? get bootMode => _bootMode;

  bool _bootOrderConfigured = false;
  bool get bootOrderConfigured => _bootOrderConfigured;

  // Passkey / invite state
  List<String>? _loginMethods;
  List<String>? get loginMethods => _loginMethods;

  String? _inviteToken;
  String? get inviteToken => _inviteToken;

  final ApiClient _api = ApiClient();

  /// Parse auth request ID from URL query parameters (for OIDC SSO flow)
  void _parseAuthRequestFromUrl() {
    try {
      final uri = Uri.base;
      // Use "id" parameter (OIDC library standard)
      _authRequestId = uri.queryParameters['id'];
      if (_authRequestId != null) {
        debugPrint('OIDC auth request detected: $_authRequestId');
      }
    } on Object catch (e) {
      debugPrint('Failed to parse URL: $e');
    }
  }

  void _parseNextFromUrl() {
    try {
      final uri = Uri.base;
      _nextUrl = uri.queryParameters['next'];
      if (_nextUrl != null && _nextUrl!.isNotEmpty) {
        debugPrint('Login redirect target detected: $_nextUrl');
      }
    } on Object catch (e) {
      debugPrint('Failed to parse next URL: $e');
    }
  }

  void _parseInviteFromUrl() {
    try {
      final token = Uri.base.queryParameters['invite'];
      if (token != null && token.isNotEmpty) {
        _inviteToken = token;
      }
    } on Object catch (_) {}
  }

  Future<bool> _redirectToNextIfValid() async {
    if (_nextUrl == null || _nextUrl!.isEmpty) return false;

    try {
      final response = await _api.get(
        '/api/v1/auth/validate-next',
        queryParameters: {'next': _nextUrl},
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

  Future<void> _checkStatus() async {
    try {
      _state = SetupState.loading;
      notifyListeners();

      final boot = await _api.get('/api/v1/system/boot') as Map<String, dynamic>;
      final screen = boot['screen'] as String?;

      switch (screen) {
        case 'emergency':
          _error = boot['error'] as String? ?? 'Storage emergency mode';
          _state = SetupState.systemError;

        case 'onboarding':
          _bootMode = boot['boot_mode'] as String?;
          _bootOrderConfigured = boot['boot_order_configured'] == true;
          // If the boot endpoint detected an abandoned USB install, revert
          // onboarding state so the user can retry.
          if (boot['install_abandoned'] == true) {
            try {
              await _api.post('/api/v1/system/onboarding', body: {'choice': 'pending'});
            } on Object catch (e) {
              debugPrint('Revert to onboarding failed: $e');
              _error = 'Installation failed. Please reboot to try again.';
              _state = SetupState.error;
              return;
            }
          }
          _state = SetupState.onboarding;

        case 'install_progress':
          _installTaskId = boot['install_task_id'] as String?;
          _state = SetupState.installDisk;

        case 'install_complete':
          _bootOrderConfigured = boot['boot_order_configured'] == true;
          _state = SetupState.installComplete;

        case 'setup':
          _isFirstSetupFlow = true;
          _state = SetupState.welcome;

        case 'unlock':
          _state = SetupState.unlock;

        case 'login':
          if (_inviteToken != null) {
            _state = SetupState.invite;
          } else {
            _state = SetupState.login;
          }

        case 'passkey_required':
          await _api.fetchCsrfToken();
          _state = SetupState.passkeyRequired;

        case 'desktop':
          await _api.fetchCsrfToken();

          // If there's an OIDC auth request, complete it immediately
          if (_authRequestId != null) {
            await _completeOidcAuthRequest();
            return; // Don't update state - we're redirecting
          }

          // Proxy-driven login flow: redirect back to the original target
          if (_nextUrl != null && _nextUrl!.isNotEmpty) {
            if (await _redirectToNextIfValid()) {
              return;
            }
          }

          _state = SetupState.complete;

        default:
          debugPrint('Unknown boot screen: $screen');
          _error = 'Unexpected server state';
          _state = SetupState.error;
      }
    } on Object catch (e) {
      _error = e.toString();
      _state = SetupState.error;
    } finally {
      notifyListeners();
    }
  }

  void startSetup() {
    _state = SetupState.credentials;
    notifyListeners();
  }

  /// Checks if an API exception represents a storage system error.
  /// Covers LUKS data volume failures (500) and emergency middleware blocks (503).
  bool _isStorageSystemError(ApiException e) {
    // LUKS data volume failure (HTTP 500).
    // Coupled to backend error message prefixes in gin_crypto_handlers.go.
    // False negatives degrade to generic error UI.
    if (e.statusCode == 500 &&
        (e.message.contains('data volume initialization failed:') ||
            e.message.contains('data volume unlock failed:'))) {
      return true;
    }
    // Storage emergency mode (HTTP 503 from emergency middleware).
    if (e.statusCode == 503 && e.message.contains('storage emergency')) {
      return true;
    }
    return false;
  }

  /// Extract a human-readable error from a JSON error response body.
  /// Prefers "message" (more descriptive, used by emergency middleware),
  /// falls back to "error" (LUKS handlers), then to the raw body.
  String _extractServerError(String body) {
    try {
      final decoded = jsonDecode(body);
      if (decoded is Map) {
        if (decoded['message'] is String) return decoded['message'] as String;
        if (decoded['error'] is String) return decoded['error'] as String;
      }
    } on Object catch (_) {}
    return body;
  }

  // --- Onboarding methods ---

  /// User chose "Try Piccolo" — persist choice and trigger disk prep.
  Future<void> chooseTryPiccolo() async {
    try {
      _state = SetupState.loading;
      _error = null;
      notifyListeners();

      await _api.post(
        '/api/v1/system/onboarding',
        body: {'choice': 'try_piccolo'},
      );

      // Disk prep started on backend. Re-check system status to determine the
      // correct next step: welcome (new system) or unlock (previously set up).
      await _checkStatus();
    } on Object catch (e) {
      _error = e.toString();
      _state = SetupState.onboarding;
      notifyListeners();
    }
  }

  /// User chose "Install to Disk" — fetch disks and transition.
  Future<void> chooseInstallDisk() async {
    try {
      _state = SetupState.loading;
      _error = null;
      notifyListeners();

      await fetchDisks();
      _state = SetupState.installDisk;
      notifyListeners();
    } on Object catch (e) {
      _error = e.toString();
      _state = SetupState.onboarding;
      notifyListeners();
    }
  }

  /// Fetch available internal disks for install target selection.
  Future<void> fetchDisks() async {
    final response = await _api.get('/api/v1/storage/disks') as Map<String, dynamic>;
    final rawDisks = response['disks'] as List<dynamic>? ?? <dynamic>[];
    _disks = rawDisks.cast<Map<String, dynamic>>();
    notifyListeners();
  }

  /// Start the install-to-disk pipeline.
  Future<bool> startInstall(String targetDisk) async {
    try {
      _error = null;
      final taskId =
          'install-${DateTime.now().millisecondsSinceEpoch}';
      _installTaskId = taskId;
      notifyListeners();

      await _api.post('/api/v1/system/install-to-disk', body: {
        'target_disk': targetDisk,
        'confirm_data_loss': true,
        'task_id': taskId,
      });
      return true;
    } on ApiException catch (e) {
      _installTaskId = null;
      _error = _extractServerError(e.message);
      notifyListeners();
      return false;
    } on Object catch (e) {
      _installTaskId = null;
      _error = e.toString();
      notifyListeners();
      return false;
    }
  }

  /// Called when install progress reaches 100% / isComplete.
  /// Fetches fresh onboarding status to get boot_order_configured.
  Future<void> onInstallComplete() async {
    try {
      final onboarding = await _api.get('/api/v1/system/onboarding') as Map<String, dynamic>;
      _bootOrderConfigured = onboarding['boot_order_configured'] == true;
    } on Object catch (_) {
      // Non-fatal; default false shows the safer "Power Off" path.
    }
    _state = SetupState.installComplete;
    notifyListeners();
  }

  /// Reboot the device after a successful install.
  ///
  /// Network/connection errors are swallowed: the reboot kills the server, so
  /// the HTTP call may fail before a response arrives. That is success, not
  /// failure. Real API errors (409, 500) are re-thrown so the UI can react.
  Future<void> rebootAfterInstall() async {
    try {
      await _api.post('/api/v1/system/reboot');
    } on ApiException {
      // Real server error (e.g. 409 "reboot only available after install") —
      // the device did NOT reboot. Re-throw so the caller can handle it.
      rethrow;
    } on Object catch (_) {
      // Connection reset / timeout — expected during reboot.
    }
  }

  /// Go back to onboarding choice from install disk view.
  void backToOnboarding() {
    _state = SetupState.onboarding;
    _error = null;
    _installTaskId = null;
    notifyListeners();
  }

  // --- Credentials / Setup flow ---

  Future<bool> submitCredentials(String password) async {
    try {
      _state = SetupState.finishing;
      _error = null;
      notifyListeners();

      const timeout = Duration(seconds: 120);

      // 1. Initialize Crypto (also unlocks, sets up auth, creates admin user, creates session)
      await _api.post('/api/v1/crypto/setup', body: {'password': password})
          .timeout(timeout);

      // 2. Fetch CSRF Token
      await _api.fetchCsrfToken().timeout(timeout);

      // 3. Generate Recovery Key
      final recoveryData = await _api.post(
        '/api/v1/crypto/recovery-key/generate',
        body: {'password': password},
      ).timeout(timeout) as Map<String, dynamic>?;

      if (recoveryData != null && recoveryData['words'] != null) {
        _recoveryWords = List<String>.from(recoveryData['words'] as Iterable<dynamic>);
        _state = SetupState.recovery;
        notifyListeners();
        return true;
      } else {
        throw Exception('Failed to generate recovery key');
      }
    } on TimeoutException {
      _error = 'Setup timed out. Please check your connection and try again.';
      _state = SetupState.credentials;
      notifyListeners();
      return false;
    } on ApiException catch (e) {
      if (_isStorageSystemError(e)) {
        _error = _extractServerError(e.message);
        _state = SetupState.systemError;
      } else {
        _error = e.toString();
        _state = SetupState.credentials;
      }
      notifyListeners();
      return false;
    } on Object catch (e) {
      _error = e.toString();
      _state = SetupState.credentials;
      notifyListeners();
      return false;
    }
  }

  Future<bool> unlock(String password) async {
    try {
      debugPrint('Unlock attempt, authRequestId: $_authRequestId');
      final unlockResp = await _api.post(
        '/api/v1/crypto/unlock',
        body: {'password': password},
      ) as Map<String, dynamic>?;

      // Two-door model: unlock is a disk operation only — no session is created.
      // Check for data volume warning (system degraded).
      if (unlockResp?['warning'] != null) {
        _error = unlockResp!['warning'] as String;
        _state = SetupState.systemError;
        notifyListeners();
        return true; // Unlock succeeded, but system is degraded.
      }

      // Route based on setup_complete: partial setup failure → retry setup,
      // normal reboot → chain login.
      if (unlockResp?['setup_complete'] == false) {
        await _completeSetupAfterUnlock(password);
      } else {
        await _chainLoginAfterUnlock(password);
      }
      notifyListeners();
      return true;
    } on ApiException catch (e) {
      _error = e.toString();
      notifyListeners();
      return false;
    } on Object catch (e) {
      _error = e.toString();
      notifyListeners();
      return false;
    }
  }

  /// Complete a partial setup after unlock. Called when setup_complete is false
  /// (reboot after interrupted first-run). Setup is idempotent — it picks up
  /// where it left off (auth init, admin creation, session).
  Future<void> _completeSetupAfterUnlock(String password) async {
    debugPrint('Partial setup detected after unlock, completing via /crypto/setup');
    await _api.post('/api/v1/crypto/setup', body: {'password': password});
    await _api.fetchCsrfToken();
    _state = SetupState.complete;
  }

  /// Attempt password login immediately after a successful unlock.
  /// Falls back to the login screen if the backend rejects password auth
  /// (e.g. remote passkey-only policy).
  Future<void> _chainLoginAfterUnlock(String password) async {
    try {
      await _completeLoginFlow('admin', password);
    } on ApiException catch (e) {
      if (e.statusCode == 423) {
        // Intentionally no _state change — stay on the unlock screen so the
        // user sees the error and can retry.
        _error = 'Unlock partially failed, please try again';
        return;
      }
      debugPrint('Chained login after unlock failed (${e.statusCode}), '
          'falling back to login screen');
      _state = SetupState.login;
    } on Object catch (e) {
      debugPrint('Chained login after unlock failed: $e, '
          'falling back to login screen');
      _state = SetupState.login;
    }
  }

  Future<bool> login(String username, String password) async {
    try {
      await _completeLoginFlow(username, password);
      notifyListeners();
      return true;
    } on Object catch (e) {
      debugPrint('Login failed: $e');
      _error = e.toString();
      notifyListeners();
      return false;
    }
  }

  /// Shared post-login flow: authenticate, handle passkey registration,
  /// OIDC, redirects, and set final state. Throws on failure.
  Future<void> _completeLoginFlow(String username, String password) async {
    final resp = await _api.post(
      '/api/v1/auth/login',
      body: {'username': username, 'password': password, 'next': _nextUrl},
    );
    await _api.fetchCsrfToken();

    // Check passkey registration requirement from login response.
    if (resp is Map && resp['must_register_passkey'] == true) {
      _state = SetupState.passkeyRequired;
      return;
    }

    if (_authRequestId != null) {
      debugPrint('OIDC flow detected, completing auth request...');
      await _completeOidcAuthRequest();
      return;
    }

    if (resp is Map && resp['redirect_url'] is String) {
      final redirectUrl = resp['redirect_url'] as String;
      if (redirectUrl.isNotEmpty) {
        debugPrint('Redirecting to login next URL: $redirectUrl');
        web.window.location.href = redirectUrl;
        return;
      }
    }

    if (_nextUrl != null && _nextUrl!.isNotEmpty) {
      if (await _redirectToNextIfValid()) return;
    }

    _state = SetupState.complete;
  }

  /// Complete the OIDC auth request and redirect back to the app
  Future<void> _completeOidcAuthRequest() async {
    if (_authRequestId == null) return;

    try {
      debugPrint('Completing OIDC auth request: $_authRequestId');

      final response = await _api.post(
        '/api/v1/oauth/resume',
        body: {'auth_request_id': _authRequestId},
      ) as Map<String, dynamic>;

      // The backend returns {data: {redirect_url: "..."}, message: "..."}
      final data = response['data'] as Map<String, dynamic>?;
      final redirectUrl = data?['redirect_url'] as String?;
      if (redirectUrl != null && redirectUrl.isNotEmpty) {
        debugPrint('OIDC redirect to: $redirectUrl');
        // Redirect the browser to complete the OIDC flow
        web.window.location.href = redirectUrl;
      } else {
        // Fallback: if no redirect URL, just go to desktop
        _state = SetupState.complete;
        notifyListeners();
      }
    } on Object catch (e) {
      debugPrint('OIDC resume failed: $e');
      _error = 'Failed to complete SSO login: $e';
      // On error, still go to desktop (the OIDC flow failed but user is logged in)
      _state = SetupState.complete;
      notifyListeners();
    }
  }

  void startRecovery() {
    _state = SetupState.forgotPassword;
    _error = null;
    notifyListeners();
  }

  void cancelRecovery() {
    // Check status again to decide where to go (unlock or login)
    unawaited(_checkStatus());
  }

  Future<bool> resetPassword(String recoveryKey, String newPassword) async {
    try {
      await _api.post(
        '/api/v1/crypto/reset-password',
        body: {'recovery_key': recoveryKey, 'new_password': newPassword},
      );

      // Success -> Go to Login (user must login with new password)
      _state = SetupState.login;
      _error = null; // Clear error
      // Optional: Show a "Password reset successful" message?
      // For now, rely on the UI transition.
      notifyListeners();
      return true;
    } on Object catch (e) {
      _error = e.toString();
      notifyListeners();
      return false;
    }
  }

  void proceedToSecurity() {
    _state = SetupState.security;
    notifyListeners();
  }

  void completeSetup() {
    _state = SetupState.complete;
    notifyListeners();
  }

  void retry() {
    _error = null;
    unawaited(_checkStatus());
  }

  // --- Passkey ---

  Future<void> fetchLoginOptions() async {
    try {
      final result = await _api
          .getLoginOptions()
          .timeout(const Duration(seconds: 3));
      _loginMethods = List<String>.from(result['methods'] as List);
      notifyListeners();
    } on Object catch (_) {
      _loginMethods = ['password'];
      notifyListeners();
    }
  }

  Future<bool> loginWithPasskey() async {
    _error = null;
    notifyListeners();
    try {
      final beginResult = await _api.beginPasskeyLogin();
      final sessionId = beginResult['session_id'] as String;
      final options = beginResult['publicKey'] as Map<String, dynamic>;

      final credential = await WebAuthnService.getCredential(options);

      await _api.finishPasskeyLogin(sessionId, credential);
      await _api.fetchCsrfToken();

      final session = await _api.get('/api/v1/auth/session') as Map<String, dynamic>;
      if (session['must_register_passkey'] == true) {
        _state = SetupState.passkeyRequired;
        notifyListeners();
        return true;
      }

      if (_authRequestId != null) {
        await _completeOidcAuthRequest();
        return true;
      }

      if (_nextUrl != null && _nextUrl!.isNotEmpty) {
        if (await _redirectToNextIfValid()) {
          return true;
        }
      }

      _state = SetupState.complete;
      notifyListeners();
      return true;
    } on Object catch (e) {
      _error = _friendlyPasskeyError(e);
      notifyListeners();
      return false;
    }
  }

  Future<bool> registerPasskey() async {
    _error = null;
    notifyListeners();
    try {
      final beginResult = await _api.beginPasskeyRegistration();
      final sessionId = beginResult['session_id'] as String;
      final options = beginResult['publicKey'] as Map<String, dynamic>;

      final credential = await WebAuthnService.createCredential(options);

      await _api.finishPasskeyRegistration(sessionId, credential);

      if (_authRequestId != null) {
        await _completeOidcAuthRequest();
        return true;
      }
      if (_nextUrl != null && _nextUrl!.isNotEmpty) {
        if (await _redirectToNextIfValid()) {
          return true;
        }
      }

      _state = SetupState.complete;
      notifyListeners();
      return true;
    } on Object catch (e) {
      _error = _friendlyPasskeyError(e);
      notifyListeners();
      return false;
    }
  }

  // --- Invite ---

  Future<String?> validateInviteToken() async {
    if (_inviteToken == null) return null;
    try {
      final result = await _api.validateInvite(_inviteToken!);
      return result['username'] as String?;
    } on Object catch (_) {
      return null;
    }
  }

  Future<bool> completeInviteRegistration() async {
    if (_inviteToken == null) return false;
    _error = null;
    notifyListeners();
    try {
      final beginResult = await _api.beginInvitePasskey(_inviteToken!);
      final sessionId = beginResult['session_id'] as String;
      final options = beginResult['publicKey'] as Map<String, dynamic>;

      final credential = await WebAuthnService.createCredential(options);

      await _api.finishInvitePasskey(_inviteToken!, sessionId, credential);
      await _api.fetchCsrfToken();
      _inviteToken = null;
      _state = SetupState.complete;
      notifyListeners();
      return true;
    } on Object catch (e) {
      _error = _friendlyPasskeyError(e);
      notifyListeners();
      return false;
    }
  }

  String _friendlyPasskeyError(Object e) {
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
    return 'Passkey error: $msg';
  }
}
