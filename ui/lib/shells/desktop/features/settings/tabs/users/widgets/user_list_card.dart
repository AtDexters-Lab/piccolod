import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/user.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Card displaying a user in the users list.
class UserListCard extends StatelessWidget {
  final User user;
  final VoidCallback onEdit;
  final VoidCallback onDelete;
  final VoidCallback onSetPassword;

  const UserListCard({
    super.key,
    required this.user,
    required this.onEdit,
    required this.onDelete,
    required this.onSetPassword,
  });

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.all(16),
      decoration: BoxDecoration(
        color: Colors.white,
        borderRadius: BorderRadius.circular(12),
        border: Border.all(color: PiccoloTheme.porcelain),
      ),
      child: Row(
        children: [
          // Avatar
          CircleAvatar(
            backgroundColor: user.isAdmin
                ? PiccoloTheme.cobalt600
                : PiccoloTheme.inkMuted.withValues(alpha: 0.2),
            foregroundColor: user.isAdmin ? Colors.white : PiccoloTheme.ink,
            child: Text(
              user.username.isNotEmpty ? user.username[0].toUpperCase() : '?',
              style: const TextStyle(fontWeight: FontWeight.bold),
            ),
          ),
          const SizedBox(width: 16),
          // User info
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Row(
                  children: [
                    Text(
                      user.username,
                      style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
                        fontWeight: FontWeight.w600,
                      ),
                    ),
                    const SizedBox(width: 8),
                    _RoleBadge(role: user.role),
                  ],
                ),
                const SizedBox(height: 4),
                Text(
                  user.email,
                  style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                    color: PiccoloTheme.inkMuted,
                  ),
                ),
                if (user.allowedApps.isNotEmpty && !user.isAdmin) ...[
                  const SizedBox(height: 8),
                  Wrap(
                    spacing: 4,
                    runSpacing: 4,
                    children: user.allowedApps.map((app) {
                      return Container(
                        padding: const EdgeInsets.symmetric(
                          horizontal: 8,
                          vertical: 2,
                        ),
                        decoration: BoxDecoration(
                          color: PiccoloTheme.mist,
                          borderRadius: BorderRadius.circular(4),
                        ),
                        child: Text(
                          app,
                          style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                            fontSize: 10,
                          ),
                        ),
                      );
                    }).toList(),
                  ),
                ],
              ],
            ),
          ),
          // Actions
          PopupMenuButton<String>(
            icon: const Icon(Icons.more_vert, color: PiccoloTheme.inkMuted),
            onSelected: (value) {
              switch (value) {
                case 'edit':
                  onEdit();
                  break;
                case 'password':
                  onSetPassword();
                  break;
                case 'delete':
                  onDelete();
                  break;
              }
            },
            itemBuilder: (context) => [
              const PopupMenuItem(
                value: 'edit',
                child: Row(
                  children: [
                    Icon(Icons.edit_outlined, size: 18),
                    SizedBox(width: 8),
                    Text('Edit'),
                  ],
                ),
              ),
              const PopupMenuItem(
                value: 'password',
                child: Row(
                  children: [
                    Icon(Icons.lock_outline, size: 18),
                    SizedBox(width: 8),
                    Text('Set Password'),
                  ],
                ),
              ),
              const PopupMenuDivider(),
              PopupMenuItem(
                value: 'delete',
                child: Row(
                  children: [
                    Icon(Icons.delete_outline,
                        size: 18, color: PiccoloTheme.critical),
                    const SizedBox(width: 8),
                    Text('Delete',
                        style: TextStyle(color: PiccoloTheme.critical)),
                  ],
                ),
              ),
            ],
          ),
        ],
      ),
    );
  }
}

class _RoleBadge extends StatelessWidget {
  final String role;

  const _RoleBadge({required this.role});

  @override
  Widget build(BuildContext context) {
    final isAdmin = role == 'admin';
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 2),
      decoration: BoxDecoration(
        color: isAdmin
            ? PiccoloTheme.cobalt600.withValues(alpha: 0.1)
            : PiccoloTheme.inkMuted.withValues(alpha: 0.1),
        borderRadius: BorderRadius.circular(12),
        border: Border.all(
          color: isAdmin
              ? PiccoloTheme.cobalt600.withValues(alpha: 0.3)
              : PiccoloTheme.inkMuted.withValues(alpha: 0.3),
        ),
      ),
      child: Text(
        isAdmin ? 'Admin' : 'Standard',
        style: TextStyle(
          fontSize: 10,
          fontWeight: FontWeight.w500,
          color: isAdmin ? PiccoloTheme.cobalt600 : PiccoloTheme.inkMuted,
        ),
      ),
    );
  }
}
