import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Re-authentication overlay displayed when the portal session expires.
///
/// Renders a scrim + centered dialog (matching the SetupWizard chrome) with
/// username (pre-filled) and password fields.
/// "Log In" re-authenticates inline; "Log Out" cancels and triggers a full logout.
class ReauthOverlay extends StatefulWidget {

  const ReauthOverlay({
    required this.onSuccess, required this.onCancel, super.key,
    this.lastKnownUsername,
  });
  final String? lastKnownUsername;
  final VoidCallback onSuccess;
  final VoidCallback onCancel;

  @override
  State<ReauthOverlay> createState() => _ReauthOverlayState();
}

class _ReauthOverlayState extends State<ReauthOverlay> {
  late final TextEditingController _usernameController;
  final TextEditingController _passwordController = TextEditingController();
  bool _obscurePassword = true;
  bool _isLoading = false;
  String? _error;

  @override
  void initState() {
    super.initState();
    _usernameController =
        TextEditingController(text: widget.lastKnownUsername ?? '');
  }

  @override
  void dispose() {
    _usernameController.dispose();
    _passwordController.dispose();
    super.dispose();
  }

  Future<void> _submit() async {
    if (_isLoading) return;
    final username = _usernameController.text.trim();
    final password = _passwordController.text;
    if (username.isEmpty || password.isEmpty) {
      setState(() => _error = 'Username and password required');
      return;
    }

    setState(() {
      _isLoading = true;
      _error = null;
    });

    try {
      await ApiClient().post('/api/v1/auth/login', body: {
        'username': username,
        'password': password,
      });
      await ApiClient().fetchCsrfToken();
      ApiClient().completeReauth(success: true);
      widget.onSuccess();
    } on ApiException catch (e) {
      setState(() {
        _isLoading = false;
        _error = e.statusCode == 401
            ? 'Invalid credentials'
            : 'Login failed (${e.statusCode})';
      });
    } on Object catch (_) {
      setState(() {
        _isLoading = false;
        _error = 'Connection error';
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    return ColoredBox(
      color: PiccoloTheme.scrim,
      child: LayoutBuilder(
        builder: (context, constraints) {
          final dialogWidth =
              (constraints.maxWidth * 0.9).clamp(360.0, 480.0);

          return Center(
            child: Container(
              width: dialogWidth,
              decoration: BoxDecoration(
                color: PiccoloTheme.porcelain,
                borderRadius: BorderRadius.circular(Radii.lg),
                boxShadow: [
                  BoxShadow(
                    color: PiccoloTheme.scrim.withValues(alpha: 0.2),
                    blurRadius: 40,
                    offset: const Offset(0, 20),
                  ),
                ],
                border: Border.all(color: PiccoloTheme.porcelain),
              ),
              child: Column(
                mainAxisSize: MainAxisSize.min,
                children: [
                  // Header
                  Padding(
                    padding: const EdgeInsets.fromLTRB(24, 24, 24, 16),
                    child: Text(
                      'Session Expired',
                      style: PiccoloTheme.textTheme.bodyMedium
                          ?.copyWith(color: PiccoloTheme.inkMuted),
                    ),
                  ),

                  // Form
                  Padding(
                    padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
                    child: Column(
                      mainAxisSize: MainAxisSize.min,
                      crossAxisAlignment: CrossAxisAlignment.stretch,
                      children: [
                        TextField(
                          controller: _usernameController,
                          autofillHints: const [AutofillHints.username],
                          decoration: const InputDecoration(
                            labelText: 'Username',
                          ),
                          textInputAction: TextInputAction.next,
                          enabled: !_isLoading,
                        ),
                        const SizedBox(height: 16),
                        TextField(
                          controller: _passwordController,
                          obscureText: _obscurePassword,
                          autofillHints: const [AutofillHints.password],
                          decoration: InputDecoration(
                            labelText: 'Password',
                            errorText: _error,
                            suffixIcon: IconButton(
                              icon: Icon(
                                _obscurePassword
                                    ? PiccoloIcons.visibilityOff
                                    : PiccoloIcons.visibility,
                                color: PiccoloTheme.inkMuted,
                              ),
                              onPressed: () => setState(
                                () => _obscurePassword = !_obscurePassword,
                              ),
                            ),
                          ),
                          textInputAction: TextInputAction.done,
                          onSubmitted: _isLoading ? null : (_) => _submit(),
                          enabled: !_isLoading,
                        ),
                        const SizedBox(height: 24),
                        Row(
                          children: [
                            TextButton(
                              onPressed: _isLoading
                                  ? null
                                  : () {
                                      ApiClient()
                                          .completeReauth(success: false);
                                      widget.onCancel();
                                    },
                              child: const Text('Log Out'),
                            ),
                            const Spacer(),
                            FilledButton(
                              onPressed: _isLoading ? null : _submit,
                              child: _isLoading
                                  ? const SizedBox(
                                      width: 20,
                                      height: 20,
                                      child: CircularProgressIndicator(
                                        strokeWidth: 2,
                                        color: Colors.white,
                                      ),
                                    )
                                  : const Text('Log In'),
                            ),
                          ],
                        ),
                      ],
                    ),
                  ),
                ],
              ),
            ),
          );
        },
      ),
    );
  }
}
