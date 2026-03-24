import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:piccolo_os/core/models/user.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/users/users_controller.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/users/widgets/invite_form_dialog.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/users/widgets/user_form_dialog.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/users/widgets/user_list_card.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Tab for managing users in the Settings app.
class UsersTab extends StatefulWidget {
  const UsersTab({super.key});

  @override
  State<UsersTab> createState() => _UsersTabState();
}

class _UsersTabState extends State<UsersTab> {
  final UsersController _controller = UsersController();

  @override
  void initState() {
    super.initState();
    unawaited(_controller.loadUsers());
  }

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return ListenableBuilder(
      listenable: _controller,
      builder: (context, child) {
        return Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            _buildHeader(),
            const SizedBox(height: 24),
            if (_controller.error != null)
              _buildError()
            else if (_controller.isLoading && _controller.users.isEmpty)
              const Center(child: CircularProgressIndicator())
            else
              _buildUsersList(),
          ],
        );
      },
    );
  }

  Widget _buildHeader() {
    return Row(
      mainAxisAlignment: MainAxisAlignment.spaceBetween,
      children: [
        Text(
          'Users',
          style: PiccoloTheme.textTheme.headlineLarge,
        ),
        Row(
          children: [
            OutlinedButton.icon(
              onPressed: _showInviteUserDialog,
              icon: const Icon(PiccoloIcons.link, size: 18),
              label: const Text('Invite User'),
            ),
            const SizedBox(width: Spacing.sm),
            FilledButton.icon(
              onPressed: _showAddUserDialog,
              icon: const Icon(PiccoloIcons.add),
              label: const Text('Add User'),
            ),
          ],
        ),
      ],
    );
  }

  Widget _buildError() {
    return Container(
      padding: const EdgeInsets.all(16),
      decoration: BoxDecoration(
        color: PiccoloTheme.critical.withValues(alpha: 0.1),
        borderRadius: BorderRadius.circular(Radii.sm),
        border: Border.all(color: PiccoloTheme.critical),
      ),
      child: Row(
        children: [
          const Icon(PiccoloIcons.error, color: PiccoloTheme.critical),
          const SizedBox(width: 12),
          Expanded(
            child: Text(
              _controller.error!,
              style: const TextStyle(color: PiccoloTheme.critical),
            ),
          ),
          TextButton(
            onPressed: _controller.loadUsers,
            child: const Text('Retry'),
          ),
        ],
      ),
    );
  }

  Widget _buildUsersList() {
    if (_controller.users.isEmpty) {
      return Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            Icon(
              PiccoloIcons.people,
              size: 64,
              color: PiccoloTheme.inkMuted.withValues(alpha: 0.5),
            ),
            const SizedBox(height: 16),
            Text(
              'No users yet',
              style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
                color: PiccoloTheme.inkMuted,
              ),
            ),
            const SizedBox(height: 8),
            Text(
              'Add your first user to get started',
              style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                color: PiccoloTheme.inkMuted,
              ),
            ),
          ],
        ),
      );
    }

    return Column(
      children: _controller.users.map((user) {
        return Padding(
          padding: const EdgeInsets.only(bottom: 12),
          child: UserListCard(
            user: user,
            onEdit: () => _showEditUserDialog(user),
            onDelete: () => _showDeleteConfirmation(user),
            onSetPassword: () => _showSetPasswordDialog(user),
            onReinvite: !user.isAdmin
                ? () => _handleReinvite(user)
                : null,
          ),
        );
      }).toList(),
    );
  }

  void _showAddUserDialog() {
    unawaited(showDialog<void>(
      context: context,
      builder: (context) => UserFormDialog(
        onSave: (username, email, password, role, allowedApps) async {
          await _controller.createUser(
            username: username,
            email: email,
            password: password!,
            role: role,
            allowedApps: allowedApps,
          );
        },
      ),
    ));
  }

  void _showInviteUserDialog() {
    unawaited(showDialog<void>(
      context: context,
      builder: (context) => InviteFormDialog(
        onInvite: (username, email, allowedApps) async {
          final token = await _controller.createInvite(
            username: username,
            email: email,
            allowedApps: allowedApps,
          );
          return token;
        },
        onShowInviteLink: _showInviteLinkDialog,
      ),
    ));
  }

  void _showInviteLinkDialog(String token) {
    final origin = Uri.base.replace(scheme: 'https').origin;
    final inviteUrl = '$origin/?invite=$token';

    unawaited(showDialog<void>(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text('Invite Created'),
        content: SizedBox(
          width: 450,
          child: Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(
                'Share this link with the user to complete their registration:',
                style: PiccoloTheme.textTheme.bodyMedium,
              ),
              const SizedBox(height: Spacing.base),
              Container(
                padding: const EdgeInsets.all(Spacing.md),
                decoration: BoxDecoration(
                  color: PiccoloTheme.mist,
                  borderRadius: BorderRadius.circular(Radii.sm),
                  border: Border.all(color: PiccoloTheme.hairline),
                ),
                child: Row(
                  children: [
                    Expanded(
                      child: SelectableText(
                        inviteUrl,
                        style: PiccoloTheme.textTheme.bodySmall?.copyWith(
                          fontFamily: 'monospace',
                        ),
                      ),
                    ),
                    const SizedBox(width: Spacing.sm),
                    IconButton(
                      icon: const Icon(PiccoloIcons.copy, size: 18),
                      tooltip: 'Copy to clipboard',
                      onPressed: () async {
                        await Clipboard.setData(ClipboardData(text: inviteUrl));
                        if (!context.mounted) return;
                        ScaffoldMessenger.of(context).showSnackBar(
                          const SnackBar(
                            content: Text('Invite link copied to clipboard'),
                            backgroundColor: PiccoloTheme.success,
                          ),
                        );
                      },
                    ),
                  ],
                ),
              ),
              const SizedBox(height: Spacing.base),
              Text(
                'The user will register a passkey when they open this link.',
                style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                  color: PiccoloTheme.inkMuted,
                ),
              ),
            ],
          ),
        ),
        actions: [
          FilledButton(
            onPressed: () => Navigator.of(context).pop(),
            child: const Text('Done'),
          ),
        ],
      ),
    ));
  }

  Future<void> _handleReinvite(User user) async {
    try {
      final token = await _controller.reinviteUser(user.id);
      if (!mounted || token.isEmpty) return;
      _showInviteLinkDialog(token);
    } on Object catch (e) {
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(
          content: Text('Failed to create invite: $e'),
          backgroundColor: PiccoloTheme.critical,
        ),
      );
    }
  }

  void _showEditUserDialog(User user) {
    unawaited(showDialog<void>(
      context: context,
      builder: (context) => UserFormDialog(
        user: user,
        onSave: (username, email, password, role, allowedApps) async {
          await _controller.updateUser(
            id: user.id,
            username: username,
            email: email,
            role: role,
            allowedApps: allowedApps,
          );
        },
      ),
    ));
  }

  void _showDeleteConfirmation(User user) {
    unawaited(showDialog<void>(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text('Delete User'),
        content: Text(
          'Are you sure you want to delete "${user.username}"? This action cannot be undone.',
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(context).pop(),
            child: const Text('Cancel'),
          ),
          FilledButton(
            onPressed: () async {
              final messenger = ScaffoldMessenger.of(context);
              Navigator.of(context).pop();
              try {
                await _controller.deleteUser(user.id);
              } on Object catch (e) {
                if (mounted) {
                  messenger.showSnackBar(
                    SnackBar(
                      content: Text('Failed to delete user: $e'),
                      backgroundColor: PiccoloTheme.critical,
                    ),
                  );
                }
              }
            },
            style: FilledButton.styleFrom(
              backgroundColor: PiccoloTheme.critical,
            ),
            child: const Text('Delete'),
          ),
        ],
      ),
    ));
  }

  void _showSetPasswordDialog(User user) {
    final passwordController = TextEditingController();
    final confirmController = TextEditingController();
    final formKey = GlobalKey<FormState>();

    unawaited(showDialog<void>(
      context: context,
      builder: (context) => AlertDialog(
        title: Text('Set Password for ${user.username}'),
        content: Form(
          key: formKey,
          child: Column(
            mainAxisSize: MainAxisSize.min,
            children: [
              TextFormField(
                controller: passwordController,
                obscureText: true,
                decoration: const InputDecoration(
                  labelText: 'New Password',
                ),
                validator: (value) {
                  if (value == null || value.length < 8) {
                    return 'Password must be at least 8 characters';
                  }
                  return null;
                },
              ),
              const SizedBox(height: 16),
              TextFormField(
                controller: confirmController,
                obscureText: true,
                decoration: const InputDecoration(
                  labelText: 'Confirm Password',
                ),
                validator: (value) {
                  if (value != passwordController.text) {
                    return 'Passwords do not match';
                  }
                  return null;
                },
              ),
            ],
          ),
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(context).pop(),
            child: const Text('Cancel'),
          ),
          FilledButton(
            onPressed: () async {
              if (formKey.currentState?.validate() ?? false) {
                Navigator.of(context).pop();
                try {
                  await _controller.setUserPassword(
                    user.id,
                    passwordController.text,
                  );
                  if (!context.mounted) return;
                  ScaffoldMessenger.of(context).showSnackBar(
                    const SnackBar(
                      content: Text('Password updated successfully'),
                      backgroundColor: PiccoloTheme.success,
                    ),
                  );
                } on Object catch (e) {
                  if (!context.mounted) return;
                  ScaffoldMessenger.of(context).showSnackBar(
                    SnackBar(
                      content: Text('Failed to set password: $e'),
                      backgroundColor: PiccoloTheme.critical,
                    ),
                  );
                }
              }
            },
            child: const Text('Set Password'),
          ),
        ],
      ),
    ));
  }
}
