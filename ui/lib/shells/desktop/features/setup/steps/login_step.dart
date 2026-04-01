import 'package:flutter/material.dart';
import 'package:piccolo_os/shared/widgets/login_form_fields.dart';


class LoginStep extends StatefulWidget {
  const LoginStep({
    required this.onLogin,
    required this.onLoginWithPasskey,
    required this.onForgotPassword,
    this.loginMethods,
    this.error,
    this.passkeyError,
    super.key,
  });
  final Future<bool> Function(String, String) onLogin;
  final Future<bool> Function() onLoginWithPasskey;
  final VoidCallback onForgotPassword;
  final List<String>? loginMethods;
  final String? error;
  final String? passkeyError;

  @override
  State<LoginStep> createState() => _LoginStepState();
}

class _LoginStepState extends State<LoginStep> {
  final TextEditingController _userController = TextEditingController(
    text: 'admin',
  );
  final TextEditingController _passController = TextEditingController();
  bool _isSubmitting = false;
  String? _error;
  String? _passkeyError;

  @override
  void initState() {
    super.initState();
    _error = widget.error;
    _passkeyError = widget.passkeyError;
  }

  @override
  void didUpdateWidget(LoginStep oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (widget.error != oldWidget.error) {
      setState(() => _error = widget.error);
    }
    if (widget.passkeyError != oldWidget.passkeyError) {
      setState(() => _passkeyError = widget.passkeyError);
    }
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
      _passkeyError = null;
    });

    final success = await widget.onLogin(
      _userController.text,
      _passController.text,
    );

    if (mounted && !success) {
      setState(() {
        _isSubmitting = false;
        _error = widget.error ?? 'Invalid credentials';
      });
    }
  }

  Future<void> _loginWithPasskey() async {
    setState(() {
      _isSubmitting = true;
      _error = null;
      _passkeyError = null;
    });

    final success = await widget.onLoginWithPasskey();

    if (mounted && !success) {
      setState(() {
        _isSubmitting = false;
        _passkeyError = widget.passkeyError;
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    final methods = widget.loginMethods;
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
            passkeyError: _passkeyError,
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
