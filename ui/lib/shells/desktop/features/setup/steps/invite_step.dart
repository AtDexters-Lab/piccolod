import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/webauthn_service.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class InviteStep extends StatelessWidget {
  const InviteStep({
    required this.username,
    required this.onRegister,
    this.isLoading = false,
    this.isValidating = false,
    this.error,
    super.key,
  });
  final String? username;
  final Future<void> Function() onRegister;
  final bool isLoading;
  final bool isValidating;
  final String? error;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          if (isValidating)
            const Center(child: CircularProgressIndicator(color: PiccoloTheme.cobalt600))
          else if (error != null && username == null) ...[
            Text(error!, style: const TextStyle(color: PiccoloTheme.critical)),
          ] else ...[
            Text(
              'Welcome, ${username ?? 'there'}!',
              style: PiccoloTheme.textTheme.titleMedium,
            ),
            const SizedBox(height: 8),
            Text(
              'Set up your passkey to get started. This will be your fast, phishing-proof sign-in.',
              style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted),
            ),
            const SizedBox(height: 24),
            if (error != null)
              Padding(
                padding: const EdgeInsets.only(bottom: 16),
                child: Text(error!, style: const TextStyle(color: PiccoloTheme.critical)),
              ),
            if (WebAuthnService.isAvailable())
              FilledButton.icon(
                onPressed: isLoading ? null : onRegister,
                icon: const Icon(PiccoloIcons.fingerprint),
                label: isLoading
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
