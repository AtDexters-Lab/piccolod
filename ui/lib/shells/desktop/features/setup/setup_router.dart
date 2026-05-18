import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/error_reporter.dart';
import 'package:piccolo_os/shells/desktop/features/setup/controllers/auth_controller.dart';
import 'package:piccolo_os/shells/desktop/features/setup/controllers/first_run_controller.dart';
import 'package:piccolo_os/shells/desktop/features/setup/controllers/invite_controller.dart';
import 'package:piccolo_os/shells/desktop/features/setup/controllers/onboarding_controller.dart';
import 'package:piccolo_os/shells/desktop/features/setup/install_disk_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/onboarding_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/setup_utils.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/unlocking_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/credentials_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/error_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/finishing_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/forgot_password_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/install_complete_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/invite_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/login_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/other_devices_panel.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/passkey_required_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/preparing_remote_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/recovery_key_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/remote_address_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/security_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/system_error_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/unlock_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/steps/welcome_step.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:web/web.dart' as web;

class SetupRouter extends StatefulWidget {
  const SetupRouter({required this.onComplete, super.key});
  // Matches DesktopController.completeSetup callback signature.
  // ignore: avoid_positional_boolean_parameters
  final void Function(bool isFirstSetupFlow) onComplete;

  @override
  State<SetupRouter> createState() => _SetupRouterState();
}

class _SetupRouterState extends State<SetupRouter> {
  final ApiClient _api = ApiClient();
  bool _loading = true;
  String? _bootError;

  // Parsed URL parameters (once, at mount time).
  String? _authRequestId;
  String? _nextUrl;
  String? _inviteToken;
  String? _setupNonce;

  // Active controller (only one at a time).
  ChangeNotifier? _controller;

  // Router-level guards.
  bool _recoveryKeyGenerated = false;
  // Tracks if setup-in-progress polling originated from a first-run setup screen,
  // so the welcome/HTTPS upgrade is shown after the poll completes.
  bool _firstSetupDetected = false;

  // Direct-render state (no controller needed).
  String? _systemError;
  bool _showFinishing = false;
  SetupPhase? _finishingPhase;
  bool _showUnlocking = false;
  Timer? _autoUnlockingPoll;
  // Sticky for one route after the in-flight spinner clears. Threaded into
  // UnlockStep so it shows a neutral state-the-action prompt instead of
  // landing the operator on a bare password field with no bridging signal.
  bool _recentlyTriedUnlock = false;

  // Boot data for install_complete.
  bool _bootOrderConfigured = false;

  @override
  void initState() {
    super.initState();
    _parseUrlParams();
    unawaited(_checkBoot());
  }

  @override
  void dispose() {
    _autoUnlockingPoll?.cancel();
    _controller?.dispose();
    super.dispose();
  }

  void _parseUrlParams() {
    try {
      final params = Uri.base.queryParameters;
      _authRequestId = params['id'];
      _nextUrl = params['next'];
      _inviteToken = params['invite'];

      // Setup nonce: URL first, then sessionStorage.
      final nonce = params['setup_nonce'];
      if (nonce != null && nonce.isNotEmpty) {
        _setupNonce = nonce;
        try {
          web.window.sessionStorage.setItem('setup_nonce', nonce);
          final cleanUri = Uri.base.replace(
            queryParameters: Map<String, String>.from(params)
              ..remove('setup_nonce'),
          );
          web.window.history.replaceState(null, '', cleanUri.toString());
        } on Object catch (_) {}
      } else {
        try {
          final stored = web.window.sessionStorage.getItem('setup_nonce');
          if (stored != null && stored.isNotEmpty) _setupNonce = stored;
        } on Object catch (_) {}
      }
    } on Object catch (_) {}
  }

  Future<void> _checkBoot() async {
    setState(() {
      _loading = true;
      _bootError = null;
    });
    try {
      final boot = await _api.get('/api/v1/system/boot') as Map<String, dynamic>;
      _route(boot);
    } on Object catch (e) {
      setState(() {
        _loading = false;
        _bootError = e.toString();
      });
    }
  }

  void _route(Map<String, dynamic> boot) {
    final screen = boot['screen'] as String?;
    debugPrint('SetupRouter: boot screen=$screen');
    // Auto-unlock-just-failed bridging signal stays sticky only across
    // the spinner→unlock transition. Once we route to anything other
    // than 'unlock' (login, desktop, etc.), clear it so a later re-lock
    // doesn't inherit the signal.
    if (screen != 'unlock') {
      _recentlyTriedUnlock = false;
    }

    // Clear setup state on non-setup screens (factory reset, setup by other user).
    if (screen != 'setup') {
      try {
        web.window.sessionStorage.removeItem('setup_started');
        web.window.sessionStorage.removeItem('pending_fqdn');
        web.window.sessionStorage.removeItem('pending_nonce');
      } on Object catch (_) {}
    }

    // Dispose old controller before creating new one.
    _controller?.dispose();
    _controller = null;
    _systemError = null;
    _showFinishing = false;
    // If we were just showing the auto-unlocking spinner and are now
    // routing somewhere else (typically UnlockStep on failure), mark the
    // sticky flag so the next render carries the bridging signal.
    if (_showUnlocking) {
      _recentlyTriedUnlock = true;
    }
    _showUnlocking = false;
    _autoUnlockingPoll?.cancel();
    _autoUnlockingPoll = null;

    switch (screen) {
      case 'emergency':
        _systemError = boot['error'] as String? ?? 'Storage emergency mode';

      case 'onboarding':
        _controller = OnboardingController(
          onComplete: () => unawaited(_checkBoot()),
          bootMode: boot['boot_mode'] as String?,
          bootOrderConfigured: boot['boot_order_configured'] == true,
          installAbandoned: boot['install_abandoned'] == true,
        )..addListener(_onControllerChanged);

      case 'install_progress':
        _controller = OnboardingController(
          onComplete: () => unawaited(_checkBoot()),
          installTaskId: boot['install_task_id'] as String?,
        )..addListener(_onControllerChanged);

      case 'install_complete':
        _bootOrderConfigured = boot['boot_order_configured'] == true;

      case 'setup':
        _routeSetup(boot);

      case 'unlock':
        if (boot['setup_in_progress'] == true) {
          _firstSetupDetected = true;
          _startSetupInProgressPolling();
        } else if (boot['lifecycle'] == 'unlocking') {
          // The post-decrypt chain is in flight (manual unlock OR auto-
          // unlock pickup). Show the spinner instead of the password
          // prompt and poll boot until the lifecycle resolves. On success
          // the device transitions to screen=login/desktop so the next
          // _route picks up the right destination; on failure lifecycle
          // returns to "locked" → password prompt with no callout.
          _startUnlockingPoll();
        } else {
          if (boot['recovery_key_pending'] == true) _firstSetupDetected = true;
          _createAuthController(
            AuthMode.unlock,
            recoveryKeyPending: boot['recovery_key_pending'] == true,
          );
        }

      case 'login':
        if (boot['setup_in_progress'] == true) {
          _firstSetupDetected = true;
          _startSetupInProgressPolling();
        } else if (_inviteToken != null) {
          _createInviteController();
        } else {
          _createAuthController(
            AuthMode.login,
            recoveryKeyPending: boot['recovery_key_pending'] == true,
          );
        }

      case 'passkey_required':
        final rkPending = boot['recovery_key_pending'] == true;
        if (rkPending) {
          // Async — handler manages its own loading state.
          unawaited(_handleRecoveryKeyThenPasskey());
          return;
        }
        _createAuthController(AuthMode.passkeyRequired);

      case 'desktop':
        // All desktop sub-paths either redirect, complete, or do async work.
        _routeDesktop(boot);
        return;

      default:
        _bootError = 'Unexpected server state';
    }

    setState(() => _loading = false);
  }

  void _routeSetup(Map<String, dynamic> boot) {
    if (boot['setup_in_progress'] == true) {
      _firstSetupDetected = true;
      _startSetupInProgressPolling();
      return;
    }

    // Invite tokens are only valid on the login screen (device must be set up first).

    final namekEnrolled = boot['namek_enrolled'] == true;
    final baseDomain = boot['namek_base_domain'] as String?;
    final suggestedHostname = boot['namek_suggested_hostname'] as String?;
    // Null from a legacy backend → 'unavailable' per the field contract;
    // normalize at the boundary so downstream gates don't have to.
    final namekStatus = boot['namek_status'] as String? ?? 'unavailable';
    final isRemote = isOnRemoteDomain() && _setupNonce != null;

    final ctrl = FirstRunController(
      isRemote: isRemote,
      namekEnrolled: namekEnrolled,
      namekBaseDomain: baseDomain,
      namekSuggestedHostname: suggestedHostname,
      namekStatus: namekStatus,
      onComplete: _onControllerComplete,
      reRoute: () => unawaited(_checkBoot()),
      onSystemError: (err) => setState(() => _systemError = err),
      generateRecoveryKey: _generateRecoveryKey,
      setupNonce: _setupNonce,
    );

    // Restore state from sessionStorage on LAN refresh.
    if (!isRemote) {
      try {
        final savedStep = web.window.sessionStorage.getItem('setup_started');
        if (savedStep != null) {
          // Check if we were mid-preparing (waiting for relay+cert).
          final fqdn = web.window.sessionStorage.getItem('pending_fqdn');
          if (fqdn != null && fqdn.isNotEmpty) {
            final nonce = web.window.sessionStorage.getItem('pending_nonce');
            ctrl.restorePreparingRemote(fqdn, nonce);
          } else if (savedStep == 'credentials') {
            // User was on credentials step (skipped address or fallback).
            ctrl.skipRemoteAddress();
          } else {
            // User saw welcome — skip to address step.
            ctrl.startSetup();
          }
        }
      } on Object catch (_) {}
    }

    _controller = ctrl..addListener(_onControllerChanged);
  }

  void _routeDesktop(Map<String, dynamic> boot) {
    if (boot['recovery_key_pending'] == true) {
      unawaited(_handleRecoveryKeyThenComplete());
      return;
    }
    // Keep loading=true while OIDC/next redirect is in-flight; the helper is
    // a no-op fall-through to onComplete when neither path is set.
    if (_authRequestId != null || (_nextUrl != null && _nextUrl!.isNotEmpty)) {
      _loading = true;
    }
    _resumeOIDCOrComplete();
  }

  void _createAuthController(AuthMode mode, {bool recoveryKeyPending = false}) {
    _controller = AuthController(
      mode: mode,
      onComplete: _onControllerComplete,
      reRoute: () => unawaited(_checkBoot()),
      onSystemError: (err) => setState(() => _systemError = err),
      recoveryKeyPending: recoveryKeyPending,
      authRequestId: _authRequestId,
      nextUrl: _nextUrl,
      generateRecoveryKey: _generateRecoveryKey,
    )..addListener(_onControllerChanged);
  }

  void _createInviteController() {
    _controller = InviteController(
      token: _inviteToken!,
      onComplete: _onControllerComplete,
    )..addListener(_onControllerChanged);
  }

  void _onControllerChanged() {
    if (mounted) setState(() {});
  }

  void _onControllerComplete() {
    if (!mounted) return;
    final isFirst = _firstSetupDetected ||
        _controller is FirstRunController;
    try { web.window.sessionStorage.removeItem('setup_started'); } on Object catch (_) {}
    widget.onComplete(isFirst);
  }

  // --- Setup-in-progress polling (router-level) ---

  void _startSetupInProgressPolling() {
    _showFinishing = true;
    _finishingPhase = SetupPhase.encrypting;
    unawaited(_pollSetupInProgress());
  }

  void _startUnlockingPoll() {
    _showUnlocking = true;
    _autoUnlockingPoll?.cancel();
    _autoUnlockingPoll = Timer.periodic(const Duration(seconds: 1), (_) {
      if (!mounted) return;
      unawaited(_checkBoot());
    });
  }

  Future<void> _pollSetupInProgress() async {
    final completed = await pollSetupCompletion(isCancelled: () => !mounted);
    if (!mounted) return;
    if (completed) {
      debugPrint('Setup no longer in progress, re-checking boot');
      await _checkBoot();
      return;
    }
    setState(() {
      _showFinishing = false;
      _bootError = 'Setup is taking longer than expected. Try refreshing.';
    });
  }

  // --- Recovery key generation (router-level guard) ---

  Future<RecoveryKeyMaterial?> _generateRecoveryKey() async {
    if (_recoveryKeyGenerated) return null;
    try {
      final recoveryData = await _api.post(
        '/api/v1/crypto/recovery-key/generate',
        body: <String, dynamic>{},
      ).timeout(const Duration(seconds: 120)) as Map<String, dynamic>?;
      if (recoveryData != null && recoveryData['words'] != null) {
        _recoveryKeyGenerated = true;
        // REC-4: server is required to return key_id alongside words. Empty
        // key_id surfaces as a hard skip — without it, /ack will 400 and the
        // operator gets re-prompted on next reload, which is the correct
        // fail-closed posture for a contract violation.
        final keyId = (recoveryData['key_id'] as String?) ?? '';
        if (keyId.isEmpty) {
          ErrorReporter().report(
            type: 'recovery_key_generate_missing_key_id',
            message: '/generate response missing key_id; ack will fail',
            flushImmediate: true,
          );
          return null;
        }
        return RecoveryKeyMaterial(
          words: List<String>.from(recoveryData['words'] as Iterable<dynamic>),
          keyId: keyId,
          // REC-3: server returns rotated=true when this generation replaced
          // an unack'd previous key. Default-false so older builds (no field)
          // render the standard chrome rather than spuriously banner.
          wasRotated: recoveryData['rotated'] == true,
        );
      }
      return null;
    } on TimeoutException {
      return null;
    } on ApiException catch (e) {
      if (e.statusCode == 403) return null; // Not admin — skip silently.
      // 409 recovery_key_already_acked: server refuses to rotate an
      // already-acknowledged key without explicit force_rotate. The gate
      // that brought us here was a false positive (transient DB error,
      // race, etc.) — skip the recovery-key step and let the caller
      // route forward. Mark generated so we don't retry on this controller.
      if (isApiError(e, 409, ServerErrorCode.recoveryKeyAlreadyAcked)) {
        // Telemetry: the gate told us pending=true but the server holds an
        // ack — false-positive on the gate (transient err, race, etc.). The
        // user sees no recovery-key screen on this skip, so observability
        // is the only signal that the gate's confidence was wrong.
        ErrorReporter().report(
          type: 'recovery_key_already_acked',
          message: 'pending-true but server has ack; skipped silently',
          flushImmediate: true,
        );
        _recoveryKeyGenerated = true;
        return null;
      }
      // 503 ack_state_unknown: server couldn't read the ack state to verify
      // rotation safety. Treat as transient skip — gate re-evaluates next
      // sign-in. Without this catch, password/passkey login flows that
      // routed here on a stale pending=true surface the 503 as an auth
      // failure even though the session was already created.
      if (isApiError(e, 503, ServerErrorCode.ackStateUnknown)) {
        ErrorReporter().report(
          type: 'recovery_key_ack_state_unknown',
          message: 'transient staleness read failure; recovery skipped',
          flushImmediate: true,
        );
        return null;
      }
      rethrow;
    }
  }

  /// Generate recovery key → show → then passkey required.
  Future<void> _handleRecoveryKeyThenPasskey() async {
    try {
      await _api.fetchCsrfToken();
      final material = await _generateRecoveryKey();
      if (material != null && mounted) {
        _pendingRecoveryMaterial = material;
        // postLogin even when _firstSetupDetected: this direct-render path
        // has no stepper (unlike FirstRunController), and postSetup would
        // emit a null subtitle leaving the screen with no bridging copy at
        // all. The postLogin subtitle ("save your recovery key — it's how
        // you'll reset your password") is grammatically correct for a
        // post-auth landing whether the operator came from setup or login.
        _pendingRecoveryContext = RecoveryKeyEntryContext.postLogin;
        final keyId = material.keyId;
        _pendingPostRecoveryAction = () {
          // RFC 20260510 §Frontend ack-await fix: await the ack BEFORE
          // transitioning to the passkey screen — a fire-and-forget ack
          // races the route transition and may never reach the server,
          // leaving RecoveryAckAt=zero and the operator re-prompted with
          // rotated words on next sign-in. Same pattern as
          // _handleRecoveryKeyThenComplete's awaited site below.
          unawaited(() async {
            await ackRecoveryKey(keyId);
            if (!mounted) return;
            _clearPendingRecovery();
            _createAuthController(AuthMode.passkeyRequired);
            setState(() {});
          }());
        };
        setState(() => _loading = false);
        return;
      }
    } on Object catch (e) {
      debugPrint('Recovery key before passkey failed: $e');
    }
    if (!mounted) return;
    _createAuthController(AuthMode.passkeyRequired);
    setState(() => _loading = false);
  }

  // Hold material intact rather than unpacking — same shape as the
  // controller-level capture.
  RecoveryKeyMaterial? _pendingRecoveryMaterial;
  RecoveryKeyEntryContext _pendingRecoveryContext =
      RecoveryKeyEntryContext.postLogin;
  VoidCallback? _pendingPostRecoveryAction;

  void _clearPendingRecovery() {
    _pendingRecoveryMaterial = null;
    // Reset to default rather than leave the last-flow value behind — keeps
    // the gate (`_pendingRecoveryMaterial != null`) and the context in lock-
    // step so a future code path that reads context outside that gate cannot
    // see stale data.
    _pendingRecoveryContext = RecoveryKeyEntryContext.postLogin;
    _pendingPostRecoveryAction = null;
  }

  /// Generate recovery key → show → then complete.
  Future<void> _handleRecoveryKeyThenComplete() async {
    try {
      await _api.fetchCsrfToken();
      final material = await _generateRecoveryKey();
      if (material != null && mounted) {
        _pendingRecoveryMaterial = material;
        // postLogin even when _firstSetupDetected: see rationale on the
        // sibling _handleRecoveryKeyThenPasskey — direct-render path lacks
        // the stepper postSetup relies on, and postLogin's subtitle is the
        // correct fallback regardless of whether the operator's prior
        // screen was setup or login.
        _pendingRecoveryContext = RecoveryKeyEntryContext.postLogin;
        final keyId = material.keyId;
        _pendingPostRecoveryAction = () async {
          // Await the ack here — _resumeOIDCOrComplete below can assign
          // window.location.href, which aborts in-flight requests on the
          // page. A fire-and-forget ack would race the navigation and may
          // never reach the server, leaving RecoveryAckAt=zero and the
          // operator re-prompted with rotated words on next sign-in.
          await ackRecoveryKey(keyId);
          if (!mounted) return;
          _clearPendingRecovery();
          _resumeOIDCOrComplete();
        };
        setState(() => _loading = false);
        return;
      }
    } on Object catch (e) {
      debugPrint('Recovery key before complete failed: $e');
    }
    _resumeOIDCOrComplete();
  }

  /// Resume an OIDC auth request or redirect to a validated next URL, falling
  /// through to widget.onComplete when neither is set. Redirect work is
  /// fire-and-forget — `window.location.href` aborts in-flight requests on
  /// success, so the caller cannot reliably observe completion.
  void _resumeOIDCOrComplete() {
    if (_authRequestId == null &&
        (_nextUrl == null || _nextUrl!.isEmpty)) {
      widget.onComplete(_firstSetupDetected);
      return;
    }
    unawaited(_api.fetchCsrfToken().then((_) async {
      if (_authRequestId != null) {
        try {
          final response = await _api.post(
            '/api/v1/oauth/resume',
            body: {'auth_request_id': _authRequestId},
          ) as Map<String, dynamic>;
          final data = response['data'] as Map<String, dynamic>?;
          final redirectUrl = data?['redirect_url'] as String?;
          if (redirectUrl != null && redirectUrl.isNotEmpty) {
            web.window.location.href = redirectUrl;
            return;
          }
        } on Object catch (e) {
          debugPrint('OIDC resume failed: $e');
        }
      } else if (_nextUrl != null) {
        try {
          final response = await _api.get(
            '/api/v1/auth/validate-next',
            queryParameters: {'next': _nextUrl},
          );
          if (response is Map && response['valid'] == true) {
            final redirectUrl = response['redirect_url'] as String?;
            if (redirectUrl != null && redirectUrl.isNotEmpty) {
              web.window.location.href = redirectUrl;
              return;
            }
          }
        } on Object catch (e) {
          debugPrint('Next URL validation failed: $e');
        }
      }
      widget.onComplete(_firstSetupDetected);
    }));
  }

  // --- Reboot handler for install_complete ---

  Future<void> _rebootAfterInstall() async {
    try {
      await _api.post('/api/v1/system/reboot');
    } on ApiException {
      rethrow;
    } on Object catch (_) {}
  }

  // --- Build ---

  @override
  Widget build(BuildContext context) {
    return LayoutBuilder(
      builder: (context, constraints) {
        final dialogWidth = (constraints.maxWidth * 0.9).clamp(360.0, 480.0);
        final dialogMaxHeight = (constraints.maxHeight * 0.9).clamp(420.0, 600.0);

        Widget content;
        String title;
        Widget? stepper;
        var showOtherDevices = false;

        if (_loading) {
          content = const Padding(
            padding: EdgeInsets.all(48),
            child: CircularProgressIndicator(color: PiccoloTheme.cobalt600),
          );
          title = 'Checking status...';
        } else if (_bootError != null) {
          content = ErrorStep(error: _bootError!, onRetry: _checkBoot);
          title = 'Connection Error';
          showOtherDevices = true;
        } else if (_systemError != null) {
          content = SystemErrorStep(error: _systemError!, onRetry: _checkBoot);
          title = 'System Error';
        } else if (_showFinishing) {
          content = FinishingStep(phase: _finishingPhase);
          title = 'Setting up...';
        } else if (_showUnlocking) {
          content = const UnlockingStep();
          title = 'Welcome back';
        } else if (_pendingRecoveryMaterial != null) {
          content = RecoveryKeyStep(
            words: _pendingRecoveryMaterial!.words,
            onNext: _pendingPostRecoveryAction!,
            entryContext: _pendingRecoveryContext,
            wasRotated: _pendingRecoveryMaterial!.wasRotated,
          );
          title = 'Recovery key';
        } else {
          final result = _buildControllerContent();
          content = result.widget;
          title = result.title;
          stepper = result.stepper;
          showOtherDevices = result.showOtherDevices;
        }

        return ColoredBox(
          color: PiccoloTheme.scrim,
          child: Center(
            child: Container(
              width: dialogWidth,
              constraints: BoxConstraints(maxHeight: dialogMaxHeight),
              decoration: BoxDecoration(
                color: PiccoloTheme.porcelain,
                borderRadius: BorderRadius.circular(Radii.lg),
                boxShadow: [
                  BoxShadow(
                    color: PiccoloTheme.scrim.withValues(alpha: 0.2),
                    blurRadius: 40,
                    offset: const Offset(0, 20),
                  ),
                ],
                border: Border.all(color: PiccoloTheme.porcelain),
              ),
              child: Column(
                mainAxisSize: MainAxisSize.min,
                children: [
                  Padding(
                    padding: const EdgeInsets.fromLTRB(24, 24, 24, 16),
                    child: Column(
                      children: [
                        if (!_loading)
                          Text(
                            title,
                            style: PiccoloTheme.textTheme.bodyMedium
                                ?.copyWith(color: PiccoloTheme.inkMuted),
                          ),
                        if (stepper != null) ...[
                          if (!_loading) const SizedBox(height: 12),
                          stepper,
                        ],
                      ],
                    ),
                  ),
                  Flexible(
                    child: SingleChildScrollView(
                      padding: const EdgeInsets.only(bottom: 8),
                      child: Column(
                        mainAxisSize: MainAxisSize.min,
                        children: [
                          AnimatedSwitcher(
                            duration: Motion.slow,
                            child: content,
                          ),
                          if (showOtherDevices)
                            const OtherDevicesPanel(),
                        ],
                      ),
                    ),
                  ),
                ],
              ),
            ),
          ),
        );
      },
    );
  }

  _ContentResult _buildControllerContent() {
    final ctrl = _controller;

    if (ctrl is FirstRunController) {
      return _buildFirstRunContent(ctrl);
    } else if (ctrl is AuthController) {
      return _buildAuthContent(ctrl);
    } else if (ctrl is InviteController) {
      return _buildInviteContent(ctrl);
    } else if (ctrl is OnboardingController) {
      return _buildOnboardingContent(ctrl);
    }

    // install_complete (no controller) or unexpected null.
    if (ctrl == null) {
      debugPrint('SetupRouter: no controller and no direct-render state');
    }
    return _ContentResult(
      widget: InstallCompleteStep(
        onReboot: _rebootAfterInstall,
        bootOrderConfigured: _bootOrderConfigured,
      ),
      title: 'Installation Complete',
    );
  }

  _ContentResult _buildFirstRunContent(FirstRunController ctrl) {
    final step = ctrl.step;
    final isRemote = ctrl.isRemote;

    Widget widget;
    String title;

    switch (step) {
      case FirstRunStep.welcome:
        widget = WelcomeStep(onNext: ctrl.startSetup);
        title = 'Welcome';
      case FirstRunStep.remoteAddress:
        widget = RemoteAddressStep(
          enrolled: ctrl.namekEnrolled,
          baseDomain: ctrl.namekBaseDomain ?? '',
          suggestedHostname: ctrl.namekSuggestedHostname,
          status: ctrl.namekStatus,
          onSubmit: ctrl.submitHostname,
          onSkip: ctrl.skipRemoteAddress,
          onRefresh: ctrl.refreshEnrollmentStatus,
          error: ctrl.hostnameError,
          isSubmitting: ctrl.settingHostname,
        );
        title = 'Choose your address';
      case FirstRunStep.preparingRemote:
        widget = PreparingRemoteStep(
          relayReady: ctrl.relayReady,
          certReady: ctrl.certReady,
          onSkip: ctrl.skipRemoteWait,
        );
        title = 'Choose your address';
      case FirstRunStep.credentials:
        widget = CredentialsStep(
          onSubmit: ctrl.submitCredentials,
          error: ctrl.error,
          onBack: isRemote ? null : ctrl.backToRemoteAddress,
        );
        title = 'Secure your disk';
      case FirstRunStep.recoveryKey:
        widget = RecoveryKeyStep(
          words: ctrl.recoveryWords,
          onNext: ctrl.proceedAfterRecovery,
          entryContext: ctrl.recoveryEntryContext,
          wasRotated: ctrl.recoveryWasRotated,
        );
        title = 'Recovery key';
      case FirstRunStep.security:
        widget = SecurityStep(onNext: ctrl.completeSetup);
        title = 'Security';
      case FirstRunStep.passkeyRequired:
        widget = PasskeyRequiredStep(
          onRegister: ctrl.registerPasskey,
          error: ctrl.passkeyError,
        );
        title = 'Passkey Setup';
    }

    // If in finishing phase, override with finishing step.
    if (ctrl.setupPhase != null) {
      widget = FinishingStep(key: const ValueKey('finishing'), phase: ctrl.setupPhase);
      title = 'Setting up...';
    }

    return _ContentResult(
      widget: widget,
      title: title,
      stepper: _buildFirstRunStepper(ctrl),
      showOtherDevices: step == FirstRunStep.welcome,
    );
  }

  _ContentResult _buildAuthContent(AuthController ctrl) {
    final step = ctrl.step;
    Widget widget;
    String title;
    var showOtherDevices = false;

    switch (step) {
      case AuthStep.main:
        switch (ctrl.mode) {
          case AuthMode.unlock:
            widget = UnlockStep(
              onUnlock: ctrl.unlock,
              onForgotPassword: ctrl.startRecovery,
              error: ctrl.error,
              recentlyTriedUnlock: _recentlyTriedUnlock,
            );
            title = 'Unlock Device';
            showOtherDevices = true;
          case AuthMode.login:
            widget = LoginStep(
              onLogin: ctrl.login,
              onLoginWithPasskey: ctrl.loginWithPasskey,
              onForgotPassword: ctrl.startRecovery,
              loginMethods: ctrl.loginMethods,
              error: ctrl.error,
              passkeyError: ctrl.passkeyError,
            );
            title = 'Log In';
            showOtherDevices = true;
          case AuthMode.passkeyRequired:
            widget = PasskeyRequiredStep(
              onRegister: ctrl.registerPasskey,
              error: ctrl.passkeyError,
            );
            title = 'Passkey Setup';
        }
      case AuthStep.forgotPassword:
        widget = ForgotPasswordStep(
          onReset: ctrl.resetPassword,
          onCancel: ctrl.cancelRecovery,
          error: ctrl.error,
        );
        title = 'Reset Password';
      case AuthStep.finishing:
        widget = FinishingStep(key: const ValueKey('finishing'), phase: ctrl.setupPhase);
        title = 'Setting up...';
      case AuthStep.recoveryKey:
        widget = RecoveryKeyStep(
          words: ctrl.recoveryWords,
          onNext: ctrl.proceedAfterRecovery,
          entryContext: ctrl.recoveryEntryContext,
          wasRotated: ctrl.recoveryWasRotated,
        );
        title = 'Recovery key';
    }

    return _ContentResult(
      widget: widget,
      title: title,
      showOtherDevices: showOtherDevices,
    );
  }

  _ContentResult _buildInviteContent(InviteController ctrl) {
    return _ContentResult(
      widget: InviteStep(
        username: ctrl.username,
        onRegister: ctrl.register,
        isLoading: ctrl.isRegistering,
        isValidating: ctrl.step == InvitePhase.validating,
        error: ctrl.error,
      ),
      title: 'Welcome',
    );
  }

  _ContentResult _buildOnboardingContent(OnboardingController ctrl) {
    Widget widget;
    String title;
    var showOtherDevices = false;

    switch (ctrl.step) {
      case OnboardingPhase.choice:
        showOtherDevices = true;
        widget = OnboardingStep(
          onTryPiccolo: ctrl.chooseTryPiccolo,
          onInstallDisk: ctrl.chooseInstallDisk,
          error: ctrl.error,
        );
        title = 'Get Started';
      case OnboardingPhase.installDisk:
        widget = InstallDiskStep(
          disks: ctrl.disks,
          onStartInstall: ctrl.startInstall,
          onBack: ctrl.backToChoice,
          onInstallComplete: ctrl.onInstallComplete,
          taskId: ctrl.installTaskId,
          error: ctrl.error,
          onRefreshDisks: ctrl.fetchDisks,
        );
        title = 'Install to Disk';
      case OnboardingPhase.installComplete:
        widget = InstallCompleteStep(
          onReboot: ctrl.rebootAfterInstall,
          bootOrderConfigured: ctrl.bootOrderConfigured,
        );
        title = 'Installation Complete';
    }

    return _ContentResult(widget: widget, title: title, showOtherDevices: showOtherDevices);
  }

  Widget _buildFirstRunStepper(FirstRunController ctrl) {
    final isRemote = ctrl.isRemote;
    final enrolled = ctrl.namekEnrolled;
    final step = ctrl.step;

    List<String> steps;
    int currentStep;

    if (isRemote) {
      steps = ['Encryption', 'Recovery', 'Passkey'];
      currentStep = switch (step) {
        FirstRunStep.credentials => 0,
        FirstRunStep.recoveryKey => 1,
        FirstRunStep.passkeyRequired => 2,
        _ => 0,
      };
    } else {
      final lastStep = enrolled ? 'Passkey' : 'Security';
      steps = ['Welcome', 'Address', 'Encryption', 'Recovery', lastStep];
      currentStep = switch (step) {
        FirstRunStep.welcome => 0,
        FirstRunStep.remoteAddress || FirstRunStep.preparingRemote => 1,
        FirstRunStep.credentials => 2,
        FirstRunStep.recoveryKey => 3,
        FirstRunStep.security || FirstRunStep.passkeyRequired => 4,
      };
    }

    // Override for finishing phase.
    if (ctrl.setupPhase != null) {
      currentStep = isRemote ? 0 : 2;
    }

    return _Stepper(steps: steps, currentStep: currentStep);
  }
}

class _ContentResult {
  const _ContentResult({
    required this.widget,
    required this.title,
    this.stepper,
    this.showOtherDevices = false,
  });
  final Widget widget;
  final String title;
  final Widget? stepper;
  final bool showOtherDevices;
}

class _Stepper extends StatelessWidget {
  const _Stepper({required this.steps, required this.currentStep});
  final List<String> steps;
  final int currentStep;

  @override
  Widget build(BuildContext context) {
    final children = <Widget>[];
    for (var i = 0; i < steps.length; i++) {
      if (i > 0) children.add(_separator(i < currentStep));
      children.add(_indicator(i, steps[i]));
    }
    return Row(children: children);
  }

  Widget _indicator(int step, String label) {
    final isActive = step == currentStep;
    final isCompleted = step < currentStep;

    return Expanded(
      child: Column(
        children: [
          CircleAvatar(
            radius: 12,
            backgroundColor: isActive || isCompleted
                ? PiccoloTheme.cobalt600
                : PiccoloTheme.mist,
            foregroundColor: isActive || isCompleted
                ? Colors.white
                : PiccoloTheme.inkMuted,
            child: isCompleted
                ? const Icon(PiccoloIcons.check, size: 14)
                : Text('${step + 1}', style: const TextStyle(fontSize: 12)),
          ),
          const SizedBox(height: 6),
          Text(
            label,
            style: TextStyle(
              fontSize: 12,
              fontWeight: isActive ? FontWeight.w600 : FontWeight.normal,
              color: isActive ? PiccoloTheme.ink : PiccoloTheme.inkMuted,
            ),
          ),
        ],
      ),
    );
  }

  Widget _separator(bool completed) {
    return SizedBox(
      width: 24,
      child: Divider(
        height: 1,
        color: completed ? PiccoloTheme.cobalt600 : null,
      ),
    );
  }
}
