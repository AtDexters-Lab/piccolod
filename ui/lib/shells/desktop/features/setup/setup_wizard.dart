import 'dart:async';
import 'dart:js_interop';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/network_models.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/network_service.dart';
import 'package:piccolo_os/core/services/webauthn_service.dart';
import 'package:piccolo_os/core/utils/downloader/downloader.dart';
import 'package:piccolo_os/shared/widgets/ca_import_guide.dart';
import 'package:piccolo_os/shared/widgets/diagnostic_log_download.dart';
import 'package:piccolo_os/shared/widgets/login_form_fields.dart';
import 'package:piccolo_os/shared/widgets/password_set_form.dart';
import 'package:piccolo_os/shells/desktop/features/setup/install_disk_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/onboarding_step.dart';
import 'package:piccolo_os/shells/desktop/features/setup/setup_controller.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:url_launcher/url_launcher.dart';
import 'package:web/web.dart' as web;

class SetupWizard extends StatefulWidget {

  const SetupWizard({required this.onComplete, super.key});
  // ignore: avoid_positional_boolean_parameters, matches DesktopController.completeSetup callback signature
  final void Function(bool isFirstSetupFlow) onComplete;

  @override
  State<SetupWizard> createState() => _SetupWizardState();
}

class _SetupWizardState extends State<SetupWizard> {
  final SetupController _controller = SetupController();
  bool _didCallComplete = false;

  @override
  Widget build(BuildContext context) {
    return ListenableBuilder(
      listenable: _controller,
      builder: (context, child) {
        final state = _controller.state;
        // If complete, trigger callback
        if (state == SetupState.complete && !_didCallComplete) {
          _didCallComplete = true;
          // Schedule callback to avoid build-phase issues
          WidgetsBinding.instance.addPostFrameCallback(
            (_) => widget.onComplete(_controller.isFirstSetupFlow),
          );
          return const SizedBox.shrink();
        } else if (state == SetupState.complete) {
          return const SizedBox.shrink();
        }

        return LayoutBuilder(
          builder: (context, constraints) {
            final dialogWidth = (constraints.maxWidth * 0.9).clamp(
              360.0,
              480.0,
            );
            final dialogMaxHeight = (constraints.maxHeight * 0.9).clamp(
              420.0,
              600.0,
            );
            final showFirstRunStepper =
                _controller.isFirstSetupFlow &&
                (state == SetupState.welcome ||
                    state == SetupState.credentials ||
                    state == SetupState.finishing ||
                    state == SetupState.recovery ||
                    state == SetupState.security) &&
                state != SetupState.onboarding &&
                state != SetupState.installDisk &&
                state != SetupState.installComplete;

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
                      // Header (logo-free)
                      Padding(
                        padding: const EdgeInsets.fromLTRB(24, 24, 24, 16),
                        child: Column(
                          children: [
                            if (state == SetupState.loading)
                              const Text('Checking status...')
                            else if (state != SetupState.welcome)
                              Text(
                                _getTitleForState(state),
                                style: PiccoloTheme.textTheme.bodyMedium
                                    ?.copyWith(color: PiccoloTheme.inkMuted),
                              ),
                            if (showFirstRunStepper) ...[
                              if (state != SetupState.loading)
                                const SizedBox(height: 12),
                              _FirstRunStepper(
                                currentStep: _getFirstRunStepIndex(state),
                              ),
                            ],
                          ],
                        ),
                      ),

                      // Content Body (scrolls on small desktop windows)
                      Flexible(
                        child: SingleChildScrollView(
                          padding: const EdgeInsets.only(bottom: 8),
                          child: Column(
                            mainAxisSize: MainAxisSize.min,
                            children: [
                              AnimatedSwitcher(
                                duration: Motion.slow,
                                child: _buildStepContent(state),
                              ),
                              // Other devices panel (shown in all states except loading/finishing/system error)
                              if (state != SetupState.loading &&
                                  state != SetupState.finishing &&
                                  state != SetupState.recovery &&
                                  state != SetupState.security &&
                                  state != SetupState.systemError &&
                                  state != SetupState.installDisk &&
                                  state != SetupState.installComplete)
                                const _OtherDevicesPanel(),
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
      },
    );
  }

  String _getTitleForState(SetupState state) {
    switch (state) {
      case SetupState.onboarding:
        return 'Get Started';
      case SetupState.installDisk:
        return 'Install to Disk';
      case SetupState.installComplete:
        return 'Installation Complete';
      case SetupState.welcome:
        return 'Welcome';
      case SetupState.credentials:
        return 'Create admin password';
      case SetupState.recovery:
        return 'Recovery key';
      case SetupState.security:
        return 'Security';
      case SetupState.finishing:
        return 'Setting up...';
      case SetupState.unlock:
        return 'Unlock Device';
      case SetupState.login:
        return 'Log In';
      case SetupState.forgotPassword:
        return 'Reset Password';
      case SetupState.invite:
        return 'Welcome';
      case SetupState.passkeyRequired:
        return 'Passkey Setup';
      case SetupState.error:
        return 'Connection Error';
      case SetupState.systemError:
        return 'System Error';
      case SetupState.loading:
      case SetupState.complete:
        return '';
    }
  }

  int _getFirstRunStepIndex(SetupState state) {
    switch (state) {
      case SetupState.welcome:
        return 0;
      case SetupState.credentials:
      case SetupState.finishing:
        return 1;
      case SetupState.recovery:
        return 2;
      case SetupState.security:
        return 3;
      case SetupState.loading:
      case SetupState.onboarding:
      case SetupState.installDisk:
      case SetupState.installComplete:
      case SetupState.unlock:
      case SetupState.login:
      case SetupState.forgotPassword:
      case SetupState.invite:
      case SetupState.passkeyRequired:
      case SetupState.error:
      case SetupState.systemError:
      case SetupState.complete:
        return 0;
    }
  }

  Widget _buildStepContent(SetupState state) {
    switch (state) {
      case SetupState.loading:
        return const Padding(
          padding: EdgeInsets.all(48),
          child: CircularProgressIndicator(color: PiccoloTheme.cobalt600),
        );
      case SetupState.onboarding:
        return OnboardingStep(
          onTryPiccolo: _controller.chooseTryPiccolo,
          onInstallDisk: _controller.chooseInstallDisk,
          error: _controller.error,
        );
      case SetupState.installDisk:
        return InstallDiskStep(
          disks: _controller.disks,
          onStartInstall: _controller.startInstall,
          onBack: _controller.backToOnboarding,
          onInstallComplete: _controller.onInstallComplete,
          taskId: _controller.installTaskId,
          error: _controller.error,
          onRefreshDisks: _controller.fetchDisks,
        );
      case SetupState.installComplete:
        return _InstallCompleteStep(
          onReboot: _controller.rebootAfterInstall,
          bootOrderConfigured: _controller.bootOrderConfigured,
        );
      case SetupState.welcome:
        return _WelcomeStep(onNext: _controller.startSetup);
      case SetupState.credentials:
        return _CredentialsStep(
          onSubmit: _controller.submitCredentials,
          initialError: _controller.error != null
              ? 'Setup failed. Please try again.'
              : null,
        );
      case SetupState.finishing:
        return const _FinishingStep();
      case SetupState.recovery:
        return _RecoveryStep(
          words: _controller.recoveryWords,
          onNext: _controller.proceedToSecurity,
        );
      case SetupState.security:
        return _SecurityStep(
          onNext: _controller.completeSetup,
        );
      case SetupState.unlock:
        return _UnlockStep(
          onUnlock: _controller.unlock,
          onForgotPassword: _controller.startRecovery,
          controller: _controller,
        );
      case SetupState.login:
        return _LoginStep(
          onLogin: _controller.login,
          onLoginWithPasskey: _controller.loginWithPasskey,
          onForgotPassword: _controller.startRecovery,
          controller: _controller,
        );
      case SetupState.invite:
        return _InviteStep(controller: _controller);
      case SetupState.passkeyRequired:
        return _PasskeyRequiredStep(controller: _controller);
      case SetupState.forgotPassword:
        return _ForgotPasswordStep(
          onReset: _controller.resetPassword,
          onCancel: _controller.cancelRecovery,
        );
      case SetupState.error:
        return _ErrorStep(
          error: _controller.error ?? 'Unknown error',
          onRetry: _controller.retry,
        );
      case SetupState.systemError:
        return _SystemErrorStep(
          error: _controller.error ?? 'Unknown system error',
          onRetry: _controller.retry,
        );
      case SetupState.complete:
        return const SizedBox.shrink();
    }
  }
}

// --- Steps ---

class _FirstRunStepper extends StatelessWidget {

  const _FirstRunStepper({required this.currentStep});
  final int currentStep;

  @override
  Widget build(BuildContext context) {
    return Row(
      children: [
        _buildStepIndicator(0, 'Welcome'),
        _buildStepSeparator(),
        _buildStepIndicator(1, 'Password'),
        _buildStepSeparator(),
        _buildStepIndicator(2, 'Recovery'),
        _buildStepSeparator(),
        _buildStepIndicator(3, 'Security'),
      ],
    );
  }

  Widget _buildStepIndicator(int step, String label) {
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

  Widget _buildStepSeparator() {
    return const SizedBox(width: 24, child: Divider(height: 1));
  }
}

class _FinishingStep extends StatelessWidget {
  const _FinishingStep();

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 24, 32, 48),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          const CircularProgressIndicator(color: PiccoloTheme.cobalt600),
          const SizedBox(height: 24),
          Text(
            'Encrypting storage and creating your admin…',
            style: PiccoloTheme.textTheme.bodyLarge,
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 8),
          Text(
            'This usually takes a few seconds.',
            style: PiccoloTheme.textTheme.labelSmall,
            textAlign: TextAlign.center,
          ),
        ],
      ),
    );
  }
}

class _InstallCompleteStep extends StatefulWidget {

  const _InstallCompleteStep({
    required this.onReboot,
    this.bootOrderConfigured = false,
  });
  final Future<void> Function() onReboot;
  final bool bootOrderConfigured;

  @override
  State<_InstallCompleteStep> createState() => _InstallCompleteStepState();
}

enum _RebootPhase { idle, rebooting, polling, timedOut }

class _InstallCompleteStepState extends State<_InstallCompleteStep> {
  static const _pollUrl = 'http://piccolo.local/api/v1/health/live';
  static const _redirectUrl = 'http://piccolo.local';
  static const _pollInterval = Duration(seconds: 3);
  static const _pollTimeout = Duration(minutes: 2);
  static const _initialDelay = Duration(seconds: 5);

  _RebootPhase _phase = _RebootPhase.idle;
  bool _pollInFlight = false;
  String? _rebootError;
  Timer? _pollTimer;
  Timer? _timeoutTimer;

  @override
  void dispose() {
    _pollTimer?.cancel();
    _timeoutTimer?.cancel();
    super.dispose();
  }

  Future<void> _rebootAndWait() async {
    if (_phase != _RebootPhase.idle) return;
    setState(() {
      _phase = _RebootPhase.rebooting;
      _rebootError = null;
    });

    try {
      await widget.onReboot();
    } on Object catch (e) {
      // Real API error (e.g. 409) — the device did NOT reboot.
      if (mounted) {
        setState(() {
          _phase = _RebootPhase.idle;
          _rebootError = e.toString();
        });
      }
      return;
    }

    if (!mounted) return;

    if (widget.bootOrderConfigured) {
      // Device will reboot to internal disk — poll and redirect.
      setState(() => _phase = _RebootPhase.polling);
      // Wait for the device to actually go down before polling.
      await Future<void>.delayed(_initialDelay);
      if (!mounted) return;
      unawaited(_pollDevice());
      _pollTimer = Timer.periodic(_pollInterval, (_) {
        unawaited(_pollDevice());
      });
      // Give up after _pollTimeout.
      _timeoutTimer = Timer(_pollTimeout, () {
        _pollTimer?.cancel();
        if (mounted) setState(() => _phase = _RebootPhase.timedOut);
      });
    }
    // If !bootOrderConfigured, the device powers off. The button stays
    // disabled with "Shutting down..." — there's nothing more to do.
  }

  Future<void> _pollDevice() async {
    if (_pollInFlight) return;
    _pollInFlight = true;
    try {
      // Use no-cors mode: the page may be served from a different origin
      // (e.g. a LAN IP or device-specific hostname) than piccolo.local.
      final init = web.RequestInit(mode: 'no-cors');
      final response = await web.window
          .fetch(_pollUrl.toJS, init)
          .toDart
          .timeout(const Duration(seconds: 3));
      // In no-cors mode, a successful response has type "opaque" (status 0).
      if (response.ok || response.type == 'opaque') {
        _pollTimer?.cancel();
        _timeoutTimer?.cancel();
        if (mounted) {
          web.window.location.href = _redirectUrl;
        }
      }
    } on Object catch (_) {
      // Device still rebooting — continue polling.
    } finally {
      _pollInFlight = false;
    }
  }

  @override
  Widget build(BuildContext context) {
    if (_phase == _RebootPhase.polling || _phase == _RebootPhase.timedOut) {
      final timedOut = _phase == _RebootPhase.timedOut;
      final message = timedOut
          ? 'Could not reach Piccolo. Check your device and visit\npiccolo.local to continue setup.'
          : 'Waiting for Piccolo to come back online.\nYou\u2019ll be redirected automatically.';
      return Padding(
        padding: const EdgeInsets.fromLTRB(32, 24, 32, 48),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            if (!timedOut)
              const CircularProgressIndicator(color: PiccoloTheme.cobalt600)
            else
              const Icon(PiccoloIcons.warning, color: PiccoloTheme.warning, size: 48),
            const SizedBox(height: 24),
            Text(
              timedOut ? 'Device unreachable' : 'Rebooting...',
              style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
                fontWeight: FontWeight.w600,
              ),
              textAlign: TextAlign.center,
            ),
            const SizedBox(height: 8),
            Text(
              message,
              style: PiccoloTheme.textTheme.labelSmall,
              textAlign: TextAlign.center,
            ),
          ],
        ),
      );
    }

    final isRebooting = _phase == _RebootPhase.rebooting;
    final subtitle = widget.bootOrderConfigured
        ? 'Your device will reboot into the internal disk. You can remove the USB drive at any time.'
        : 'Remove the USB drive after the device powers off, then power it back on.';
    final buttonLabel = widget.bootOrderConfigured ? 'Reboot Now' : 'Power Off';
    final buttonIcon = widget.bootOrderConfigured ? PiccoloIcons.restart : PiccoloIcons.power;
    final activeLabel = widget.bootOrderConfigured ? 'Rebooting...' : 'Shutting down...';

    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          const Icon(
            PiccoloIcons.success,
            color: PiccoloTheme.success,
            size: 48,
          ),
          const SizedBox(height: 16),
          Text(
            'Piccolo has been installed',
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              fontWeight: FontWeight.w600,
            ),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 8),
          Text(
            subtitle,
            style: const TextStyle(fontSize: 13, color: PiccoloTheme.inkMuted),
            textAlign: TextAlign.center,
          ),
          if (_rebootError != null) ...[
            const SizedBox(height: 16),
            Container(
              padding: const EdgeInsets.all(12),
              decoration: BoxDecoration(
                color: PiccoloTheme.critical.withValues(alpha: 0.08),
                borderRadius: BorderRadius.circular(Radii.sm),
                border: Border.all(
                  color: PiccoloTheme.critical.withValues(alpha: 0.2),
                ),
              ),
              child: Text(
                _rebootError!,
                style: PiccoloTheme.textTheme.labelMedium?.copyWith(
                  color: PiccoloTheme.critical,
                ),
              ),
            ),
          ],
          const SizedBox(height: 32),
          FilledButton.icon(
            onPressed: isRebooting ? null : _rebootAndWait,
            icon: isRebooting
                ? const SizedBox(
                    width: 20,
                    height: 20,
                    child: CircularProgressIndicator(
                      color: Colors.white,
                      strokeWidth: 2,
                    ),
                  )
                : Icon(buttonIcon),
            label: Text(isRebooting ? activeLabel : buttonLabel),
          ),
        ],
      ),
    );
  }
}

class _WelcomeStep extends StatelessWidget {

  const _WelcomeStep({required this.onNext});
  final VoidCallback onNext;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          Text(
            'Hello',
            style: PiccoloTheme.textTheme.displayLarge,
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 16),
          Text(
            "Let's set up your Piccolo.\nCreate an admin password and save a recovery key. Takes about a minute.",
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              color: PiccoloTheme.inkMuted,
            ),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 40),
          FilledButton(
            onPressed: onNext,
            child: const Text('Start setup'),
          ),
        ],
      ),
    );
  }
}

class _CredentialsStep extends StatefulWidget {

  const _CredentialsStep({required this.onSubmit, this.initialError});
  final Future<bool> Function(String) onSubmit;
  final String? initialError;

  @override
  State<_CredentialsStep> createState() => _CredentialsStepState();
}

class _CredentialsStepState extends State<_CredentialsStep> {
  final TextEditingController _passController = TextEditingController();
  final TextEditingController _confirmController = TextEditingController();
  String? _error;
  String? _confirmError;
  bool _isSubmitting = false;

  @override
  void initState() {
    super.initState();
    _error = widget.initialError;
  }

  @override
  void didUpdateWidget(covariant _CredentialsStep oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (widget.initialError != oldWidget.initialError &&
        widget.initialError != null &&
        _error == null) {
      setState(() => _error = widget.initialError);
    }
  }

  @override
  void dispose() {
    _passController.dispose();
    _confirmController.dispose();
    super.dispose();
  }

  Future<void> _submit() async {
    setState(() {
      _error = null;
      _confirmError = null;
    });

    if (_passController.text.isEmpty) {
      setState(() => _error = 'Password is required');
      return;
    }
    if (_passController.text != _confirmController.text) {
      setState(() => _confirmError = 'Passwords do not match');
      return;
    }

    setState(() => _isSubmitting = true);
    final success = await widget.onSubmit(_passController.text);

    if (mounted && !success) {
      setState(() {
        _isSubmitting = false;
        _error = 'Setup failed. Please try again.';
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: AutofillGroup(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            PasswordSetForm(
              passwordController: _passController,
              confirmController: _confirmController,
              passwordLabel: 'Admin password',
              confirmLabel: 'Confirm password',
              passwordError: _error,
              confirmError: _confirmError,
              onSubmitted: _submit,
            ),
            const SizedBox(height: 16),
            const Text(
              'This password encrypts storage and unlocks Piccolo. Keep it somewhere safe.',
              style: TextStyle(color: PiccoloTheme.inkMuted, fontSize: 13),
            ),
            const SizedBox(height: 32),
            FilledButton(
              onPressed: _isSubmitting ? null : _submit,
              child: _isSubmitting
                  ? const SizedBox(
                      width: 20,
                      height: 20,
                      child: CircularProgressIndicator(
                        color: Colors.white,
                        strokeWidth: 2,
                      ),
                    )
                  : const Text('Continue'),
            ),
          ],
        ),
      ),
    );
  }
}

class _RecoveryStep extends StatefulWidget {

  const _RecoveryStep({required this.words, required this.onNext});
  final List<String> words;
  final VoidCallback onNext;

  @override
  State<_RecoveryStep> createState() => _RecoveryStepState();
}

class _RecoveryStepState extends State<_RecoveryStep> {
  bool _confirmed = false;

  void _downloadKey() {
    final content =
        "PICCOLO RECOVERY KEY\n\n${widget.words.join(" ")}\n\nKeep this file safe.";
    downloadTextFile(content, 'piccolo-recovery-key.txt');

    ScaffoldMessenger.of(context).showSnackBar(
      const SnackBar(
        content: Text('Downloading recovery key...'),
        duration: Duration(seconds: 2),
      ),
    );
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          Text(
            'Your recovery key',
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              fontWeight: FontWeight.bold,
            ),
          ),
          const SizedBox(height: 12),
          Container(
            padding: const EdgeInsets.all(12),
            decoration: BoxDecoration(
              color: PiccoloTheme.mist,
              borderRadius: BorderRadius.circular(Radii.sm),
              border: Border.all(
                color: PiccoloTheme.cobalt600.withValues(alpha: 0.35),
                width: 1.2,
              ),
            ),
            child: const Row(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Icon(
                  PiccoloIcons.info,
                  color: PiccoloTheme.inkMuted,
                  size: 18,
                ),
                SizedBox(width: 8),
                Expanded(
                  child: Text(
                    "Your 24-word recovery key is below. Store it offline; you'll need it to reset your password.",
                    style: TextStyle(
                      fontSize: 13,
                      color: PiccoloTheme.inkMuted,
                    ),
                  ),
                ),
              ],
            ),
          ),
          const SizedBox(height: 18),
          Container(
            padding: const EdgeInsets.all(16),
            decoration: BoxDecoration(
              color: PiccoloTheme.porcelain,
              borderRadius: BorderRadius.circular(Radii.sm),
              border: Border.all(
                color: PiccoloTheme.ink.withValues(alpha: 0.06),
              ),
            ),
            constraints: const BoxConstraints(maxHeight: 150),
            child: SingleChildScrollView(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  SelectableText(
                    widget.words.join(' '),
                    style: PiccoloTheme.mono.copyWith(
                      fontSize: 16,
                      height: 1.4,
                    ),
                  ),
                ],
              ),
            ),
          ),
          const SizedBox(height: 18),

          // Download button (clipboard copy is unreliable in insecure contexts)
          OutlinedButton.icon(
            onPressed: _downloadKey,
            icon: const Icon(PiccoloIcons.download, size: 16),
            label: const Text('Download key'),
            style: OutlinedButton.styleFrom(foregroundColor: PiccoloTheme.ink),
          ),
          const SizedBox(height: 20),

          // Confirmation Checkbox
          InkWell(
            onTap: () => setState(() => _confirmed = !_confirmed),
            child: Row(
              children: [
                Checkbox(
                  value: _confirmed,
                  onChanged: (v) => setState(() => _confirmed = v ?? false),
                  activeColor: PiccoloTheme.cobalt600,
                ),
                Expanded(
                  child: Text(
                    "I've saved this recovery key somewhere safe.",
                    style: PiccoloTheme.textTheme.bodyMedium,
                  ),
                ),
              ],
            ),
          ),

          const SizedBox(height: 24),
          FilledButton(
            onPressed: _confirmed ? widget.onNext : null,
            style: FilledButton.styleFrom(
              backgroundColor: PiccoloTheme.success,
              foregroundColor: Colors.white,
              disabledBackgroundColor: PiccoloTheme.ink.withValues(alpha: 0.1),
              disabledForegroundColor: PiccoloTheme.ink.withValues(alpha: 0.3),
              padding: const EdgeInsets.symmetric(horizontal: 32, vertical: 16),
              shape: RoundedRectangleBorder(
                borderRadius: BorderRadius.circular(Radii.sm),
              ),
            ),
            child: const Text('Continue'),
          ),
        ],
      ),
    );
  }
}

class _SecurityStep extends StatefulWidget {
  const _SecurityStep({required this.onNext});
  final VoidCallback onNext;

  @override
  State<_SecurityStep> createState() => _SecurityStepState();
}

class _SecurityStepState extends State<_SecurityStep> {
  bool _downloading = false;

  Future<void> _downloadCA() async {
    setState(() => _downloading = true);
    try {
      final response = await ApiClient().get('/api/v1/system/ca.crt');
      downloadTextFile(response as String, 'piccolo-ca.crt');
    } on Object catch (_) {
      if (mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          const SnackBar(
            content: Text('Download failed. You can download later from Settings > Security.'),
            duration: Duration(seconds: 3),
          ),
        );
      }
    } finally {
      if (mounted) setState(() => _downloading = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          Text('Secure your connection',
              style: PiccoloTheme.textTheme.bodyLarge
                  ?.copyWith(fontWeight: FontWeight.bold)),
          const SizedBox(height: 12),
          Container(
            padding: const EdgeInsets.all(12),
            decoration: BoxDecoration(
              color: PiccoloTheme.mist,
              borderRadius: BorderRadius.circular(Radii.sm),
              border: Border.all(
                color: PiccoloTheme.cobalt600.withValues(alpha: 0.35),
                width: 1.2,
              ),
            ),
            child: const Row(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Icon(PiccoloIcons.lock, color: PiccoloTheme.inkMuted, size: 18),
                SizedBox(width: 8),
                Expanded(
                  child: Text(
                    'Your device supports HTTPS on the local network. '
                    'Download the CA certificate and import it into your browser '
                    'to access the portal securely without warnings.',
                    style: TextStyle(fontSize: 13, color: PiccoloTheme.inkMuted),
                  ),
                ),
              ],
            ),
          ),
          const SizedBox(height: Spacing.lg),
          OutlinedButton.icon(
            onPressed: _downloading ? null : _downloadCA,
            icon: const Icon(PiccoloIcons.download, size: 16),
            label: Text(_downloading ? 'Downloading...' : 'Download CA Certificate'),
            style: OutlinedButton.styleFrom(foregroundColor: PiccoloTheme.ink),
          ),
          const SizedBox(height: Spacing.lg),
          const CaImportGuide(),
          const SizedBox(height: Spacing.lg),
          FilledButton(
            onPressed: widget.onNext,
            style: FilledButton.styleFrom(
              backgroundColor: PiccoloTheme.success,
              foregroundColor: Colors.white,
              padding: const EdgeInsets.symmetric(horizontal: 32, vertical: 16),
              shape: RoundedRectangleBorder(
                  borderRadius: BorderRadius.circular(Radii.sm)),
            ),
            child: const Text('Finish setup'),
          ),
        ],
      ),
    );
  }
}

class _UnlockStep extends StatefulWidget {

  const _UnlockStep({required this.onUnlock, required this.onForgotPassword, required this.controller});
  final Future<bool> Function(String) onUnlock;
  final VoidCallback onForgotPassword;
  final SetupController controller;

  @override
  State<_UnlockStep> createState() => _UnlockStepState();
}

class _UnlockStepState extends State<_UnlockStep> {
  final TextEditingController _passController = TextEditingController();
  bool _isSubmitting = false;
  bool _obscureText = true;
  String? _error;

  @override
  void dispose() {
    _passController.dispose();
    super.dispose();
  }

  Future<void> _submit() async {
    setState(() {
      _isSubmitting = true;
      _error = null;
    });

    final success = await widget.onUnlock(_passController.text);

    if (mounted && !success) {
      setState(() {
        _isSubmitting = false;
        _error = widget.controller.error ?? 'Incorrect password';
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          TextField(
            controller: _passController,
            obscureText: _obscureText,
            autofillHints: const [AutofillHints.password],
            decoration: InputDecoration(
              labelText: 'Password',
              errorText: _error,
              suffixIcon: IconButton(
                icon: Icon(
                  _obscureText
                      ? PiccoloIcons.visibilityOff
                      : PiccoloIcons.visibility,
                  color: PiccoloTheme.inkMuted,
                ),
                onPressed: () => setState(() => _obscureText = !_obscureText),
              ),
            ),
            onSubmitted: (_) => _submit(),
          ),
          const SizedBox(height: 24),
          Row(
            children: [
              TextButton(
                onPressed: widget.onForgotPassword,
                child: const Text('Forgot Password?'),
              ),
              const Spacer(),
              FilledButton(
                onPressed: _isSubmitting ? null : _submit,
                child: _isSubmitting
                    ? const SizedBox(
                        width: 20,
                        height: 20,
                        child: CircularProgressIndicator(
                          color: Colors.white,
                          strokeWidth: 2,
                        ),
                      )
                    : const Text('Unlock'),
              ),
            ],
          ),
        ],
      ),
    );
  }
}

class _LoginStep extends StatefulWidget {

  const _LoginStep({
    required this.onLogin,
    required this.onLoginWithPasskey,
    required this.onForgotPassword,
    required this.controller,
  });
  final Future<bool> Function(String, String) onLogin;
  final Future<bool> Function() onLoginWithPasskey;
  final VoidCallback onForgotPassword;
  final SetupController controller;

  @override
  State<_LoginStep> createState() => _LoginStepState();
}

class _LoginStepState extends State<_LoginStep> {
  final TextEditingController _userController = TextEditingController(
    text: 'admin',
  );
  final TextEditingController _passController = TextEditingController();
  bool _isSubmitting = false;
  String? _error;

  @override
  void initState() {
    super.initState();
    unawaited(widget.controller.fetchLoginOptions());
  }

  @override
  void dispose() {
    _userController.dispose();
    _passController.dispose();
    super.dispose();
  }

  Future<void> _submit() async {
    setState(() {
      _isSubmitting = true;
      _error = null;
    });

    final success = await widget.onLogin(
      _userController.text,
      _passController.text,
    );

    if (mounted && !success) {
      setState(() {
        _isSubmitting = false;
        _error = 'Invalid credentials';
      });
    }
  }

  Future<void> _loginWithPasskey() async {
    setState(() {
      _isSubmitting = true;
      _error = null;
    });

    final success = await widget.onLoginWithPasskey();

    if (mounted && !success) {
      setState(() {
        _isSubmitting = false;
        _error = widget.controller.error;
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    final methods = widget.controller.loginMethods;
    final showPassword = LoginFormFields.isPasswordAvailable(methods);

    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          LoginFormFields(
            methods: methods,
            usernameController: _userController,
            passwordController: _passController,
            onSubmitPassword: _submit,
            onSubmitPasskey: _loginWithPasskey,
            isLoading: _isSubmitting,
            error: _error,
            unavailableMessage:
                'Passkey sign-in is required but unavailable in this browser context. '
                'Please access this device over HTTPS to sign in.',
          ),
          if (showPassword) ...[
            const SizedBox(height: 24),
            Row(
              children: [
                TextButton(
                  onPressed: widget.onForgotPassword,
                  child: const Text('Forgot Password?'),
                ),
                const Spacer(),
                FilledButton(
                  onPressed: _isSubmitting ? null : _submit,
                  child: _isSubmitting
                      ? const SizedBox(
                          width: 20,
                          height: 20,
                          child: CircularProgressIndicator(
                            color: Colors.white,
                            strokeWidth: 2,
                          ),
                        )
                      : const Text('Log In'),
                ),
              ],
            ),
          ],
        ],
      ),
    );
  }
}

// --- Invite Step ---

class _InviteStep extends StatefulWidget {
  const _InviteStep({required this.controller});
  final SetupController controller;

  @override
  State<_InviteStep> createState() => _InviteStepState();
}

class _InviteStepState extends State<_InviteStep> {
  String? _username;
  bool _isLoading = true;
  bool _isRegistering = false;
  String? _error;

  @override
  void initState() {
    super.initState();
    unawaited(_validate());
  }

  Future<void> _validate() async {
    final username = await widget.controller.validateInviteToken();
    if (mounted) {
      setState(() {
        _username = username;
        _isLoading = false;
        if (username == null) {
          _error = 'This invite link is invalid or has expired.';
        }
      });
    }
  }

  Future<void> _register() async {
    setState(() { _isRegistering = true; _error = null; });
    final success = await widget.controller.completeInviteRegistration();
    if (mounted && !success) {
      setState(() {
        _isRegistering = false;
        _error = widget.controller.error;
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          if (_isLoading)
            const Center(child: CircularProgressIndicator(color: PiccoloTheme.cobalt600))
          else if (_error != null && _username == null) ...[
            Text(_error!, style: const TextStyle(color: PiccoloTheme.critical)),
          ] else ...[
            Text(
              'Welcome, $_username!',
              style: PiccoloTheme.textTheme.titleMedium,
            ),
            const SizedBox(height: 8),
            Text(
              'Set up your passkey to get started. This will be your fast, phishing-proof sign-in.',
              style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted),
            ),
            const SizedBox(height: 24),
            if (_error != null)
              Padding(
                padding: const EdgeInsets.only(bottom: 16),
                child: Text(_error!, style: const TextStyle(color: PiccoloTheme.critical)),
              ),
            if (WebAuthnService.isAvailable())
              FilledButton.icon(
                onPressed: _isRegistering ? null : _register,
                icon: const Icon(PiccoloIcons.fingerprint),
                label: _isRegistering
                    ? const SizedBox(
                        width: 20, height: 20,
                        child: CircularProgressIndicator(strokeWidth: 2, color: Colors.white),
                      )
                    : const Text('Create Passkey'),
              )
            else
              Text(
                'Passkey registration requires HTTPS. Please access this device over a secure connection.',
                style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted),
              ),
          ],
        ],
      ),
    );
  }
}

// --- Passkey Required Step ---

class _PasskeyRequiredStep extends StatefulWidget {
  const _PasskeyRequiredStep({required this.controller});
  final SetupController controller;

  @override
  State<_PasskeyRequiredStep> createState() => _PasskeyRequiredStepState();
}

class _PasskeyRequiredStepState extends State<_PasskeyRequiredStep> {
  bool _isLoading = false;
  String? _error;

  Future<void> _register() async {
    setState(() { _isLoading = true; _error = null; });
    final success = await widget.controller.registerPasskey();
    if (mounted && !success) {
      setState(() {
        _isLoading = false;
        _error = widget.controller.error;
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          Text(
            'Set Up Your Passkey',
            style: PiccoloTheme.textTheme.titleMedium,
          ),
          const SizedBox(height: 8),
          Text(
            'A passkey is required for remote access. This is a one-time setup.',
            style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted),
          ),
          const SizedBox(height: 24),
          if (_error != null)
            Padding(
              padding: const EdgeInsets.only(bottom: 16),
              child: Text(_error!, style: const TextStyle(color: PiccoloTheme.critical)),
            ),
          if (WebAuthnService.isAvailable())
            FilledButton.icon(
              onPressed: _isLoading ? null : _register,
              icon: const Icon(PiccoloIcons.fingerprint),
              label: _isLoading
                  ? const SizedBox(
                      width: 20, height: 20,
                      child: CircularProgressIndicator(strokeWidth: 2, color: Colors.white),
                    )
                  : const Text('Create Passkey'),
            )
          else
            Text(
              'Passkey registration requires HTTPS. Please access this device over a secure connection.',
              style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted),
            ),
        ],
      ),
    );
  }
}

class _ForgotPasswordStep extends StatefulWidget {

  const _ForgotPasswordStep({required this.onReset, required this.onCancel});
  final Future<bool> Function(String, String) onReset;
  final VoidCallback onCancel;

  @override
  State<_ForgotPasswordStep> createState() => _ForgotPasswordStepState();
}

class _ForgotPasswordStepState extends State<_ForgotPasswordStep> {
  final TextEditingController _keyController = TextEditingController();

  final TextEditingController _passController = TextEditingController();

  final TextEditingController _confirmController = TextEditingController();

  bool _isSubmitting = false;

  String? _generalError;

  String? _confirmError;

  String? _keyError;

  @override
  void dispose() {
    _keyController.dispose();

    _passController.dispose();

    _confirmController.dispose();

    super.dispose();
  }

  Future<void> _submit() async {
    setState(() {
      _generalError = null;

      _confirmError = null;

      _keyError = null;
    });

    if (_keyController.text.trim().isEmpty || _passController.text.isEmpty) {
      setState(() => _generalError = 'All fields are required');

      return;
    }

    if (_passController.text != _confirmController.text) {
      setState(() => _confirmError = 'Passwords do not match');

      return;
    }

    setState(() {
      _isSubmitting = true;
    });

    final success = await widget.onReset(
      _keyController.text,
      _passController.text,
    );

    if (mounted && !success) {
      setState(() {
        _isSubmitting = false;

        _keyError = 'Invalid recovery key';
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),

      child: Column(
        mainAxisSize: MainAxisSize.min,

        crossAxisAlignment: CrossAxisAlignment.stretch,

        children: [
          const Text(
            'Enter your 24-word recovery key to reset your password.',

            style: TextStyle(color: PiccoloTheme.inkMuted, fontSize: 13),
          ),

          const SizedBox(height: 16),

          TextField(
            controller: _keyController,

            maxLines: 3,

            decoration: InputDecoration(
              labelText: 'Recovery Key',

              hintText: 'word1 word2 ...',

              errorText: _keyError,
            ),
          ),

          const SizedBox(height: 16),

          Form(
            child: AutofillGroup(
              child: PasswordSetForm(
                passwordController: _passController,

                confirmController: _confirmController,

                passwordLabel: 'New Password',

                confirmLabel: 'Confirm New Password',

                passwordError: _generalError,

                confirmError: _confirmError,

                onSubmitted: _submit,
              ),
            ),
          ),

          const SizedBox(height: 24),

          Row(
            mainAxisAlignment: MainAxisAlignment.end,

            children: [
              TextButton(
                onPressed: widget.onCancel,

                child: const Text('Cancel'),
              ),

              const SizedBox(width: 16),

              FilledButton(
                onPressed: _isSubmitting ? null : _submit,

                child: _isSubmitting
                    ? const SizedBox(
                        width: 20,
                        height: 20,
                        child: CircularProgressIndicator(
                          color: Colors.white,
                          strokeWidth: 2,
                        ),
                      )
                    : const Text('Reset Password'),
              ),
            ],
          ),
        ],
      ),
    );
  }
}

class _SystemErrorStep extends StatelessWidget {
  const _SystemErrorStep({required this.error, required this.onRetry});
  final String error;
  final VoidCallback onRetry;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          const Icon(PiccoloIcons.warning, color: PiccoloTheme.critical, size: 48),
          const SizedBox(height: 12),
          Text(
            'Storage operation failed',
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.w600),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 8),
          const Text(
            'This is a system error. Download the diagnostic log and contact support.',
            style: TextStyle(color: PiccoloTheme.inkMuted, fontSize: 13),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 12),
          ExpansionTile(
            tilePadding: EdgeInsets.zero,
            childrenPadding: EdgeInsets.zero,
            title: const Text('Details', style: TextStyle(fontSize: 13)),
            children: [
              ConstrainedBox(
                constraints: const BoxConstraints(maxHeight: 120),
                child: SingleChildScrollView(
                  padding: const EdgeInsets.symmetric(vertical: 8),
                  child: SelectionArea(
                    child: Text(
                      error,
                      style: const TextStyle(fontSize: 12, color: PiccoloTheme.inkMuted),
                    ),
                  ),
                ),
              ),
            ],
          ),
          const SizedBox(height: 24),
          const DiagnosticLogDownload(
            apiPath: '/api/v1/system/diagnostic-log',
            showTitle: false,
            showDescription: false,
          ),
          const SizedBox(height: 16),
          FilledButton.icon(
            onPressed: onRetry,
            icon: const Icon(PiccoloIcons.refresh),
            label: const Text('Retry'),
            style: FilledButton.styleFrom(
              backgroundColor: PiccoloTheme.mist,
              foregroundColor: PiccoloTheme.ink,
            ),
          ),
        ],
      ),
    );
  }
}

class _ErrorStep extends StatelessWidget {

  const _ErrorStep({required this.error, required this.onRetry});
  final String error;
  final VoidCallback onRetry;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          const Icon(
            PiccoloIcons.wifiOff,
            color: PiccoloTheme.critical,
            size: 48,
          ),
          const SizedBox(height: 12),
          Text(
            "We couldn't reach Piccolo.",
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              fontWeight: FontWeight.w600,
            ),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 8),
          const Text(
            'Check that your device is online, then try again.',
            style: TextStyle(color: PiccoloTheme.inkMuted, fontSize: 13),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 12),
          ExpansionTile(
            tilePadding: EdgeInsets.zero,
            childrenPadding: EdgeInsets.zero,
            title: const Text('Details', style: TextStyle(fontSize: 13)),
            children: [
              SelectionArea(
                child: Text(
                  error,
                  style: const TextStyle(
                    fontSize: 12,
                    color: PiccoloTheme.inkMuted,
                  ),
                ),
              ),
            ],
          ),
          const SizedBox(height: 24),
          FilledButton.icon(
            onPressed: onRetry,
            icon: const Icon(PiccoloIcons.refresh),
            label: const Text('Retry'),
            style: FilledButton.styleFrom(
              backgroundColor: PiccoloTheme.mist,
              foregroundColor: PiccoloTheme.ink,
            ),
          ),
        ],
      ),
    );
  }
}

class _OtherDevicesPanel extends StatefulWidget {
  const _OtherDevicesPanel();

  @override
  State<_OtherDevicesPanel> createState() => _OtherDevicesPanelState();
}

class _OtherDevicesPanelState extends State<_OtherDevicesPanel> {
  final NetworkService _networkService = NetworkService(ApiClient());
  List<DiscoveredPeer> _peers = [];
  bool _isLoading = false;
  bool _hasLoaded = false;
  String? _error;
  DateTime? _lastFetch;

  Future<void> _fetchPeers() async {
    if (_isLoading) return;

    setState(() {
      _isLoading = true;
      _error = null;
    });

    try {
      final response = await _networkService.getPeers();
      if (mounted) {
        setState(() {
          _peers = response.peers;
          _isLoading = false;
          _hasLoaded = true;
          _lastFetch = DateTime.now();
        });
      }
    } on Object catch (e) {
      if (mounted) {
        setState(() {
          _isLoading = false;
          _hasLoaded = true;
          _error = 'Could not discover devices';
        });
      }
      debugPrint('Peer discovery error: $e');
    }
  }

  void _onExpansionChanged(bool expanded) {
    if (!expanded) return;

    // Fetch on first expand or if last fetch was > 30s ago
    final shouldRefetch = !_hasLoaded ||
        (_lastFetch != null &&
            DateTime.now().difference(_lastFetch!).inSeconds > 30);

    if (shouldRefetch) {
      unawaited(_fetchPeers());
    }
  }

  Future<void> _openPeerUrl(String url) async {
    final uri = Uri.parse(url);
    if (await canLaunchUrl(uri)) {
      await launchUrl(uri, mode: LaunchMode.externalApplication);
    }
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(16, 8, 16, 16),
      child: ExpansionTile(
        tilePadding: const EdgeInsets.symmetric(horizontal: 16),
        childrenPadding: const EdgeInsets.fromLTRB(16, 0, 16, 16),
        shape: RoundedRectangleBorder(
          borderRadius: BorderRadius.circular(Radii.md),
          side: const BorderSide(color: PiccoloTheme.hairline),
        ),
        collapsedShape: RoundedRectangleBorder(
          borderRadius: BorderRadius.circular(Radii.md),
          side: const BorderSide(color: PiccoloTheme.hairline),
        ),
        backgroundColor: PiccoloTheme.mist.withValues(alpha: 0.5),
        collapsedBackgroundColor: PiccoloTheme.mist.withValues(alpha: 0.3),
        onExpansionChanged: _onExpansionChanged,
        title: Row(
          children: [
            const Icon(
              PiccoloIcons.devices,
              size: 18,
              color: PiccoloTheme.inkMuted,
            ),
            const SizedBox(width: 8),
            const Text(
              'Other Devices on Network',
              style: TextStyle(
                fontSize: 13,
                color: PiccoloTheme.inkMuted,
                fontWeight: FontWeight.w500,
              ),
            ),
            if (_hasLoaded && _peers.isNotEmpty) ...[
              const SizedBox(width: 8),
              Container(
                padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 2),
                decoration: BoxDecoration(
                  color: PiccoloTheme.cobalt600.withValues(alpha: 0.1),
                  borderRadius: BorderRadius.circular(Radii.sm),
                ),
                child: Text(
                  '${_peers.length}',
                  style: const TextStyle(
                    fontSize: 11,
                    color: PiccoloTheme.cobalt600,
                    fontWeight: FontWeight.w600,
                  ),
                ),
              ),
            ],
          ],
        ),
        children: [
          if (_isLoading)
            const Padding(
              padding: EdgeInsets.all(16),
              child: SizedBox(
                width: 24,
                height: 24,
                child: CircularProgressIndicator(
                  strokeWidth: 2,
                  color: PiccoloTheme.cobalt600,
                ),
              ),
            )
          else if (_error != null)
            Padding(
              padding: const EdgeInsets.all(16),
              child: Column(
                children: [
                  Text(
                    _error!,
                    style: const TextStyle(
                      fontSize: 13,
                      color: PiccoloTheme.inkMuted,
                    ),
                  ),
                  const SizedBox(height: 8),
                  TextButton.icon(
                    onPressed: _fetchPeers,
                    icon: const Icon(PiccoloIcons.refresh, size: 16),
                    label: const Text('Retry'),
                  ),
                ],
              ),
            )
          else if (_peers.isEmpty)
            const Padding(
              padding: EdgeInsets.all(16),
              child: Text(
                'No other devices found on this network',
                style: TextStyle(
                  fontSize: 13,
                  color: PiccoloTheme.inkMuted,
                ),
              ),
            )
          else
            ..._peers.map(_buildPeerTile),
        ],
      ),
    );
  }

  Widget _buildPeerTile(DiscoveredPeer peer) {
    return InkWell(
      onTap: peer.online ? () => _openPeerUrl(peer.url) : null,
      borderRadius: BorderRadius.circular(Radii.sm),
      child: Padding(
        padding: const EdgeInsets.symmetric(vertical: 10, horizontal: 8),
        child: Row(
          children: [
            Container(
              width: 8,
              height: 8,
              decoration: BoxDecoration(
                color: peer.online ? PiccoloTheme.success : PiccoloTheme.inkMuted,
                shape: BoxShape.circle,
              ),
            ),
            const SizedBox(width: 12),
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text(
                    peer.displayName,
                    style: const TextStyle(
                      fontWeight: FontWeight.w500,
                      fontSize: 14,
                    ),
                  ),
                  Text(
                    peer.online
                        ? [peer.model, peer.ipv4].where((s) => s != null).join(' \u2022 ')
                        : '(offline)',
                    style: const TextStyle(
                      fontSize: 12,
                      color: PiccoloTheme.inkMuted,
                    ),
                  ),
                ],
              ),
            ),
            if (peer.online)
              const Icon(
                PiccoloIcons.arrowForwardIos,
                size: 14,
                color: PiccoloTheme.inkMuted,
              ),
          ],
        ),
      ),
    );
  }
}
