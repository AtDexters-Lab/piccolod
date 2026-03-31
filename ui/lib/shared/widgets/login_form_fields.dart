import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/webauthn_service.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Shared login form fields used by both the setup wizard login step
/// and the session-expired re-authentication overlay.
///
/// Renders passkey button, "or" divider, and username/password fields
/// based on the available [methods]. Callers provide their own action
/// buttons (Log In, Log Out, Forgot Password, etc.) below this widget.
class LoginFormFields extends StatefulWidget {
  const LoginFormFields({
    required this.methods,
    required this.usernameController,
    required this.passwordController,
    required this.onSubmitPassword,
    required this.onSubmitPasskey,
    required this.isLoading,
    this.error,
    this.passkeyError,
    this.unavailableMessage,
    super.key,
  });

  /// Available login methods (e.g., `['passkey', 'password']`).
  /// When null (not yet loaded), a loading indicator is shown.
  /// An empty list means no methods are available.
  final List<String>? methods;
  final TextEditingController usernameController;
  final TextEditingController passwordController;
  final VoidCallback onSubmitPassword;
  final VoidCallback onSubmitPasskey;
  final bool isLoading;
  final String? error;

  /// Error from a passkey operation, shown near the passkey button.
  final String? passkeyError;

  /// Message shown when neither passkey nor password is available.
  final String? unavailableMessage;

  /// Whether password login should be shown for the given [methods].
  /// Returns false when [methods] is null (still loading).
  static bool isPasswordAvailable(List<String>? methods) =>
      methods?.contains('password') ?? false;

  /// Whether passkey login should be shown for the given [methods].
  static bool isPasskeyAvailable(List<String>? methods) =>
      (methods?.contains('passkey') ?? false) && WebAuthnService.isAvailable();

  @override
  State<LoginFormFields> createState() => _LoginFormFieldsState();
}

class _LoginFormFieldsState extends State<LoginFormFields> {
  bool _obscurePassword = true;

  @override
  Widget build(BuildContext context) {
    // Methods not yet loaded — show a compact spinner.
    if (widget.methods == null) {
      return const Padding(
        padding: EdgeInsets.symmetric(vertical: 24),
        child: Center(
          child: SizedBox(
            width: 24,
            height: 24,
            child: CircularProgressIndicator(strokeWidth: 2),
          ),
        ),
      );
    }

    final showPasskey = LoginFormFields.isPasskeyAvailable(widget.methods);
    final showPassword = LoginFormFields.isPasswordAvailable(widget.methods);

    return Column(
      mainAxisSize: MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        if (showPasskey) ...[
          FilledButton.icon(
            onPressed: widget.isLoading ? null : widget.onSubmitPasskey,
            icon: const Icon(PiccoloIcons.fingerprint),
            label: const Text('Sign in with Passkey'),
          ),
          if (widget.passkeyError != null) ...[
            const SizedBox(height: 8),
            Text(
              widget.passkeyError!,
              style: PiccoloTheme.textTheme.bodySmall
                  ?.copyWith(color: PiccoloTheme.critical),
            ),
          ],
          if (showPassword) ...[
            const SizedBox(height: 16),
            Row(
              children: [
                const Expanded(child: Divider()),
                Padding(
                  padding: const EdgeInsets.symmetric(horizontal: 12),
                  child: Text(
                    'or',
                    style: PiccoloTheme.textTheme.bodySmall
                        ?.copyWith(color: PiccoloTheme.inkMuted),
                  ),
                ),
                const Expanded(child: Divider()),
              ],
            ),
            const SizedBox(height: 16),
          ],
        ],
        if (showPassword) ...[
          TextField(
            controller: widget.usernameController,
            autofillHints: const [AutofillHints.username],
            decoration: const InputDecoration(labelText: 'Username'),
            textInputAction: TextInputAction.next,
            enabled: !widget.isLoading,
          ),
          const SizedBox(height: 16),
          TextField(
            controller: widget.passwordController,
            obscureText: _obscurePassword,
            autofillHints: const [AutofillHints.password],
            decoration: InputDecoration(
              labelText: 'Password',
              errorText: widget.error,
              suffixIcon: IconButton(
                icon: Icon(
                  _obscurePassword
                      ? PiccoloIcons.visibilityOff
                      : PiccoloIcons.visibility,
                  color: PiccoloTheme.inkMuted,
                ),
                onPressed: () =>
                    setState(() => _obscurePassword = !_obscurePassword),
              ),
            ),
            textInputAction: TextInputAction.done,
            onSubmitted:
                widget.isLoading ? null : (_) => widget.onSubmitPassword(),
            enabled: !widget.isLoading,
          ),
        ] else if (widget.error != null) ...[
          Text(widget.error!,
              style: const TextStyle(color: PiccoloTheme.critical)),
        ] else if (!showPasskey) ...[
          Text(
            widget.unavailableMessage ??
                'Passkey sign-in is required but unavailable in this '
                    'browser context.',
            style: PiccoloTheme.textTheme.bodyMedium
                ?.copyWith(color: PiccoloTheme.inkMuted),
          ),
        ],
      ],
    );
  }
}
