import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/webauthn_service.dart';
import 'package:piccolo_os/shared/widgets/login_form_fields.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Re-authentication overlay displayed when the portal session expires.
///
/// Renders a scrim + centered dialog (matching the SetupWizard chrome) with
/// login fields gated by the server's login-options endpoint.
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
  bool _isLoading = false;
  String? _error;
  List<String>? _methods;

  @override
  void initState() {
    super.initState();
    _usernameController =
        TextEditingController(text: widget.lastKnownUsername ?? '');
    unawaited(_fetchLoginOptions());
  }

  Future<void> _fetchLoginOptions() async {
    final api = ApiClient();
    for (var attempt = 0; attempt < 3; attempt++) {
      try {
        final result = await api
            .getLoginOptions()
            .timeout(const Duration(seconds: 5));
        if (mounted) {
          setState(
              () => _methods = List<String>.from(result['methods'] as List));
        }
        return;
      } on Object catch (_) {
        if (!mounted) return;
        if (attempt < 2) {
          await Future<void>.delayed(Duration(seconds: 1 << attempt));
          if (!mounted) return;
        }
      }
    }
    if (mounted) setState(() => _methods = ['password']);
  }

  Future<void> _loginWithPasskey() async {
    if (_isLoading) return;
    setState(() { _isLoading = true; _error = null; });

    try {
      final beginResult = await ApiClient().beginPasskeyLogin();
      final sessionId = beginResult['session_id'] as String;
      final options = beginResult['publicKey'] as Map<String, dynamic>;

      final credential = await WebAuthnService.getCredential(options);

      await ApiClient().finishPasskeyLogin(sessionId, credential);
      await ApiClient().fetchCsrfToken();
      ApiClient().completeReauth(success: true);
      widget.onSuccess();
    } on ApiException catch (e) {
      if (_handleLockedError(e)) return;
      if (mounted) {
        setState(() {
          _isLoading = false;
          _error = 'Passkey login failed';
        });
      }
    } on Object catch (e) {
      if (mounted) {
        setState(() {
          _isLoading = false;
          final msg = e.toString();
          if (msg.contains('NotAllowedError') || msg.contains('cancelled')) {
            _error = 'Cancelled or timed out. If using a phone, ensure Bluetooth is on and devices are nearby.';
          } else {
            _error = 'Passkey login failed';
          }
        });
      }
    }
  }

  /// Returns true (and cancels the overlay) if [e] is a 423 Locked error.
  /// Storage is locked — reauth can't help; transition to SetupWizard.
  bool _handleLockedError(ApiException e) {
    if (e.statusCode != 423) return false;
    ApiClient().completeReauth(success: false);
    widget.onCancel();
    return true;
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
      if (_handleLockedError(e)) return;
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
                        LoginFormFields(
                          methods: _methods,
                          usernameController: _usernameController,
                          passwordController: _passwordController,
                          onSubmitPassword: _submit,
                          onSubmitPasskey: _loginWithPasskey,
                          isLoading: _isLoading,
                          error: _error,
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
                            if (LoginFormFields.isPasswordAvailable(_methods))
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
