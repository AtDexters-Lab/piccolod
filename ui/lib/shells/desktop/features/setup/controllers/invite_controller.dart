import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/webauthn_service.dart';
import 'package:piccolo_os/shells/desktop/features/setup/setup_utils.dart';

enum InvitePhase { validating, ready }

class InviteController extends ChangeNotifier {
  InviteController({
    required this.token,
    required this.onComplete,
  }) {
    unawaited(_validate());
  }

  final String token;
  final VoidCallback onComplete;
  final ApiClient _api = ApiClient();
  bool _disposed = false;

  @override
  void dispose() {
    _disposed = true;
    super.dispose();
  }

  InvitePhase _step = InvitePhase.validating;
  InvitePhase get step => _step;

  String? _username;
  String? get username => _username;

  String? _error;
  String? get error => _error;

  bool _isRegistering = false;
  bool get isRegistering => _isRegistering;

  bool get isFirstSetupFlow => false;

  Future<void> _validate() async {
    try {
      final result = await _api.validateInvite(token);
      _username = result['username'] as String?;
      if (_username == null) {
        _error = 'This invite link is invalid or has expired.';
      }
    } on Object catch (_) {
      _error = 'This invite link is invalid or has expired.';
    }
    if (_disposed) return;
    _step = InvitePhase.ready;
    notifyListeners();
  }

  Future<bool> register() async {
    _error = null;
    _isRegistering = true;
    notifyListeners();
    try {
      final beginResult = await _api.beginInvitePasskey(token);
      final sessionId = beginResult['session_id'] as String;
      final options = beginResult['publicKey'] as Map<String, dynamic>;

      final credential = await WebAuthnService.createCredential(options);

      await _api.finishInvitePasskey(token, sessionId, credential);
      await _api.fetchCsrfToken();
      onComplete();
      return true;
    } on ApiException catch (e) {
      _error = friendlyApiError(e);
      _isRegistering = false;
      if (!_disposed) notifyListeners();
      return false;
    } on Object catch (e) {
      _error = friendlyPasskeyError(e);
      _isRegistering = false;
      if (!_disposed) notifyListeners();
      return false;
    }
  }
}
