import 'dart:convert';
import 'package:flutter/material.dart';
import 'package:web/web.dart' as web;
import '../../../../core/services/api_client.dart';

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
  complete, // Done, go to desktop
  error, // API error (connectivity)
  systemError, // LUKS / system failure
}

class SetupController extends ChangeNotifier {
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

  final ApiClient _api = ApiClient();

  SetupController() {
    _parseAuthRequestFromUrl();
    _parseNextFromUrl();
    _checkStatus();
  }

  /// Parse auth request ID from URL query parameters (for OIDC SSO flow)
  void _parseAuthRequestFromUrl() {
    try {
      final uri = Uri.base;
      // Use "id" parameter (OIDC library standard)
      _authRequestId = uri.queryParameters['id'];
      if (_authRequestId != null) {
        debugPrint('OIDC auth request detected: $_authRequestId');
      }
    } catch (e) {
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
    } catch (e) {
      debugPrint('Failed to parse next URL: $e');
    }
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
    } catch (e) {
      debugPrint('Next URL validation failed: $e');
    }

    return false;
  }

  Future<void> _checkStatus() async {
    try {
      _state = SetupState.loading;
      notifyListeners();

      // Check emergency status first — other endpoints may be degraded.
      final emergency = await _api.get('/api/v1/system/emergency');
      if (emergency['emergency'] == true) {
        final level = emergency['level'] as String?;
        if (level != 'soft') {
          // Hard emergency: irrecoverable, show system error.
          _error = emergency['error'] as String? ?? 'Storage emergency mode';
          _state = SetupState.systemError;
          return;
        }
        // Soft emergency: device was previously set up. Fall through to
        // crypto/status check so the user can reach the unlock screen.
      }

      // Check onboarding status — USB boot may require onboarding choice.
      final onboarding = await _api.get('/api/v1/system/onboarding');
      _bootMode = onboarding['boot_mode'] as String?;
      if (onboarding['required'] == true) {
        _state = SetupState.onboarding;
        return;
      }
      // If install completed, show reboot prompt.
      if (onboarding['state'] == 'install_disk' &&
          onboarding['install_done'] == true) {
        _state = SetupState.installComplete;
        return;
      }

      final status = await _api.get('/api/v1/crypto/status');
      // Expect: {"initialized": bool, "locked": bool}

      final initialized = status['initialized'] == true;
      _isFirstSetupFlow = !initialized;

      if (initialized) {
        if (status['locked'] == true) {
          _state = SetupState.unlock;
        } else {
          // Already unlocked. Check session.
          final session = await _api.get('/api/v1/auth/session');
          if (session['authenticated'] == true) {
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
          } else {
            _state = SetupState.login;
          }
        }
      } else {
        // Not initialized, start setup
        _state = SetupState.welcome;
      }
    } catch (e) {
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
        if (decoded['message'] is String) return decoded['message'];
        if (decoded['error'] is String) return decoded['error'];
      }
    } catch (_) {}
    return body;
  }

  // --- Onboarding methods ---

  /// User chose "Try Piccolo" — persist choice and trigger disk prep.
  Future<void> chooseTryPiccolo() async {
    try {
      _state = SetupState.finishing;
      _error = null;
      notifyListeners();

      await _api.post(
        '/api/v1/system/onboarding',
        body: {'choice': 'try_piccolo'},
      );

      // Disk prep started on backend. Re-check system status to determine the
      // correct next step: welcome (new system) or unlock (previously set up).
      await _checkStatus();
    } catch (e) {
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
    } catch (e) {
      _error = e.toString();
      _state = SetupState.onboarding;
      notifyListeners();
    }
  }

  /// Fetch available internal disks for install target selection.
  Future<void> fetchDisks() async {
    final response = await _api.get('/api/v1/storage/disks');
    final rawDisks = response['disks'] as List? ?? [];
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
    } catch (e) {
      _installTaskId = null;
      _error = e.toString();
      notifyListeners();
      return false;
    }
  }

  /// Called when install progress reaches 100% / isComplete.
  void onInstallComplete() {
    _state = SetupState.installComplete;
    notifyListeners();
  }

  /// Reboot the device after a successful install.
  Future<void> rebootAfterInstall() async {
    try {
      _state = SetupState.finishing;
      notifyListeners();
      await _api.post('/api/v1/system/reboot');
    } catch (e) {
      _error = e.toString();
      _state = SetupState.installComplete;
      notifyListeners();
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

      // 1. Initialize Crypto
      await _api.post('/api/v1/crypto/setup', body: {'password': password});

      // 2. Unlock (to get session and auth)
      await _api.post('/api/v1/crypto/unlock', body: {'password': password});

      // 3. Check if Auth is initialized
      final authState = await _api.get('/api/v1/auth/initialized');

      if (authState['initialized'] != true) {
        // 3b. Initialize Auth (create admin account) if not already done
        await _api.post('/api/v1/auth/setup', body: {'password': password});
      }

      // 4. Fetch CSRF Token
      await _api.fetchCsrfToken();

      // 5. Generate Recovery Key
      final recoveryData = await _api.post(
        '/api/v1/crypto/recovery-key/generate',
        body: {'password': password},
      );

      if (recoveryData != null && recoveryData['words'] != null) {
        _recoveryWords = List<String>.from(recoveryData['words']);
        _state = SetupState.recovery;
        notifyListeners();
        return true;
      } else {
        throw Exception("Failed to generate recovery key");
      }
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
    } catch (e) {
      _error = e.toString();
      _state = SetupState.credentials;
      notifyListeners();
      return false;
    }
  }

  Future<bool> unlock(String password) async {
    try {
      debugPrint('Unlock attempt, authRequestId: $_authRequestId');
      await _api.post('/api/v1/crypto/unlock', body: {'password': password});
      // After unlock, we might have an auto-created session (best-effort).
      // Let's verify or just proceed to login if needed.
      // Actually, handleCryptoUnlock does try to create a session.
      // Let's check session status to be sure.

      final session = await _api.get('/api/v1/auth/session');
      if (session['authenticated'] == true) {
        await _api.fetchCsrfToken();

        // Handle OIDC auth request if present (SSO flow)
        if (_authRequestId != null) {
          debugPrint('OIDC flow detected after unlock, completing auth request...');
          await _completeOidcAuthRequest();
          return true; // Don't set complete state - we're redirecting
        }

        if (_nextUrl != null && _nextUrl!.isNotEmpty) {
          if (await _redirectToNextIfValid()) {
            return true;
          }
        }

        _state = SetupState.complete;
      } else {
        // Unlocked but no session (weird but possible) -> Login
        _state = SetupState.login;
      }

      notifyListeners();
      return true;
    } on ApiException catch (e) {
      if (_isStorageSystemError(e)) {
        _error = _extractServerError(e.message);
        _state = SetupState.systemError;
      } else {
        _error = e.toString();
      }
      notifyListeners();
      return false;
    } catch (e) {
      _error = e.toString();
      notifyListeners();
      return false;
    }
  }

  Future<bool> login(String username, String password) async {
    try {
      debugPrint('Login attempt for user: $username');
      final resp = await _api.post(
        '/api/v1/auth/login',
        body: {'username': username, 'password': password, 'next': _nextUrl},
      );
      await _api.fetchCsrfToken();
      debugPrint('Login successful, authRequestId: $_authRequestId');

      // Handle OIDC auth request if present (SSO flow)
      if (_authRequestId != null) {
        debugPrint('OIDC flow detected, completing auth request...');
        await _completeOidcAuthRequest();
        return true; // Don't set complete state - we're redirecting
      }

      if (resp is Map && resp['redirect_url'] is String) {
        final redirectUrl = resp['redirect_url'] as String;
        if (redirectUrl.isNotEmpty) {
          debugPrint('Redirecting to login next URL: $redirectUrl');
          web.window.location.href = redirectUrl;
          return true;
        }
      }

      _state = SetupState.complete;
      notifyListeners();
      return true;
    } catch (e) {
      debugPrint('Login failed: $e');
      _error = e.toString();
      notifyListeners();
      return false;
    }
  }

  /// Complete the OIDC auth request and redirect back to the app
  Future<void> _completeOidcAuthRequest() async {
    if (_authRequestId == null) return;

    try {
      debugPrint('Completing OIDC auth request: $_authRequestId');

      final response = await _api.post(
        '/api/v1/oauth/resume',
        body: {'auth_request_id': _authRequestId},
      );

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
    } catch (e) {
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
    _checkStatus();
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
    } catch (e) {
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
    _checkStatus();
  }
}
