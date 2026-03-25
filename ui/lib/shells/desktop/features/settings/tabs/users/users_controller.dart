import 'package:flutter/foundation.dart';
import 'package:piccolo_os/core/models/user.dart';
import 'package:piccolo_os/core/services/api_client.dart';

/// Controller for managing users in the Settings app.
class UsersController extends ChangeNotifier {
  bool _disposed = false;

  @override
  void dispose() {
    _disposed = true;
    super.dispose();
  }

  void _safeNotify() {
    if (!_disposed) notifyListeners();
  }

  // State
  bool _isLoading = false;
  bool get isLoading => _isLoading;

  String? _error;
  String? get error => _error;

  List<User> _users = [];
  List<User> get users => _users;

  User? _selectedUser;
  User? get selectedUser => _selectedUser;

  /// Load all users from the API.
  Future<void> loadUsers() async {
    if (_disposed) return;
    _isLoading = true;
    _error = null;
    _safeNotify();

    try {
      final response = await ApiClient().get('/api/v1/users');
      if (_disposed) return;

      final usersList = (response as Map<String, dynamic>)['users'] as List<dynamic>? ?? <dynamic>[];
      _users = usersList.map((json) => User.fromJson(json as Map<String, dynamic>)).toList();
      _error = null;
    } on Object catch (e) {
      if (_disposed) return;
      _error = e.toString();
    } finally {
      if (!_disposed) {
        _isLoading = false;
        _safeNotify();
      }
    }
  }

  /// Select a user for viewing/editing.
  void selectUser(User? user) {
    if (_disposed) return;
    _selectedUser = user;
    _safeNotify();
  }

  /// Create a new user.
  Future<void> createUser({
    required String username,
    required String email,
    required String password,
    required String role,
    List<String> allowedApps = const [],
  }) async {
    if (_disposed) return;
    _isLoading = true;
    _error = null;
    _safeNotify();

    try {
      await ApiClient().fetchCsrfToken();
      if (_disposed) return;

      await ApiClient().post('/api/v1/users', body: {
        'username': username,
        'email': email,
        'password': password,
        'role': role,
        'allowed_apps': allowedApps,
      });

      if (_disposed) return;
      await loadUsers();
    } catch (e) {
      if (_disposed) return;
      _error = e.toString();
      _isLoading = false;
      _safeNotify();
      rethrow;
    }
  }

  /// Update an existing user.
  Future<void> updateUser({
    required String id,
    String? username,
    String? email,
    String? role,
    List<String>? allowedApps,
  }) async {
    if (_disposed) return;
    _isLoading = true;
    _error = null;
    _safeNotify();

    try {
      await ApiClient().fetchCsrfToken();
      if (_disposed) return;

      final body = <String, dynamic>{};
      if (username != null) body['username'] = username;
      if (email != null) body['email'] = email;
      if (role != null) body['role'] = role;
      if (allowedApps != null) body['allowed_apps'] = allowedApps;

      await ApiClient().put('/api/v1/users/$id', body: body);

      if (_disposed) return;
      await loadUsers();
    } catch (e) {
      if (_disposed) return;
      _error = e.toString();
      _isLoading = false;
      _safeNotify();
      rethrow;
    }
  }

  /// Delete a user.
  Future<void> deleteUser(String id) async {
    if (_disposed) return;
    _isLoading = true;
    _error = null;
    _safeNotify();

    try {
      await ApiClient().fetchCsrfToken();
      if (_disposed) return;

      await ApiClient().delete('/api/v1/users/$id');

      if (_disposed) return;
      if (_selectedUser?.id == id) {
        _selectedUser = null;
      }
      await loadUsers();
    } catch (e) {
      if (_disposed) return;
      _error = e.toString();
      _isLoading = false;
      _safeNotify();
      rethrow;
    }
  }

  /// Set a user's password (admin action).
  Future<void> setUserPassword(String id, String newPassword) async {
    if (_disposed) return;
    _isLoading = true;
    _error = null;
    _safeNotify();

    try {
      await ApiClient().fetchCsrfToken();
      if (_disposed) return;

      await ApiClient().post('/api/v1/users/$id/password', body: {
        'password': newPassword,
      });

      if (_disposed) return;
      _error = null;
    } catch (e) {
      if (_disposed) return;
      _error = e.toString();
      rethrow;
    } finally {
      if (!_disposed) {
        _isLoading = false;
        _safeNotify();
      }
    }
  }

  /// Create an invite for a new user (passkey-based onboarding).
  /// Returns the invite token on success.
  Future<String> createInvite({
    required String username,
    required String email,
    List<String> allowedApps = const [],
  }) async {
    if (_disposed) return '';
    _isLoading = true;
    _error = null;
    _safeNotify();

    try {
      await ApiClient().fetchCsrfToken();
      if (_disposed) return '';

      final result = await ApiClient().createInvite(
        username: username,
        email: email,
        allowedApps: allowedApps.isNotEmpty ? allowedApps : null,
      );

      if (_disposed) return '';
      await loadUsers();
      return (result['token'] as String?) ?? '';
    } catch (e) {
      if (_disposed) return '';
      _error = e.toString();
      _isLoading = false;
      _safeNotify();
      rethrow;
    }
  }

  /// Reinvite an existing user, returning a fresh invite token.
  Future<String> reinviteUser(String userId) async {
    if (_disposed) return '';
    _isLoading = true;
    _error = null;
    _safeNotify();

    try {
      await ApiClient().fetchCsrfToken();
      if (_disposed) return '';

      final result = await ApiClient().reinviteUser(userId);

      if (_disposed) return '';
      _isLoading = false;
      _safeNotify();
      return (result['token'] as String?) ?? '';
    } catch (e) {
      if (_disposed) return '';
      _error = e.toString();
      _isLoading = false;
      _safeNotify();
      rethrow;
    }
  }
}
