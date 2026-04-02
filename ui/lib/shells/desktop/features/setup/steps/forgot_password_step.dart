import 'package:flutter/material.dart';
import 'package:piccolo_os/shared/widgets/password_set_form.dart';
import 'package:piccolo_os/shared/widgets/password_strength_indicator.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class ForgotPasswordStep extends StatefulWidget {
  const ForgotPasswordStep({required this.onReset, required this.onCancel, this.error, super.key});
  final Future<bool> Function(String, String) onReset;
  final VoidCallback onCancel;
  final String? error;

  @override
  State<ForgotPasswordStep> createState() => _ForgotPasswordStepState();
}

class _ForgotPasswordStepState extends State<ForgotPasswordStep> {
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

    final pass = _passController.text;
    final policyError = PasswordPolicy.validate(pass);
    if (policyError != null) {
      setState(() => _generalError = policyError);
      return;
    }

    if (pass != _confirmController.text) {
      setState(() => _confirmError = 'Passwords do not match');
      return;
    }

    setState(() => _isSubmitting = true);

    final success = await widget.onReset(
      _keyController.text,
      _passController.text,
    );

    if (mounted && !success) {
      setState(() {
        _isSubmitting = false;
        _keyError = widget.error ?? 'Invalid recovery key';
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
            'Enter your 24-word recovery key to reset your encryption password.',
            style: TextStyle(color: PiccoloTheme.inkMuted, fontSize: 13),
          ),
          const SizedBox(height: 16),
          TextField(
            controller: _keyController,
            maxLines: 3,
            autofocus: true,
            decoration: InputDecoration(
              labelText: 'Recovery Key',
              hintText: 'Enter all 24 words separated by spaces',
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
