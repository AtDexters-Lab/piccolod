import 'package:flutter/material.dart';
import '../../core/services/api_client.dart';
import '../../theme/piccolo_icons.dart';
import '../../theme/piccolo_theme.dart';

/// Compact re-authentication overlay displayed when the portal session expires.
///
/// Renders a scrim + centered card with username (pre-filled) and password fields.
/// "Log In" re-authenticates inline; "Log Out" cancels and triggers a full logout.
class ReauthOverlay extends StatefulWidget {
  final String? lastKnownUsername;
  final VoidCallback onSuccess;
  final VoidCallback onCancel;

  const ReauthOverlay({
    super.key,
    this.lastKnownUsername,
    required this.onSuccess,
    required this.onCancel,
  });

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
      ApiClient().completeReauth(true);
      widget.onSuccess();
    } on ApiException catch (e) {
      setState(() {
        _isLoading = false;
        _error = e.statusCode == 401
            ? 'Invalid credentials'
            : 'Login failed (${e.statusCode})';
      });
    } catch (e) {
      setState(() {
        _isLoading = false;
        _error = 'Connection error';
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context).textTheme;

    return Container(
      color: PiccoloTheme.scrim,
      alignment: Alignment.center,
      child: Container(
        width: 380,
        padding: const EdgeInsets.all(28),
        decoration: BoxDecoration(
          color: PiccoloTheme.porcelain,
          borderRadius: BorderRadius.circular(Radii.lg),
          boxShadow: Elevation.elev4,
        ),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            // Header
            Row(
              children: [
                Icon(PiccoloIcons.lock,
                    size: 20, color: PiccoloTheme.inkMuted),
                const SizedBox(width: 8),
                Text('Session Expired', style: theme.titleMedium),
              ],
            ),
            const SizedBox(height: 8),
            Text(
              'Your session has expired. Log in again to continue.',
              style: theme.bodySmall
                  ?.copyWith(color: PiccoloTheme.inkMuted),
            ),
            const SizedBox(height: 20),

            // Username
            TextField(
              controller: _usernameController,
              decoration: const InputDecoration(
                labelText: 'Username',
                isDense: true,
              ),
              textInputAction: TextInputAction.next,
              enabled: !_isLoading,
            ),
            const SizedBox(height: 12),

            // Password
            TextField(
              controller: _passwordController,
              obscureText: _obscurePassword,
              decoration: InputDecoration(
                labelText: 'Password',
                isDense: true,
                suffixIcon: IconButton(
                  icon: Icon(
                    _obscurePassword
                        ? PiccoloIcons.visibility
                        : PiccoloIcons.visibilityOff,
                    size: 18,
                  ),
                  onPressed: () =>
                      setState(() => _obscurePassword = !_obscurePassword),
                ),
              ),
              textInputAction: TextInputAction.done,
              onSubmitted: _isLoading ? null : (_) => _submit(),
              enabled: !_isLoading,
            ),
            const SizedBox(height: 8),

            // Error
            if (_error != null) ...[
              Text(
                _error!,
                style: theme.bodySmall
                    ?.copyWith(color: PiccoloTheme.critical),
              ),
              const SizedBox(height: 8),
            ],

            const SizedBox(height: 12),

            // Actions
            Row(
              mainAxisAlignment: MainAxisAlignment.end,
              children: [
                TextButton(
                  onPressed: _isLoading
                      ? null
                      : () {
                          ApiClient().completeReauth(false);
                          widget.onCancel();
                        },
                  child: const Text('Log Out'),
                ),
                const SizedBox(width: 8),
                FilledButton(
                  onPressed: _isLoading ? null : _submit,
                  child: _isLoading
                      ? const SizedBox(
                          width: 16,
                          height: 16,
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
    );
  }
}
