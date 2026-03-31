import 'dart:async';
import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/webauthn_service.dart';
import 'package:web/web.dart' as web;

/// Phases within the crypto setup operation (displayed by _FinishingStep).
enum SetupPhase { encrypting, creatingAdmin, generatingKey }

enum SetupState {
  loading, // Checking status
  onboarding, // USB boot: Try Piccolo / Install to Disk choice
  installDisk, // Disk selection + download + write progress
  installComplete, // Reboot prompt after successful install
  welcome, // First run intro
  remoteAddress, // Choose your address (between welcome and credentials)
  preparingRemote, // Waiting for relay + cert after hostname claim
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
    _parseSetupNonceFromUrl();
    unawaited(_checkStatus());
  }

  @override
  void dispose() {
    _pollCancelled = true;
    _readinessTimer?.cancel();
    super.dispose();
  }

  SetupState _state = SetupState.loading;
  SetupState get state => _state;

  // True when this wizard session started on an uninitialized device.
  // Used by the Desktop shell to show first-run UX only once.
  bool _isFirstSetupFlow = false;
  bool get isFirstSetupFlow => _isFirstSetupFlow;

  String? _error;
  String? get error => _error;

  SetupPhase? _setupPhase;
  SetupPhase? get setupPhase => _setupPhase;

  // Guard against double recovery key generation — each call creates new
  // entropy and overwrites LUKS keyslot 2, silently invalidating any
  // previously displayed words.
  bool _recoveryKeyGenerated = false;

  // Server-authoritative signal: recovery key has not been generated yet
  // on a post-setup device. Set from boot response, consumed in routing.
  bool _recoveryKeyPending = false;

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

  // Namek enrollment state (RFC 20260328)
  bool _namekEnrolled = false;
  bool get namekEnrolled => _namekEnrolled;

  String? _namekBaseDomain;
  String? get namekBaseDomain => _namekBaseDomain;

  String? _setupNonce; // from URL on remote domain, sent in crypto/setup body

  // True when the setup wizard is running on the remote domain (after redirect).
  // Determined by origin detection, not token presence.
  bool _isRemoteSetup = false;
  bool get isRemoteSetup => _isRemoteSetup;

  bool _settingHostname = false;
  bool get settingHostname => _settingHostname;

  String? _hostnameError;
  String? get hostnameError => _hostnameError;

  // Remote readiness polling state (between hostname claim and redirect).
  bool _relayReady = false;
  bool get relayReady => _relayReady;
  bool _certReady = false;
  bool get certReady => _certReady;
  bool _remoteFallback = false;
  bool get remoteFallback => _remoteFallback;
  String? _pendingFqdn;
  String? _pendingNonce;
  Timer? _readinessTimer;

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
    } on FormatException catch (e) {
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
    } on FormatException catch (e) {
      debugPrint('Failed to parse next URL: $e');
    }
  }

  void _parseInviteFromUrl() {
    try {
      final token = Uri.base.queryParameters['invite'];
      if (token != null && token.isNotEmpty) {
        _inviteToken = token;
      }
    } on FormatException catch (_) {}
  }

  /// Parse setup nonce from URL or sessionStorage (for remote setup redirect).
  /// Persists to sessionStorage so it survives page refreshes, then strips
  /// the nonce from the URL bar via replaceState to reduce exposure.
  void _parseSetupNonceFromUrl() {
    try {
      // Check URL first (initial redirect), then sessionStorage (page refresh).
      final nonce = Uri.base.queryParameters['setup_nonce'];
      if (nonce != null && nonce.isNotEmpty) {
        _setupNonce = nonce;
        // Persist in sessionStorage for page refresh survival.
        web.window.sessionStorage.setItem('setup_nonce', nonce);
        // Strip nonce from URL to avoid exposure in browser history/logs.
        final cleanUri = Uri.base.replace(
          queryParameters: Map<String, String>.from(Uri.base.queryParameters)
            ..remove('setup_nonce'),
        );
        web.window.history.replaceState(null, '', cleanUri.toString());
      } else {
        // Page refresh: recover nonce from sessionStorage.
        final stored = web.window.sessionStorage.getItem('setup_nonce');
        if (stored != null && stored.isNotEmpty) {
          _setupNonce = stored;
        }
      }
    } on Object catch (_) {
      // Catches both FormatException (Uri.base) and DOMException
      // (sessionStorage/replaceState when storage is disabled).
    }
  }

  /// Detect if we're on the remote domain (HTTPS, not .local, not localhost, not an IP).
  bool get _isOnRemoteDomain {
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

  /// Persist current setup step to sessionStorage (best-effort).
  /// sessionStorage is origin-scoped, so LAN and remote cannot interfere.
  void _persistSetupStep() {
    try {
      web.window.sessionStorage.setItem('setup_step', _state.name);
      if (_state == SetupState.preparingRemote) {
        if (_pendingFqdn != null) {
          web.window.sessionStorage.setItem('pending_fqdn', _pendingFqdn!);
        }
        if (_pendingNonce != null) {
          web.window.sessionStorage.setItem('pending_nonce', _pendingNonce!);
        }
      }
    } on Object catch (_) {}
  }

  /// Clear persisted setup step and auxiliary data from sessionStorage.
  /// Note: setup_nonce is NOT cleared here — it has its own lifecycle
  /// (cleared in submitCredentials after server-side consumption) and must
  /// survive across retries and refresh-after-timeout on the remote domain.
  void _clearPersistedSetupStep() {
    try {
      web.window.sessionStorage.removeItem('setup_step');
      web.window.sessionStorage.removeItem('pending_fqdn');
      web.window.sessionStorage.removeItem('pending_nonce');
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

      // Detect remote setup before the switch — early-return branches
      // (setup_in_progress waits) need _isRemoteSetup set so
      // proceedAfterRecovery() routes correctly after recovery key display.
      if (_isOnRemoteDomain && _setupNonce != null) {
        _isRemoteSetup = true;
      }

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
          _namekEnrolled = boot['namek_enrolled'] == true;
          _namekBaseDomain = boot['namek_base_domain'] as String?;

          if (_enterSetupWaitIfNeeded(boot)) return;

          // Remote continuation: on the remote domain with a valid nonce from LAN.
          // Skip to credentials (password step) — address was claimed on LAN.
          if (_isRemoteSetup) {
            _state = SetupState.credentials;
          } else {
            _state = SetupState.welcome;
            // Restore LAN step position from sessionStorage (page refresh).
            try {
              final saved = web.window.sessionStorage.getItem('setup_step');
              if (saved != null) {
                final restored = SetupState.values
                    .where((s) => s.name == saved)
                    .firstOrNull;
                if (restored == SetupState.remoteAddress ||
                    restored == SetupState.credentials) {
                  _state = restored!;
                } else if (restored == SetupState.preparingRemote) {
                  final fqdn = web.window.sessionStorage
                      .getItem('pending_fqdn');
                  final nonce = web.window.sessionStorage
                      .getItem('pending_nonce');
                  if (fqdn != null && fqdn.isNotEmpty) {
                    _pendingFqdn = fqdn;
                    _pendingNonce = nonce;
                    _state = SetupState.preparingRemote;
                    _startReadinessPolling();
                  } else {
                    _state = SetupState.remoteAddress;
                  }
                }
              }
            } on Object catch (_) {}
          }

        case 'unlock':
          _clearPersistedSetupStep();
          _recoveryKeyPending = boot['recovery_key_pending'] == true;
          if (_enterSetupWaitIfNeeded(boot)) return;
          _state = SetupState.unlock;

        case 'login':
          _recoveryKeyPending = boot['recovery_key_pending'] == true;
          if (_enterSetupWaitIfNeeded(boot)) return;
          if (_inviteToken != null) {
            _state = SetupState.invite;
          } else {
            _state = SetupState.login;
          }

        case 'passkey_required':
          await _api.fetchCsrfToken();
          if (boot['recovery_key_pending'] == true &&
              await _generateAndShowRecoveryKey()) {
            return;
          }
          _state = SetupState.passkeyRequired;

        case 'desktop':
          await _api.fetchCsrfToken();

          // First-run: show recovery key before anything else.
          if (boot['recovery_key_pending'] == true &&
              await _generateAndShowRecoveryKey()) {
            return;
          }

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
    _state = SetupState.remoteAddress;
    _persistSetupStep();
    notifyListeners();
  }

  /// Re-check boot response to detect if auto-enrollment completed
  /// while the user was on the welcome screen.
  Future<void> refreshEnrollmentStatus() async {
    try {
      final boot = await _api.get('/api/v1/system/boot') as Map<String, dynamic>;
      if (boot['screen'] == 'setup') {
        _namekEnrolled = boot['namek_enrolled'] == true;
        _namekBaseDomain = boot['namek_base_domain'] as String?;
        notifyListeners();
      }
    } on Object catch (_) {}
  }

  /// Claim a custom hostname on namek and start polling for remote readiness
  /// before redirecting to the remote domain.
  Future<bool> submitHostname(String hostname) async {
    // Cancel any in-flight readiness poll from a previous claim.
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
      _pendingFqdn = fqdn ?? '$hostname.$baseDomain';
      _pendingNonce = nonce;
      _relayReady = false;
      _certReady = false;
      _state = SetupState.preparingRemote;
      _persistSetupStep();
      notifyListeners();

      _startReadinessPolling();
      return true;
    } on ApiException catch (e) {
      _hostnameError = _extractServerError(e.message);
      _settingHostname = false;
      notifyListeners();
      return false;
    } on Object catch (e) {
      _hostnameError = e.toString();
      _settingHostname = false;
      notifyListeners();
      return false;
    }
  }

  bool _pollCancelled = false;
  final _readinessStopwatch = Stopwatch();

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
    if (_pollCancelled) return;
    try {
      final resp = await _api
          .get('/api/v1/identity/remote-readiness')
          .timeout(const Duration(seconds: 10)) as Map<String, dynamic>;
      if (_pollCancelled) return;
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
      // Polling failure (including timeout) is non-fatal; keep trying.
      if (_pollCancelled) return;
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
    _state = SetupState.credentials;
    _persistSetupStep();
    notifyListeners();
  }

  /// Skip the remote waiting and continue setup on LAN.
  void skipRemoteWait() {
    _readinessTimer?.cancel();
    _readinessTimer = null;
    _fallbackToLanSetup();
  }

  /// Skip the remote address step and continue setup on LAN.
  void skipRemoteAddress() {
    _state = SetupState.credentials;
    _persistSetupStep();
    notifyListeners();
  }

  /// Go back from credentials to the remote address step.
  void backToRemoteAddress() {
    _state = SetupState.remoteAddress;
    _error = null;
    _remoteFallback = false;
    _persistSetupStep();
    notifyListeners();
  }

  /// Checks if an API exception represents a storage system error.
  /// Covers LUKS data volume failures (500) and emergency middleware blocks (503).
  bool _isStorageSystemError(ApiException e) {
    // Prefer structured error code matching.
    final code = _extractErrorCode(e.message);
    if (code == 'storage_init_failed' ||
        code == 'storage_unlock_failed' ||
        code == 'storage_emergency') {
      return true;
    }
    // Legacy fallback for older backends without code field.
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

  /// Extract a structured error code from a JSON error response body.
  String? _extractErrorCode(String body) {
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
  String _extractServerError(String body) {
    try {
      final decoded = jsonDecode(body);
      if (decoded is Map) {
        if (decoded['message'] is String) return decoded['message'] as String;
        if (decoded['error'] is String) return decoded['error'] as String;
      }
    } on Object catch (_) {}
    // Guard against raw HTML or overly long error bodies.
    if (body.length > 200 || body.contains('<html>')) {
      return 'An unexpected error occurred. Please try again.';
    }
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

  /// Generate and display recovery key. Returns true on success (state is
  /// now [SetupState.recovery]), false if skipped or timed out. On timeout,
  /// sets [_error] but does NOT mutate [_state] — callers pick their own
  /// fallback. Guarded against double invocation (each call creates new
  /// entropy and overwrites LUKS keyslot 2).
  /// ApiException and other errors propagate to the caller.
  Future<bool> _generateAndShowRecoveryKey() async {
    if (_recoveryKeyGenerated) return false;
    _setupPhase = SetupPhase.generatingKey;
    notifyListeners();
    try {
      final recoveryData = await _api.post(
        '/api/v1/crypto/recovery-key/generate',
        body: <String, dynamic>{},
      ).timeout(const Duration(seconds: 120)) as Map<String, dynamic>?;
      if (recoveryData != null && recoveryData['words'] != null) {
        _recoveryWords = List<String>.from(
            recoveryData['words'] as Iterable<dynamic>);
        _recoveryKeyGenerated = true;
        _setupPhase = null;
        _state = SetupState.recovery;
        _clearPersistedSetupStep();
        return true;
      } else {
        throw Exception('Failed to generate recovery key');
      }
    } on TimeoutException {
      _setupPhase = null;
      _error = 'Recovery key generation timed out. '
          'You can generate one later from Settings.';
      return false;
    } on ApiException catch (e) {
      _setupPhase = null;
      if (e.statusCode == 403) {
        // Not authorized (non-admin user) — skip silently.
        return false;
      }
      rethrow;
    }
  }

  /// If boot indicates setup is in progress, enter the finishing/wait state.
  /// Returns true if waiting was started (caller should return early).
  bool _enterSetupWaitIfNeeded(Map<String, dynamic> boot) {
    if (boot['setup_in_progress'] != true) return false;
    _isFirstSetupFlow = true; // setup handler running = first-run
    _state = SetupState.finishing;
    _setupPhase = SetupPhase.encrypting;
    notifyListeners();
    unawaited(_waitForSetupCompletion());
    return true;
  }

  /// Poll /crypto/status until setup handler releases the mutex, then
  /// recover via boot. Ceiling: 3 minutes (setup takes 30-120s on typical
  /// hardware; 10min server-side budget is for extreme ARM/eMMC devices
  /// where a refresh-and-retry is acceptable UX).
  Future<void> _waitForSetupCompletion() async {
    _error = null;
    // Callers set _state/phase before invoking; only notify if not already
    // in the finishing state (avoids redundant rebuild).
    if (_state != SetupState.finishing) {
      _state = SetupState.finishing;
      _setupPhase = SetupPhase.encrypting;
      notifyListeners();
    }
    for (var i = 0; i < 60; i++) {
      await Future<void>.delayed(const Duration(seconds: 3));
      try {
        final status = await _api.get('/api/v1/crypto/status')
            as Map<String, dynamic>;
        if (status['setup_in_progress'] != true) {
          debugPrint('Setup no longer in progress, recovering via boot');
          await _recoverAfterSetupComplete();
          return;
        }
      } on Object catch (_) {
        // Polling failure (network, timeout) is non-fatal; keep trying.
      }
    }
    _setupPhase = null;
    _error = 'Setup is taking longer than expected. Try refreshing.';
    _state = SetupState.credentials;
    notifyListeners();
  }

  /// After setup is known to be complete (or no longer in progress),
  /// check boot for session state and route accordingly.
  /// Uses server-authoritative `recovery_key_pending` to decide whether
  /// to generate a recovery key, instead of always generating.
  Future<void> _recoverAfterSetupComplete() async {
    _setupPhase = null;
    try {
      final boot = await _api.get('/api/v1/system/boot')
          as Map<String, dynamic>;
      final screen = boot['screen'] as String?;
      final rkPending = boot['recovery_key_pending'] == true;
      if (screen == 'desktop' || screen == 'passkey_required') {
        await _api.fetchCsrfToken();
        if (!rkPending || !await _generateAndShowRecoveryKey()) {
          _state = screen == 'passkey_required'
              ? SetupState.passkeyRequired
              : SetupState.complete;
        }
      } else if (screen == 'login') {
        _recoveryKeyPending = rkPending;
        _state = SetupState.login;
      } else if (screen == 'unlock') {
        _recoveryKeyPending = rkPending;
        _state = SetupState.unlock;
      } else {
        _state = SetupState.credentials;
        _error = null;
      }
    } on Object catch (e) {
      debugPrint('Recovery after setup complete failed: $e');
      _error = e.toString();
      _state = SetupState.error;
    }
    notifyListeners();
  }

  Future<bool> submitCredentials(String password) async {
    try {
      _state = SetupState.finishing;
      _error = null;
      _setupPhase = SetupPhase.encrypting;
      notifyListeners();

      const timeout = Duration(seconds: 120);

      // 1. Initialize Crypto (also unlocks, sets up auth, creates admin user, creates session)
      await _api.post('/api/v1/crypto/setup', body: {
        'password': password,
        if (_setupNonce != null) 'setup_nonce': _setupNonce,
      }).timeout(timeout);

      // Nonce consumed server-side. Clear client-side storage.
      if (_setupNonce != null) {
        _setupNonce = null;
        try { web.window.sessionStorage.removeItem('setup_nonce'); } on Object catch (_) {}
      }

      // 2. Fetch CSRF Token
      _setupPhase = SetupPhase.creatingAdmin;
      notifyListeners();
      await _api.fetchCsrfToken().timeout(timeout);

      // 3. Generate Recovery Key
      final ok = await _generateAndShowRecoveryKey();
      if (!ok) {
        // Timeout — setup succeeded but recovery key can be generated later.
        _state = SetupState.complete;
      }
      notifyListeners();
      return ok;
    } on TimeoutException {
      _setupPhase = null;
      // Client timeout does not mean server-side setup finished — the server
      // has a 10-minute budget. Poll setup_in_progress before recovering,
      // otherwise we may route to login/unlock while setupMu is still held.
      try {
        await _waitForSetupCompletion();
      } on Object catch (_) {
        _error = 'Setup is taking longer than expected. '
            'Your device may still be working \u2014 try refreshing in a minute.';
        _state = SetupState.credentials;
        notifyListeners();
      }
      return false;
    } on ApiException catch (e) {
      _setupPhase = null;
      final code = _extractErrorCode(e.message);
      if (_isStorageSystemError(e)) {
        _error = _extractServerError(e.message);
        _state = SetupState.systemError;
      } else if (e.statusCode == 409 && code == 'setup_in_progress') {
        await _waitForSetupCompletion();
      } else if (e.statusCode == 409 && code == 'setup_complete') {
        await _recoverAfterSetupComplete();
      } else {
        _error = _extractServerError(e.message);
        _state = SetupState.credentials;
      }
      notifyListeners();
      return false;
    } on Object catch (e) {
      _setupPhase = null;
      final msg = e.toString();
      _error = msg.length > 200 ? 'Setup failed. Please try again.' : msg;
      _state = SetupState.credentials;
      debugPrint('Unexpected in submitCredentials: ${e.runtimeType}: $e');
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
      debugPrint('Unexpected in unlock: ${e.runtimeType}: $e');
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
    if (_recoveryKeyPending) _isFirstSetupFlow = true; // UI stepper
    _state = SetupState.finishing;
    _setupPhase = SetupPhase.encrypting;
    notifyListeners();
    try {
      await _api.post('/api/v1/crypto/setup', body: {'password': password});
    } on ApiException catch (e) {
      final code = _extractErrorCode(e.message);
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
      if (!await _generateAndShowRecoveryKey()) {
        // Timeout or guard — route to login so user can proceed.
        _state = SetupState.login;
      }
    } on Object catch (e) {
      // Unlock+setup succeeded but recovery key generation failed.
      // Route to login rather than leaving user stuck on unlock screen.
      debugPrint('Recovery key after partial setup failed: $e');
      _setupPhase = null;
      _state = SetupState.login;
    }
    notifyListeners();
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
    } on ApiException catch (e) {
      debugPrint('Login failed: $e');
      _error = _extractServerError(e.message);
      notifyListeners();
      return false;
    } on Object catch (e) {
      debugPrint('Unexpected in login: ${e.runtimeType}: $e');
      final msg = e.toString();
      _error = msg.length > 200 ? 'Login failed. Please try again.' : msg;
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

    // First-run: generate recovery key before any routing (passkey, OIDC,
    // redirect). Uses server-authoritative _recoveryKeyPending from boot.
    // Safe before OIDC: OIDC providers can't be configured during first-run.
    if (_recoveryKeyPending && await _generateAndShowRecoveryKey()) return;

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

  void proceedAfterRecovery() {
    if (!_isFirstSetupFlow) {
      // Non-first-run: recovery key was an interruption during a normal
      // login. Re-check boot to resume the correct flow (passkey_required,
      // OIDC redirect, next URL, or desktop). In-memory state like
      // _authRequestId and _nextUrl survives from initial URL parsing.
      unawaited(_checkStatus());
      return;
    } else if (_isOnRemoteDomain) {
      // First-run remote: passkey registration (correct RP ID).
      _state = SetupState.passkeyRequired;
    } else {
      // First-run LAN: CA certificate download for HTTPS.
      _state = SetupState.security;
    }
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
    for (var attempt = 0; attempt < 3; attempt++) {
      try {
        final result = await _api
            .getLoginOptions()
            .timeout(const Duration(seconds: 5));
        _loginMethods = List<String>.from(result['methods'] as List);
        notifyListeners();
        return;
      } on Object catch (_) {
        if (attempt < 2) {
          await Future<void>.delayed(Duration(seconds: 1 << attempt));
        }
      }
    }
    _loginMethods = ['password'];
    notifyListeners();
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

      // First-run: generate recovery key before any routing.
      if (_recoveryKeyPending) {
        if (await _generateAndShowRecoveryKey()) {
          notifyListeners();
          return true;
        }
      }

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
