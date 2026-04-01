import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/webauthn_service.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class PasskeyRequiredStep extends StatefulWidget {
  const PasskeyRequiredStep({required this.onRegister, this.error, super.key});
  final Future<bool> Function() onRegister;
  final String? error;

  @override
  State<PasskeyRequiredStep> createState() => _PasskeyRequiredStepState();
}

class _PasskeyRequiredStepState extends State<PasskeyRequiredStep> {
  bool _isLoading = false;
  String? _error;

  @override
  void initState() {
    super.initState();
    _error = widget.error;
  }

  @override
  void didUpdateWidget(PasskeyRequiredStep oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (widget.error != oldWidget.error) {
      setState(() => _error = widget.error);
    }
  }

  Future<void> _register() async {
    setState(() { _isLoading = true; _error = null; });
    final success = await widget.onRegister();
    if (mounted && !success) {
      setState(() {
        _isLoading = false;
        _error = widget.error;
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
            'A passkey lets you sign in quickly without typing your encryption password. '
            'When prompted, save it on a device you carry with you \u2014 like your phone \u2014 so you can sign in from any network.',
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
