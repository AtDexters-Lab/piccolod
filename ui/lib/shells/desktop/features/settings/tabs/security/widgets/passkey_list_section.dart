import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/webauthn_service.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/security/passkeys_controller.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Section widget that displays and manages the current user's passkeys.
class PasskeyListSection extends StatefulWidget {
  const PasskeyListSection({super.key});

  @override
  State<PasskeyListSection> createState() => _PasskeyListSectionState();
}

class _PasskeyListSectionState extends State<PasskeyListSection> {
  final PasskeysController _controller = PasskeysController();

  @override
  void initState() {
    super.initState();
    unawaited(_controller.loadPasskeys());
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
            Row(
              mainAxisAlignment: MainAxisAlignment.spaceBetween,
              children: [
                Text('Passkeys', style: PiccoloTheme.textTheme.titleMedium),
                if (WebAuthnService.isAvailable())
                  FilledButton.icon(
                    onPressed:
                        _controller.isLoading ? null : _handleAddPasskey,
                    icon: const Icon(PiccoloIcons.add, size: 18),
                    label: const Text('Add Passkey'),
                  ),
              ],
            ),
            const SizedBox(height: Spacing.base),
            if (_controller.error != null) _buildError(),
            if (_controller.isLoading && _controller.passkeys.isEmpty)
              const Padding(
                padding: EdgeInsets.symmetric(vertical: Spacing.lg),
                child: Center(child: CircularProgressIndicator()),
              )
            else if (_controller.passkeys.isEmpty)
              _buildEmpty()
            else
              _buildPasskeyList(),
          ],
        );
      },
    );
  }

  Widget _buildError() {
    return Padding(
      padding: const EdgeInsets.only(bottom: Spacing.base),
      child: Container(
        padding: const EdgeInsets.all(Spacing.base),
        decoration: BoxDecoration(
          color: PiccoloTheme.critical.withValues(alpha: 0.1),
          borderRadius: BorderRadius.circular(Radii.sm),
          border: Border.all(color: PiccoloTheme.critical),
        ),
        child: Row(
          children: [
            const Icon(PiccoloIcons.error, color: PiccoloTheme.critical),
            const SizedBox(width: Spacing.md),
            Expanded(
              child: Text(
                _controller.error!,
                style: const TextStyle(color: PiccoloTheme.critical),
              ),
            ),
            TextButton(
              onPressed: _controller.loadPasskeys,
              child: const Text('Retry'),
            ),
          ],
        ),
      ),
    );
  }

  Widget _buildEmpty() {
    return Container(
      padding: const EdgeInsets.all(Spacing.lg),
      decoration: BoxDecoration(
        color: PiccoloTheme.porcelain,
        borderRadius: BorderRadius.circular(Radii.md),
        border: Border.all(color: PiccoloTheme.hairline),
      ),
      child: Row(
        children: [
          Icon(
            PiccoloIcons.fingerprint,
            size: 32,
            color: PiccoloTheme.inkMuted.withValues(alpha: 0.5),
          ),
          const SizedBox(width: Spacing.base),
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(
                  'No passkeys registered',
                  style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
                    color: PiccoloTheme.inkMuted,
                  ),
                ),
                const SizedBox(height: Spacing.xs),
                Text(
                  'Add a passkey for passwordless sign-in.',
                  style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                    color: PiccoloTheme.inkMuted,
                  ),
                ),
              ],
            ),
          ),
        ],
      ),
    );
  }

  Widget _buildPasskeyList() {
    return Column(
      children: _controller.passkeys.map((passkey) {
        return Padding(
          padding: const EdgeInsets.only(bottom: Spacing.md),
          child: _PasskeyCard(
            passkey: passkey,
            onRename: () => _showRenameDialog(passkey),
            onDelete: () => _showDeleteConfirmation(passkey),
          ),
        );
      }).toList(),
    );
  }

  Future<void> _handleAddPasskey() async {
    try {
      await _controller.registerPasskey();
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(
          content: Text('Passkey registered successfully'),
          backgroundColor: PiccoloTheme.success,
        ),
      );
    } on Object catch (_) {
      // Error is shown via _controller.error in the UI
    }
  }

  void _showRenameDialog(Passkey passkey) {
    final nameController =
        TextEditingController(text: passkey.friendlyName);
    final formKey = GlobalKey<FormState>();

    unawaited(showDialog<void>(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text('Rename Passkey'),
        content: Form(
          key: formKey,
          child: TextFormField(
            controller: nameController,
            autofocus: true,
            decoration: const InputDecoration(
              labelText: 'Name',
              prefixIcon: Icon(PiccoloIcons.edit),
            ),
            validator: (value) {
              if (value == null || value.trim().isEmpty) {
                return 'Name is required';
              }
              return null;
            },
          ),
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(context).pop(),
            child: const Text('Cancel'),
          ),
          FilledButton(
            onPressed: () async {
              if (!(formKey.currentState?.validate() ?? false)) return;
              Navigator.of(context).pop();
              try {
                await _controller.renamePasskey(
                  passkey.id,
                  nameController.text.trim(),
                );
              } on Object catch (e) {
                if (!context.mounted) return;
                ScaffoldMessenger.of(context).showSnackBar(
                  SnackBar(
                    content: Text('Failed to rename passkey: $e'),
                    backgroundColor: PiccoloTheme.critical,
                  ),
                );
              }
            },
            child: const Text('Rename'),
          ),
        ],
      ),
    ));
  }

  void _showDeleteConfirmation(Passkey passkey) {
    unawaited(showDialog<void>(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text('Remove Passkey'),
        content: Text(
          'Remove "${passkey.friendlyName}"? '
          'You will no longer be able to sign in with this passkey.',
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
                await _controller.deletePasskey(passkey.id);
                if (mounted) {
                  messenger.showSnackBar(
                    SnackBar(
                      content: const Text(
                        'Passkey removed. To stop it appearing in sign-in '
                        "prompts, also remove it from your browser's "
                        'password settings.',
                      ),
                      duration: const Duration(seconds: 15),
                      action: SnackBarAction(
                        label: 'Dismiss',
                        onPressed: () {},
                      ),
                    ),
                  );
                }
              } on Object catch (e) {
                if (mounted) {
                  messenger.showSnackBar(
                    SnackBar(
                      content: Text('Failed to remove passkey: $e'),
                      backgroundColor: PiccoloTheme.critical,
                    ),
                  );
                }
              }
            },
            style: FilledButton.styleFrom(
              backgroundColor: PiccoloTheme.critical,
            ),
            child: const Text('Remove'),
          ),
        ],
      ),
    ));
  }
}

/// Card showing a single passkey entry.
class _PasskeyCard extends StatelessWidget {
  const _PasskeyCard({
    required this.passkey,
    required this.onRename,
    required this.onDelete,
  });

  final Passkey passkey;
  final VoidCallback onRename;
  final VoidCallback onDelete;

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.all(Spacing.base),
      decoration: BoxDecoration(
        color: PiccoloTheme.porcelain,
        borderRadius: BorderRadius.circular(Radii.md),
        border: Border.all(color: PiccoloTheme.hairline),
      ),
      child: Row(
        children: [
          const Icon(PiccoloIcons.fingerprint,
              color: PiccoloTheme.cobalt600, size: 24),
          const SizedBox(width: Spacing.base),
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(
                  passkey.friendlyName,
                  style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
                    fontWeight: FontWeight.w600,
                  ),
                ),
                const SizedBox(height: Spacing.xs),
                Row(
                  children: [
                    _MetadataChip(label: passkey.rpContext),
                    const SizedBox(width: Spacing.sm),
                    Text(
                      'Added ${_formatDate(passkey.createdAt)}',
                      style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                        color: PiccoloTheme.inkMuted,
                      ),
                    ),
                    const SizedBox(width: Spacing.sm),
                    Text(
                      'Last used ${passkey.lastUsedAt != null ? _formatDate(passkey.lastUsedAt!) : 'Never'}',
                      style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                        color: PiccoloTheme.inkMuted,
                      ),
                    ),
                  ],
                ),
              ],
            ),
          ),
          PopupMenuButton<String>(
            icon: const Icon(PiccoloIcons.moreVert,
                color: PiccoloTheme.inkMuted),
            onSelected: (value) {
              switch (value) {
                case 'rename':
                  onRename();
                case 'delete':
                  onDelete();
              }
            },
            itemBuilder: (context) => [
              const PopupMenuItem(
                value: 'rename',
                child: Row(
                  children: [
                    Icon(PiccoloIcons.edit, size: 18),
                    SizedBox(width: Spacing.sm),
                    Text('Rename'),
                  ],
                ),
              ),
              const PopupMenuDivider(),
              const PopupMenuItem(
                value: 'delete',
                child: Row(
                  children: [
                    Icon(PiccoloIcons.delete,
                        size: 18, color: PiccoloTheme.critical),
                    SizedBox(width: Spacing.sm),
                    Text('Remove',
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

  static String _formatDate(DateTime date) {
    final localDate = date.toLocal();
    final now = DateTime.now();
    final today = DateTime(now.year, now.month, now.day);
    final dateDay = DateTime(localDate.year, localDate.month, localDate.day);
    final time = _formatTime(localDate);

    if (dateDay == today) return 'Today, $time';
    if (dateDay == today.subtract(const Duration(days: 1))) {
      return 'Yesterday, $time';
    }

    final months = [
      'Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun',
      'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec',
    ];
    return '${months[localDate.month - 1]} ${localDate.day}, ${localDate.year}';
  }

  static String _formatTime(DateTime date) {
    final hour = date.hour % 12 == 0 ? 12 : date.hour % 12;
    final minute = date.minute.toString().padLeft(2, '0');
    final period = date.hour < 12 ? 'AM' : 'PM';
    return '$hour:$minute $period';
  }
}

class _MetadataChip extends StatelessWidget {
  const _MetadataChip({required this.label});
  final String label;

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: Spacing.sm, vertical: 2),
      decoration: BoxDecoration(
        color: PiccoloTheme.cobalt600.withValues(alpha: 0.1),
        borderRadius: BorderRadius.circular(Radii.xxs),
        border: Border.all(
          color: PiccoloTheme.cobalt600.withValues(alpha: 0.3),
        ),
      ),
      child: Text(
        label,
        style: const TextStyle(
          fontSize: 10,
          fontWeight: FontWeight.w500,
          color: PiccoloTheme.cobalt600,
        ),
      ),
    );
  }
}
