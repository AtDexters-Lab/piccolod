import 'dart:convert';

import 'package:flutter/foundation.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/webauthn_service.dart';

/// A registered passkey returned by the API.
class Passkey {
  Passkey({
    required this.id,
    required this.friendlyName,
    required this.rpId,
    required this.createdAt,
    this.lastUsedAt,
  });

  factory Passkey.fromJson(Map<String, dynamic> json) {
    return Passkey(
      id: (json['id'] as String?) ?? '',
      friendlyName: ((json['friendly_name'] as String?) ?? '').isEmpty
          ? 'Passkey'
          : json['friendly_name'] as String,
      rpId: (json['rp_id'] as String?) ?? '',
      createdAt: json['created_at'] != null
          ? DateTime.parse(json['created_at'] as String)
          : DateTime.now(),
      lastUsedAt: json['last_used_at'] != null
          ? DateTime.parse(json['last_used_at'] as String)
          : null,
    );
  }

  final String id;
  final String friendlyName;
  final String rpId;
  final DateTime createdAt;
  final DateTime? lastUsedAt;

  /// Human-readable RP context label.
  String get rpContext {
    if (rpId.isEmpty) return 'LAN';
    if (rpId.contains('.') && !rpId.endsWith('.local')) return 'Remote';
    return 'LAN';
  }
}

/// Controller for managing passkeys in the Security settings tab.
class PasskeysController extends ChangeNotifier {
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

  List<Passkey> _passkeys = [];
  List<Passkey> get passkeys => _passkeys;

  /// Load all passkeys from the API.
  Future<void> loadPasskeys() async {
    if (_disposed) return;
    _isLoading = true;
    _error = null;
    _safeNotify();

    try {
      final list = await ApiClient().listPasskeys();
      if (_disposed) return;
      _passkeys = list
          .map((json) => Passkey.fromJson(json as Map<String, dynamic>))
          .toList();
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

  /// Register a new passkey via the WebAuthn ceremony.
  Future<void> registerPasskey() async {
    if (_disposed) return;
    _isLoading = true;
    _error = null;
    _safeNotify();

    try {
      final beginResult = await ApiClient().beginPasskeyRegistration();
      if (_disposed) return;

      final sessionId = beginResult['session_id'] as String;
      final options = beginResult['publicKey'] as Map<String, dynamic>;

      final credential = await WebAuthnService.createCredential(options);
      if (_disposed) return;

      await ApiClient().finishPasskeyRegistration(sessionId, credential);
      if (_disposed) return;

      await loadPasskeys();
    } on ApiException catch (e) {
      if (_disposed) return;
      _error = _friendlyApiError(e);
      _isLoading = false;
      _safeNotify();
      rethrow;
    } on Object catch (e) {
      if (_disposed) return;
      _error = _friendlyPasskeyError(e);
      _isLoading = false;
      _safeNotify();
      rethrow;
    }
  }

  /// Delete a passkey by ID.
  Future<void> deletePasskey(String id) async {
    if (_disposed) return;
    _isLoading = true;
    _error = null;
    _safeNotify();

    try {
      await ApiClient().deletePasskey(id);
      if (_disposed) return;
      await loadPasskeys();
    } on Object catch (e) {
      if (_disposed) return;
      _error = e.toString();
      _isLoading = false;
      _safeNotify();
      rethrow;
    }
  }

  /// Rename a passkey.
  Future<void> renamePasskey(String id, String name) async {
    if (_disposed) return;
    _isLoading = true;
    _error = null;
    _safeNotify();

    try {
      await ApiClient().renamePasskey(id, name);
      if (_disposed) return;
      await loadPasskeys();
    } on Object catch (e) {
      if (_disposed) return;
      _error = e.toString();
      _isLoading = false;
      _safeNotify();
      rethrow;
    }
  }

  String _friendlyApiError(ApiException e) {
    final serverMsg = _extractServerError(e.message);
    if (serverMsg.contains('ceremony expired') ||
        serverMsg == 'ceremony expired or not found') {
      return 'Session expired. Please try again.';
    }
    if (e.statusCode == 401) {
      return 'Your session has expired. Please sign in again.';
    }
    return 'Server error (${e.statusCode}). Please try again.';
  }

  /// Extract a human-readable error from a JSON error response body.
  String _extractServerError(String body) {
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

  String _friendlyPasskeyError(Object e) {
    final msg = e.toString();
    if (msg.contains('InvalidStateError') || msg.contains('already registered')) {
      return 'This authenticator already has a passkey registered for this device. Try a different authenticator.';
    }
    if (msg.contains('NotAllowedError') || msg.contains('cancelled')) {
      return 'Cancelled or timed out. If using a phone, ensure Bluetooth is on and devices are nearby.';
    }
    if (msg.contains('NotSupportedError')) {
      return 'Passkeys are not supported in this browser.';
    }
    if (msg.contains('not found') || msg.contains('expired')) {
      return 'Session expired. Please try again.';
    }
    debugPrint('Unexpected passkey error: $msg');
    return 'An unexpected error occurred. Please try again.';
  }
}
