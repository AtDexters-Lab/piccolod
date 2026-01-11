import 'package:flutter/material.dart';
import 'package:web/web.dart' as web;
import '../../../../core/services/api_client.dart';

enum SetupState {
  loading, // Checking status
  welcome, // First run intro
  credentials, // Set password
  recovery, // Recovery key
  finishing, // Finalizing setup
  unlock, // Already initialized, just need password
  login, // Unlocked but needs session
  forgotPassword, // Reset password flow
  complete, // Done, go to desktop
  error, // API error
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

  final ApiClient _api = ApiClient();

  SetupController() {
    _parseAuthRequestFromUrl();
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

  Future<void> _checkStatus() async {
    try {
      _state = SetupState.loading;
      notifyListeners();

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

        _state = SetupState.complete;
      } else {
        // Unlocked but no session (weird but possible) -> Login
        _state = SetupState.login;
      }

      notifyListeners();
      return true;
    } catch (e) {
      _error = e.toString();
      notifyListeners();
      return false;
    }
  }

  Future<bool> login(String username, String password) async {
    try {
      debugPrint('Login attempt for user: $username');
      await _api.post(
        '/api/v1/auth/login',
        body: {'username': username, 'password': password},
      );
      await _api.fetchCsrfToken();
      debugPrint('Login successful, authRequestId: $_authRequestId');

      // Handle OIDC auth request if present (SSO flow)
      if (_authRequestId != null) {
        debugPrint('OIDC flow detected, completing auth request...');
        await _completeOidcAuthRequest();
        return true; // Don't set complete state - we're redirecting
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

  void completeSetup() {
    _state = SetupState.complete;
    notifyListeners();
  }

  void retry() {
    _error = null;
    _checkStatus();
  }
}
