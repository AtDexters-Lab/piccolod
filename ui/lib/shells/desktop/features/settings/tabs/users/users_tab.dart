import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/user.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/users/users_controller.dart';
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
        FilledButton.icon(
          onPressed: _showAddUserDialog,
          icon: const Icon(PiccoloIcons.add),
          label: const Text('Add User'),
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
