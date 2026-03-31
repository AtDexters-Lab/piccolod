import 'package:flutter/material.dart';
import 'package:piccolo_os/shared/widgets/password_strength_indicator.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class PasswordSetForm extends StatefulWidget {

  const PasswordSetForm({
    required this.passwordController, required this.confirmController, super.key,
    this.passwordLabel = 'Password',
    this.confirmLabel = 'Confirm Password',
    this.passwordError,
    this.confirmError,
    this.onSubmitted,
    this.autofocus = false,
  });
  final TextEditingController passwordController;
  final TextEditingController confirmController;
  final String passwordLabel;
  final String confirmLabel;
  final String? passwordError;
  final String? confirmError;
  final VoidCallback? onSubmitted;
  final bool autofocus;

  @override
  State<PasswordSetForm> createState() => _PasswordSetFormState();
}

class _PasswordSetFormState extends State<PasswordSetForm> {
  bool _obscurePassword = true;
  bool _obscureConfirm = true;
  String? _internalConfirmError;

  @override
  void initState() {
    super.initState();
    // Listen to controllers for updates and validation
    widget.passwordController.addListener(_validate);
    widget.confirmController.addListener(_validate);
  }

  @override
  void dispose() {
    widget.passwordController.removeListener(_validate);
    widget.confirmController.removeListener(_validate);
    super.dispose();
  }

  void _validate() {
    final password = widget.passwordController.text;
    final confirm = widget.confirmController.text;

    String? error;
    if (confirm.isNotEmpty && password != confirm) {
      error = 'Passwords do not match';
    }

    if (mounted && _internalConfirmError != error) {
      setState(() {
        _internalConfirmError = error;
      });
    } else {
      // Just rebuild for strength indicator if no error change
      if (mounted) setState(() {});
    }
  }

  @override
  Widget build(BuildContext context) {
    return Column(
      mainAxisSize: MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        TextField(
          controller: widget.passwordController,
          obscureText: _obscurePassword,
          autofocus: widget.autofocus,
          autofillHints: const [AutofillHints.newPassword],
          decoration: InputDecoration(
            labelText: widget.passwordLabel,
            border: OutlineInputBorder(borderRadius: BorderRadius.circular(Radii.sm)),
            errorText: widget.passwordError,
            suffixIcon: IconButton(
              icon: Icon(
                _obscurePassword
                    ? PiccoloIcons.visibilityOff
                    : PiccoloIcons.visibility,
                color: PiccoloTheme.inkMuted,
              ),
              onPressed: () => setState(() => _obscurePassword = !_obscurePassword),
            ),
          ),
        ),
        PasswordStrengthIndicator(password: widget.passwordController.text),
        const SizedBox(height: 16),
        TextField(
          controller: widget.confirmController,
          obscureText: _obscureConfirm,
          autofillHints: const [AutofillHints.newPassword],
          decoration: InputDecoration(
            labelText: widget.confirmLabel,
            border: OutlineInputBorder(borderRadius: BorderRadius.circular(Radii.sm)),
            // Prefer widget-provided error, fallback to internal validation error
            errorText: widget.confirmError ?? _internalConfirmError,
            suffixIcon: IconButton(
              icon: Icon(
                _obscureConfirm
                    ? PiccoloIcons.visibilityOff
                    : PiccoloIcons.visibility,
                color: PiccoloTheme.inkMuted,
              ),
              onPressed: () => setState(() => _obscureConfirm = !_obscureConfirm),
            ),
          ),
          onSubmitted: widget.onSubmitted != null
              ? (_) => widget.onSubmitted!()
              : null,
        ),
      ],
    );
  }
}
