import 'package:flutter/material.dart';
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

  String? _error;
  String? get error => _error;

  final String _deviceName = "Piccolo Node";
  String get deviceName => _deviceName;

  List<String> _recoveryWords = [];
  List<String> get recoveryWords => _recoveryWords;

  final ApiClient _api = ApiClient();

  SetupController() {
    _checkStatus();
  }

  Future<void> _checkStatus() async {
    try {
      _state = SetupState.loading;
      notifyListeners();

      final status = await _api.get('/api/v1/crypto/status');
      // Expect: {"initialized": bool, "locked": bool}

      if (status['initialized'] == true) {
        if (status['locked'] == true) {
          _state = SetupState.unlock;
        } else {
          // Already unlocked. Check session.
          final session = await _api.get('/api/v1/auth/session');
          if (session['authenticated'] == true) {
            await _api.fetchCsrfToken();
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
      notifyListeners();
      return false;
    }
  }

  Future<bool> unlock(String password) async {
    try {
      await _api.post('/api/v1/crypto/unlock', body: {'password': password});
      // After unlock, we might have an auto-created session (best-effort).
      // Let's verify or just proceed to login if needed.
      // Actually, handleCryptoUnlock does try to create a session.
      // Let's check session status to be sure.

      final session = await _api.get('/api/v1/auth/session');
      if (session['authenticated'] == true) {
        await _api.fetchCsrfToken();
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
      await _api.post(
        '/api/v1/auth/login',
        body: {'username': username, 'password': password},
      );
      await _api.fetchCsrfToken();

      _state = SetupState.complete;
      notifyListeners();
      return true;
    } catch (e) {
      _error = e.toString();
      notifyListeners();
      return false;
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
