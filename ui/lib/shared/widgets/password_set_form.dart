import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import '../../theme/piccolo_theme.dart';
import 'password_strength_indicator.dart';

class PasswordSetForm extends StatefulWidget {
  final TextEditingController passwordController;
  final TextEditingController confirmController;
  final String passwordLabel;
  final String confirmLabel;
  final String? passwordError;
  final String? confirmError;
  final VoidCallback? onSubmitted;

  const PasswordSetForm({
    super.key,
    required this.passwordController,
    required this.confirmController,
    this.passwordLabel = "Password",
    this.confirmLabel = "Confirm Password",
    this.passwordError,
    this.confirmError,
    this.onSubmitted,
  });

  @override
  State<PasswordSetForm> createState() => _PasswordSetFormState();
}

class _PasswordSetFormState extends State<PasswordSetForm> {
  bool _obscureText = true;

  @override
  void initState() {
    super.initState();
    // Listen to password changes to update strength indicator
    widget.passwordController.addListener(_update);
  }

  @override
  void dispose() {
    widget.passwordController.removeListener(_update);
    super.dispose();
  }

  void _update() {
    if (mounted) setState(() {});
  }

  void _toggleVisibility() {
    setState(() {
      _obscureText = !_obscureText;
    });
  }

  @override
  Widget build(BuildContext context) {
    return Column(
      mainAxisSize: MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        TextField(
          controller: widget.passwordController,
          obscureText: _obscureText,
          autofillHints: const [AutofillHints.newPassword],
          decoration: InputDecoration(
            labelText: widget.passwordLabel,
            border: OutlineInputBorder(borderRadius: BorderRadius.circular(12)),
            filled: true,
            fillColor: Colors.white,
            errorText: widget.passwordError,
            suffixIcon: IconButton(
              icon: Icon(
                _obscureText
                    ? Icons.visibility_off_outlined
                    : Icons.visibility_outlined,
                color: PiccoloTheme.inkMuted,
              ),
              onPressed: _toggleVisibility,
            ),
          ),
        ),
        PasswordStrengthIndicator(password: widget.passwordController.text),
        const SizedBox(height: 16),
        TextField(
          controller: widget.confirmController,
          obscureText: _obscureText,
          autofillHints: const [AutofillHints.newPassword],
          decoration: InputDecoration(
            labelText: widget.confirmLabel,
            border: OutlineInputBorder(borderRadius: BorderRadius.circular(12)),
            filled: true,
            fillColor: Colors.white,
            errorText: widget.confirmError,
            suffixIcon: IconButton(
              icon: Icon(
                _obscureText
                    ? Icons.visibility_off_outlined
                    : Icons.visibility_outlined,
                color: PiccoloTheme.inkMuted,
              ),
              onPressed: _toggleVisibility,
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
