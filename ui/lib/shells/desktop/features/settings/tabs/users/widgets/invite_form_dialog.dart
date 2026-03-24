import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Dialog for creating a user invite (passkey-based onboarding, no password).
class InviteFormDialog extends StatefulWidget {

  const InviteFormDialog({
    required this.onInvite,
    required this.onShowInviteLink,
    super.key,
  });

  /// Called with (username, email, allowedApps). Returns the invite token.
  final Future<String> Function(
    String username,
    String email,
    List<String> allowedApps,
  ) onInvite;

  /// Called after a successful invite to display the invite link.
  final void Function(String token) onShowInviteLink;

  @override
  State<InviteFormDialog> createState() => _InviteFormDialogState();
}

class _InviteFormDialogState extends State<InviteFormDialog> {
  final _formKey = GlobalKey<FormState>();
  final _usernameController = TextEditingController();
  final _emailController = TextEditingController();
  final _allowedAppsController = TextEditingController();

  bool _isLoading = false;
  String? _error;

  @override
  void dispose() {
    _usernameController.dispose();
    _emailController.dispose();
    _allowedAppsController.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: const Text('Invite User'),
      content: SizedBox(
        width: 400,
        child: Form(
          key: _formKey,
          child: SingleChildScrollView(
            child: Column(
              mainAxisSize: MainAxisSize.min,
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                if (_error != null) ...[
                  Container(
                    padding: const EdgeInsets.all(12),
                    decoration: BoxDecoration(
                      color: PiccoloTheme.critical.withValues(alpha: 0.1),
                      borderRadius: BorderRadius.circular(Radii.sm),
                    ),
                    child: Row(
                      children: [
                        const Icon(PiccoloIcons.error,
                            color: PiccoloTheme.critical, size: 18),
                        const SizedBox(width: 8),
                        Expanded(
                          child: Text(
                            _error!,
                            style:
                                const TextStyle(color: PiccoloTheme.critical),
                          ),
                        ),
                      ],
                    ),
                  ),
                  const SizedBox(height: 16),
                ],
                Text(
                  'The invited user will set up a passkey when they open the invite link.',
                  style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                    color: PiccoloTheme.inkMuted,
                  ),
                ),
                const SizedBox(height: 16),
                TextFormField(
                  controller: _usernameController,
                  decoration: const InputDecoration(
                    labelText: 'Username',
                    prefixIcon: Icon(PiccoloIcons.person),
                  ),
                  validator: (value) {
                    if (value == null || value.isEmpty) {
                      return 'Username is required';
                    }
                    if (value.length < 3) {
                      return 'Username must be at least 3 characters';
                    }
                    if (!RegExp(r'^[a-zA-Z0-9_]+$').hasMatch(value)) {
                      return 'Username can only contain letters, numbers, and underscores';
                    }
                    return null;
                  },
                ),
                const SizedBox(height: 16),
                TextFormField(
                  controller: _emailController,
                  decoration: const InputDecoration(
                    labelText: 'Email',
                    prefixIcon: Icon(PiccoloIcons.email),
                  ),
                  keyboardType: TextInputType.emailAddress,
                  validator: (value) {
                    if (value == null || value.isEmpty) {
                      return 'Email is required';
                    }
                    if (!RegExp(r'^[\w-\.]+@([\w-]+\.)+[\w-]{2,4}$')
                        .hasMatch(value)) {
                      return 'Please enter a valid email';
                    }
                    return null;
                  },
                ),
                const SizedBox(height: 16),
                TextFormField(
                  controller: _allowedAppsController,
                  decoration: const InputDecoration(
                    labelText: 'Allowed Apps (optional)',
                    hintText: 'app1, app2, app3',
                    prefixIcon: Icon(PiccoloIcons.apps),
                  ),
                ),
                const SizedBox(height: 4),
                Text(
                  'Comma-separated list of app IDs. Leave empty for default access.',
                  style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                    color: PiccoloTheme.inkMuted,
                  ),
                ),
              ],
            ),
          ),
        ),
      ),
      actions: [
        TextButton(
          onPressed: _isLoading ? null : () => Navigator.of(context).pop(),
          child: const Text('Cancel'),
        ),
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
              : const Text('Create Invite'),
        ),
      ],
    );
  }

  Future<void> _submit() async {
    if (!(_formKey.currentState?.validate() ?? false)) {
      return;
    }

    setState(() {
      _isLoading = true;
      _error = null;
    });

    try {
      final allowedApps = _allowedAppsController.text
          .split(',')
          .map((e) => e.trim())
          .where((e) => e.isNotEmpty)
          .toList();

      final token = await widget.onInvite(
        _usernameController.text,
        _emailController.text,
        allowedApps,
      );

      if (mounted) {
        Navigator.of(context).pop();
        widget.onShowInviteLink(token);
      }
    } on Object catch (e) {
      if (mounted) {
        setState(() {
          _error = e.toString();
          _isLoading = false;
        });
      }
    }
  }
}
