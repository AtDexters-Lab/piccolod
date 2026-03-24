import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/webauthn_service.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Non-blocking prompt suggesting the user register a passkey.
/// Shown as a banner at the top of the desktop when has_passkey is false.
class PasskeyPromptOverlay extends StatefulWidget {
  const PasskeyPromptOverlay({
    required this.onRegistered,
    required this.onDismiss,
    super.key,
  });

  final VoidCallback onRegistered;
  final VoidCallback onDismiss;

  @override
  State<PasskeyPromptOverlay> createState() => _PasskeyPromptOverlayState();
}

class _PasskeyPromptOverlayState extends State<PasskeyPromptOverlay> {
  bool _isRegistering = false;
  bool _success = false;
  String? _error;

  Future<void> _register() async {
    if (!WebAuthnService.isAvailable()) {
      setState(() => _error = 'Passkeys require HTTPS. Install the Piccolo CA certificate first.');
      return;
    }

    setState(() {
      _isRegistering = true;
      _error = null;
    });

    try {
      final beginResult = await ApiClient().beginPasskeyRegistration();
      final sessionId = beginResult['session_id'] as String;
      final options = beginResult['publicKey'] as Map<String, dynamic>;

      final credential = await WebAuthnService.createCredential(options);

      await ApiClient().finishPasskeyRegistration(sessionId, credential);
      if (mounted) {
        setState(() {
          _isRegistering = false;
          _error = null;
          _success = true;
        });
        // Show success briefly, then dismiss
        await Future<void>.delayed(const Duration(seconds: 2));
        widget.onRegistered();
      }
      return;
    } on Object catch (e) {
      if (mounted) {
        final msg = e.toString();
        setState(() {
          _isRegistering = false;
          if (msg.contains('InvalidStateError') || msg.contains('already registered')) {
            _error = 'This authenticator already has a passkey registered. Try a different one or dismiss.';
          } else if (msg.contains('NotAllowedError') || msg.contains('cancelled')) {
            _error = 'Cancelled or timed out. You can try again or set up later in Settings.';
          } else {
            _error = 'Failed to register passkey.';
          }
        });
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    return Align(
      alignment: Alignment.topCenter,
      child: Padding(
        padding: const EdgeInsets.only(top: 48),
        child: Material(
          elevation: 8,
          borderRadius: BorderRadius.circular(Radii.lg),
          color: PiccoloTheme.porcelain,
          child: Container(
            width: 420,
            padding: const EdgeInsets.all(20),
            child: Column(
              mainAxisSize: MainAxisSize.min,
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                Row(
                  children: [
                    const Icon(PiccoloIcons.fingerprint, color: PiccoloTheme.cobalt600, size: 24),
                    const SizedBox(width: 12),
                    Expanded(
                      child: Text(
                        'Set up fast sign-in',
                        style: PiccoloTheme.textTheme.titleSmall,
                      ),
                    ),
                    IconButton(
                      icon: const Icon(PiccoloIcons.close, size: 18, color: PiccoloTheme.inkMuted),
                      onPressed: widget.onDismiss,
                      padding: EdgeInsets.zero,
                      constraints: const BoxConstraints(),
                    ),
                  ],
                ),
                const SizedBox(height: 8),
                Text(
                  'Register a passkey for fast, phishing-proof sign-in. '
                  'Your password remains available for disk unlock and recovery.',
                  style: PiccoloTheme.textTheme.bodySmall?.copyWith(color: PiccoloTheme.inkMuted),
                ),
                const SizedBox(height: 16),
                if (_success) ...[
                  Row(
                    children: [
                      Icon(PiccoloIcons.shieldCheck, color: PiccoloTheme.success, size: 18),
                      const SizedBox(width: 8),
                      Text('Passkey registered successfully!',
                          style: PiccoloTheme.textTheme.bodySmall?.copyWith(color: PiccoloTheme.success)),
                    ],
                  ),
                ] else ...[
                if (_error != null)
                  Padding(
                    padding: const EdgeInsets.only(bottom: 12),
                    child: Text(_error!, style: PiccoloTheme.textTheme.bodySmall?.copyWith(color: PiccoloTheme.critical)),
                  ),
                Row(
                  mainAxisAlignment: MainAxisAlignment.end,
                  children: [
                    TextButton(
                      onPressed: widget.onDismiss,
                      child: const Text('Later'),
                    ),
                    const SizedBox(width: 8),
                    FilledButton.icon(
                      onPressed: _isRegistering ? null : _register,
                      icon: _isRegistering
                          ? const SizedBox(
                              width: 16, height: 16,
                              child: CircularProgressIndicator(strokeWidth: 2, color: Colors.white),
                            )
                          : const Icon(PiccoloIcons.fingerprint, size: 18),
                      label: const Text('Create Passkey'),
                    ),
                  ],
                ),
                ], // end else (not _success)
              ],
            ),
          ),
        ),
      ),
    );
  }
}
