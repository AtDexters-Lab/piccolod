import 'dart:async';
import 'dart:js_interop';

import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:web/web.dart' as web;

class InstallCompleteStep extends StatefulWidget {
  const InstallCompleteStep({
    required this.onReboot,
    this.bootOrderConfigured = false,
    super.key,
  });
  final Future<void> Function() onReboot;
  final bool bootOrderConfigured;

  @override
  State<InstallCompleteStep> createState() => _InstallCompleteStepState();
}

enum _RebootPhase { idle, rebooting, polling, timedOut }

class _InstallCompleteStepState extends State<InstallCompleteStep> {
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
      setState(() => _phase = _RebootPhase.polling);
      await Future<void>.delayed(_initialDelay);
      if (!mounted) return;
      unawaited(_pollDevice());
      _pollTimer = Timer.periodic(_pollInterval, (_) {
        unawaited(_pollDevice());
      });
      _timeoutTimer = Timer(_pollTimeout, () {
        _pollTimer?.cancel();
        if (mounted) setState(() => _phase = _RebootPhase.timedOut);
      });
    }
  }

  Future<void> _pollDevice() async {
    if (_pollInFlight) return;
    _pollInFlight = true;
    try {
      final init = web.RequestInit(mode: 'no-cors');
      final response = await web.window
          .fetch(_pollUrl.toJS, init)
          .toDart
          .timeout(const Duration(seconds: 3));
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
