import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class UnlockStep extends StatefulWidget {
  const UnlockStep({
    required this.onUnlock,
    required this.onForgotPassword,
    this.error,
    super.key,
  });
  final Future<bool> Function(String) onUnlock;
  final VoidCallback onForgotPassword;
  final String? error;

  @override
  State<UnlockStep> createState() => _UnlockStepState();
}

class _UnlockStepState extends State<UnlockStep> {
  final TextEditingController _passController = TextEditingController();
  bool _isSubmitting = false;
  bool _obscureText = true;
  String? _error;

  @override
  void initState() {
    super.initState();
    _error = widget.error;
  }

  @override
  void didUpdateWidget(UnlockStep oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (widget.error != oldWidget.error && widget.error != null) {
      setState(() => _error = widget.error);
    }
  }

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
        _error = widget.error ?? 'Incorrect password';
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
            autofocus: true,
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
