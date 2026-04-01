import 'package:flutter/material.dart';
import 'package:piccolo_os/shared/widgets/password_set_form.dart';
import 'package:piccolo_os/shared/widgets/password_strength_indicator.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class CredentialsStep extends StatefulWidget {
  const CredentialsStep({
    required this.onSubmit,
    this.error,
    this.onBack,
    super.key,
  });
  final Future<bool> Function(String) onSubmit;
  final String? error;
  final VoidCallback? onBack;

  @override
  State<CredentialsStep> createState() => _CredentialsStepState();
}

class _CredentialsStepState extends State<CredentialsStep> {
  final TextEditingController _passController = TextEditingController();
  final TextEditingController _confirmController = TextEditingController();
  String? _error;
  String? _confirmError;
  bool _isSubmitting = false;

  @override
  void initState() {
    super.initState();
    _error = widget.error;
  }

  @override
  void didUpdateWidget(CredentialsStep oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (widget.error != oldWidget.error && widget.error != null) {
      setState(() => _error = widget.error);
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

    final pass = _passController.text;
    if (pass.isEmpty) {
      setState(() => _error = 'Password is required');
      return;
    }
    final policyError = PasswordPolicy.validate(pass);
    if (policyError != null) {
      setState(() => _error = policyError);
      return;
    }
    if (pass != _confirmController.text) {
      setState(() => _confirmError = 'Passwords do not match');
      return;
    }

    setState(() => _isSubmitting = true);
    final success = await widget.onSubmit(pass);

    if (mounted && !success) {
      setState(() {
        _isSubmitting = false;
        _error = widget.error ?? 'Setup failed. Please try again.';
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
              passwordLabel: 'Encryption password',
              confirmLabel: 'Confirm password',
              passwordError: _error,
              confirmError: _confirmError,
              onSubmitted: _submit,
              autofocus: true,
            ),
            const SizedBox(height: 16),
            const Text(
              'This password protects your encrypted storage \u2014 you\u2019ll need it after each reboot. Store it somewhere safe.',
              style: TextStyle(color: PiccoloTheme.inkMuted, fontSize: 13),
            ),
            const SizedBox(height: 32),
            Row(
              children: [
                if (widget.onBack != null) ...[
                  TextButton(
                    onPressed: _isSubmitting ? null : widget.onBack,
                    child: const Text('Back'),
                  ),
                  const SizedBox(width: 16),
                ],
                Expanded(
                  child: FilledButton(
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
                        : const Text('Encrypt & Continue'),
                  ),
                ),
              ],
            ),
          ],
        ),
      ),
    );
  }
}
