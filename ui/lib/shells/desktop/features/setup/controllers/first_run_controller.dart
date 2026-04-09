import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/webauthn_service.dart';
import 'package:piccolo_os/shells/desktop/features/setup/setup_utils.dart';
import 'package:web/web.dart' as web;

enum FirstRunStep {
  welcome,
  remoteAddress,
  preparingRemote,
  credentials,
  recoveryKey,
  security,
  passkeyRequired,
}

class FirstRunController extends ChangeNotifier {
  FirstRunController({
    required this.isRemote,
    required this.namekEnrolled,
    required this.namekBaseDomain,
    required this.onComplete,
    required this.reRoute,
    required this.onSystemError,
    required this.generateRecoveryKey,
    this.namekSuggestedHostname,
    this.namekStatus,
    String? setupNonce,
  }) : _setupNonce = setupNonce {
    _step = isRemote ? FirstRunStep.credentials : FirstRunStep.welcome;
  }

  final bool isRemote;
  bool namekEnrolled;
  String? namekBaseDomain;
  String? namekSuggestedHostname;
  // Tri-state signal from the backend: "enrolled" | "pending" | "unavailable".
  // Null for legacy backends — UI treats null as "unavailable" branch.
  String? namekStatus;
  final VoidCallback onComplete;
  final VoidCallback reRoute;
  final void Function(String error) onSystemError;
  final Future<List<String>?> Function() generateRecoveryKey;

  String? _setupNonce;
  final ApiClient _api = ApiClient();
  bool _disposed = false;

  FirstRunStep _step = FirstRunStep.welcome;
  FirstRunStep get step => _step;

  bool get isFirstSetupFlow => true;

  String? _error;
  String? get error => _error;

  SetupPhase? _setupPhase;
  SetupPhase? get setupPhase => _setupPhase;

  List<String> _recoveryWords = [];
  List<String> get recoveryWords => _recoveryWords;

  // Hostname claim state
  bool _settingHostname = false;
  bool get settingHostname => _settingHostname;

  String? _hostnameError;
  String? get hostnameError => _hostnameError;

  // Remote readiness polling state
  bool _relayReady = false;
  bool get relayReady => _relayReady;
  bool _certReady = false;
  bool get certReady => _certReady;
  bool _remoteFallback = false;
  bool get remoteFallback => _remoteFallback;
  String? _pendingFqdn;
  String? _pendingNonce;
  Timer? _readinessTimer;
  bool _pollCancelled = false;
  final _readinessStopwatch = Stopwatch();

  String? _passkeyError;
  String? get passkeyError => _passkeyError;

  @override
  void dispose() {
    _disposed = true;
    _pollCancelled = true;
    _readinessTimer?.cancel();
    super.dispose();
  }

  // --- Step transitions ---

  void startSetup() {
    _step = FirstRunStep.remoteAddress;
    _persistSetupStarted();
    notifyListeners();
  }

  /// Restore preparingRemote state from sessionStorage (LAN refresh).
  void restorePreparingRemote(String fqdn, String? nonce) {
    _pendingFqdn = fqdn;
    _pendingNonce = nonce;
    _relayReady = false;
    _certReady = false;
    _step = FirstRunStep.preparingRemote;
    _startReadinessPolling();
    notifyListeners();
  }

  /// Re-check boot response to detect if auto-enrollment completed.
  Future<void> refreshEnrollmentStatus() async {
    try {
      final boot = await _api.get('/api/v1/system/boot') as Map<String, dynamic>;
      if (boot['screen'] == 'setup') {
        namekEnrolled = boot['namek_enrolled'] == true;
        namekBaseDomain = boot['namek_base_domain'] as String?;
        namekSuggestedHostname = boot['namek_suggested_hostname'] as String?;
        namekStatus = boot['namek_status'] as String?;
        notifyListeners();
      }
    } on Object catch (_) {}
  }

  void skipRemoteAddress() {
    _step = FirstRunStep.credentials;
    _persistSetupStep('credentials');
    notifyListeners();
  }

  void backToRemoteAddress() {
    _step = FirstRunStep.remoteAddress;
    _error = null;
    _remoteFallback = false;
    notifyListeners();
  }

  // --- Hostname claim ---

  Future<bool> submitHostname(String hostname) async {
    _readinessTimer?.cancel();
    _readinessTimer = null;
    _pollCancelled = true;

    try {
      _settingHostname = true;
      _hostnameError = null;
      notifyListeners();

      final result = await _api.post(
        '/api/v1/identity/setup-hostname',
        body: {'hostname': hostname},
      ) as Map<String, dynamic>;

      final fqdn = result['fqdn'] as String?;
      final baseDomain = result['base_domain'] as String?;
      final nonce = result['setup_nonce'] as String?;

      _settingHostname = false;
      namekSuggestedHostname = null; // prevent stale suggestion on back-navigation
      _pendingFqdn = fqdn ?? '$hostname.$baseDomain';
      _pendingNonce = nonce;
      _relayReady = false;
      _certReady = false;
      _step = FirstRunStep.preparingRemote;
      _persistPreparingState();
      notifyListeners();

      _startReadinessPolling();
      return true;
    } on ApiException catch (e) {
      _hostnameError = extractServerError(e.message);
      namekSuggestedHostname = null; // clear stale suggestion on rejection
      _settingHostname = false;
      notifyListeners();
      return false;
    } on Object catch (e) {
      _hostnameError = e.toString();
      // Don't clear suggestion on transient errors (timeout, network) —
      // the user should retry with the same hostname.
      _settingHostname = false;
      notifyListeners();
      return false;
    }
  }

  // --- Readiness polling ---

  void _startReadinessPolling() {
    _pollCancelled = false;
    _readinessStopwatch
      ..reset()
      ..start();
    _readinessTimer?.cancel();
    _scheduleNextPoll();
  }

  void _scheduleNextPoll() {
    _readinessTimer = Timer(const Duration(seconds: 3), _pollReadiness);
  }

  Future<void> _pollReadiness() async {
    if (_pollCancelled || _disposed) return;
    try {
      final resp = await _api
          .get('/api/v1/identity/remote-readiness')
          .timeout(const Duration(seconds: 10)) as Map<String, dynamic>;
      if (_pollCancelled || _disposed) return;
      final newRelay = resp['relay'] == true;
      final newCert = resp['cert'] == true;
      if (newRelay != _relayReady || newCert != _certReady) {
        _relayReady = newRelay;
        _certReady = newCert;
        notifyListeners();
      }

      if (resp['ready'] == true) {
        _pollCancelled = true;
        _readinessTimer = null;
        _redirectToRemote();
        return;
      }
    } on Object catch (_) {
      if (_pollCancelled || _disposed) return;
    }

    if (_readinessStopwatch.elapsed.inSeconds >= 180) {
      _readinessTimer = null;
      _fallbackToLanSetup();
      return;
    }
    _scheduleNextPoll();
  }

  void _redirectToRemote() {
    final uri = Uri(
      scheme: 'https',
      host: _pendingFqdn,
      path: '/',
      queryParameters:
          _pendingNonce != null ? {'setup_nonce': _pendingNonce} : null,
    );
    web.window.location.href = uri.toString();
  }

  void _fallbackToLanSetup() {
    _pendingFqdn = null;
    _pendingNonce = null;
    _pollCancelled = true;
    _remoteFallback = true;
    _step = FirstRunStep.credentials;
    _clearPreparingState();
    _persistSetupStep('credentials');
    notifyListeners();
  }

  void skipRemoteWait() {
    _readinessTimer?.cancel();
    _readinessTimer = null;
    _fallbackToLanSetup();
  }

  // --- Credentials ---

  Future<bool> submitCredentials(String password) async {
    try {
      _step = FirstRunStep.credentials; // ensure we're on this step for error routing
      _error = null;
      _setupPhase = SetupPhase.encrypting;
      notifyListeners();

      const timeout = Duration(seconds: 120);

      await _api.post('/api/v1/crypto/setup', body: {
        'password': password,
        if (_setupNonce != null) 'setup_nonce': _setupNonce,
      }).timeout(timeout);

      // Nonce consumed server-side.
      if (_setupNonce != null) {
        _setupNonce = null;
        try { web.window.sessionStorage.removeItem('setup_nonce'); } on Object catch (_) {}
      }

      _setupPhase = SetupPhase.creatingAdmin;
      notifyListeners();
      await _api.fetchCsrfToken().timeout(timeout);

      // Generate recovery key
      final words = await generateRecoveryKey();
      if (words != null) {
        _recoveryWords = words;
        _setupPhase = null;
        _step = FirstRunStep.recoveryKey;
      } else {
        // Timeout — setup succeeded but passkey may still be required.
        // Let the router re-check boot to determine the correct next step.
        _setupPhase = null;
        reRoute();
      }
      if (!_disposed) notifyListeners();
      return words != null;
    } on TimeoutException {
      _setupPhase = null;
      try {
        await _waitForSetupCompletion();
      } on Object catch (_) {
        _error = 'Setup is taking longer than expected. '
            'Your device may still be working \u2014 try refreshing in a minute.';
        _step = FirstRunStep.credentials;
        if (!_disposed) notifyListeners();
      }
      return false;
    } on ApiException catch (e) {
      _setupPhase = null;
      final code = extractErrorCode(e.message);
      if (isStorageSystemError(e)) {
        onSystemError(extractServerError(e.message));
      } else if (e.statusCode == 409 && code == 'setup_in_progress') {
        await _waitForSetupCompletion();
      } else if (e.statusCode == 409 && code == 'setup_complete') {
        await _recoverAfterSetupComplete();
      } else {
        _error = extractServerError(e.message);
        _step = FirstRunStep.credentials;
      }
      if (!_disposed) notifyListeners();
      return false;
    } on Object catch (e) {
      _setupPhase = null;
      final msg = e.toString();
      _error = msg.length > 200 ? 'Setup failed. Please try again.' : msg;
      _step = FirstRunStep.credentials;
      debugPrint('Unexpected in submitCredentials: ${e.runtimeType}: $e');
      if (!_disposed) notifyListeners();
      return false;
    }
  }

  /// Poll /crypto/status until setup completes.
  Future<void> _waitForSetupCompletion() async {
    _error = null;
    _setupPhase = SetupPhase.encrypting;
    if (!_disposed) notifyListeners();
    for (var i = 0; i < 60; i++) {
      await Future<void>.delayed(const Duration(seconds: 3));
      if (_disposed) return;
      try {
        final status = await _api.get('/api/v1/crypto/status')
            as Map<String, dynamic>;
        if (status['setup_in_progress'] != true) {
          debugPrint('Setup no longer in progress, recovering via boot');
          await _recoverAfterSetupComplete();
          return;
        }
      } on Object catch (_) {}
    }
    if (_disposed) return;
    _setupPhase = null;
    _error = 'Setup is taking longer than expected. Try refreshing.';
    _step = FirstRunStep.credentials;
    notifyListeners();
  }

  Future<void> _recoverAfterSetupComplete() async {
    _setupPhase = null;
    try {
      final boot = await _api.get('/api/v1/system/boot')
          as Map<String, dynamic>;
      final screen = boot['screen'] as String?;
      final rkPending = boot['recovery_key_pending'] == true;
      if (screen == 'desktop' || screen == 'passkey_required') {
        await _api.fetchCsrfToken();
        if (rkPending) {
          final words = await generateRecoveryKey();
          if (words != null) {
            _recoveryWords = words;
            _step = FirstRunStep.recoveryKey;
            if (!_disposed) notifyListeners();
            return;
          }
        }
        if (screen == 'passkey_required') {
          _step = FirstRunStep.passkeyRequired;
        } else {
          onComplete();
        }
      } else {
        // Boot returned login/unlock/other — let router re-route
        // with the correct controller.
        reRoute();
        return;
      }
    } on Object catch (e) {
      debugPrint('Recovery after setup complete failed: $e');
      _error = e.toString();
      _step = FirstRunStep.credentials;
    }
    if (!_disposed) notifyListeners();
  }

  // --- Recovery key proceed ---

  void proceedAfterRecovery() {
    if (isOnRemoteDomain()) {
      _step = FirstRunStep.passkeyRequired;
    } else {
      _step = FirstRunStep.security;
    }
    notifyListeners();
  }

  void completeSetup() {
    onComplete();
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

      await _api.finishPasskeyRegistration(sessionId, credential);
      onComplete();
      return true;
    } on ApiException catch (e) {
      _passkeyError = friendlyApiError(e);
      if (!_disposed) notifyListeners();
      return false;
    } on Object catch (e) {
      _passkeyError = friendlyPasskeyError(e);
      if (!_disposed) notifyListeners();
      return false;
    }
  }

  // --- sessionStorage helpers ---

  void _persistSetupStarted() {
    _persistSetupStep('1');
  }

  void _persistSetupStep(String step) {
    try { web.window.sessionStorage.setItem('setup_started', step); } on Object catch (_) {}
  }

  void _persistPreparingState() {
    try {
      if (_pendingFqdn != null) {
        web.window.sessionStorage.setItem('pending_fqdn', _pendingFqdn!);
      }
      if (_pendingNonce != null) {
        web.window.sessionStorage.setItem('pending_nonce', _pendingNonce!);
      }
    } on Object catch (_) {}
  }

  void _clearPreparingState() {
    try {
      web.window.sessionStorage.removeItem('pending_fqdn');
      web.window.sessionStorage.removeItem('pending_nonce');
    } on Object catch (_) {}
  }
}
