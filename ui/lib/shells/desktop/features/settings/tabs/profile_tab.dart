import 'package:flutter/material.dart';
import '../../../../../theme/piccolo_icons.dart';
import '../../../../../theme/piccolo_theme.dart';
import '../../../../../shared/widgets/info_row.dart';
import '../../../../../shared/widgets/password_set_form.dart';
import '../../../../../shared/widgets/piccolo_card.dart';
import '../settings_controller.dart';

class ProfileTab extends StatelessWidget {
  final SettingsController controller;
  final VoidCallback? onLogout;

  const ProfileTab({super.key, required this.controller, this.onLogout});

  @override
  Widget build(BuildContext context) {
    final session = controller.session;
    if (session == null) return const SizedBox.shrink();

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        _InfoCard(
          title: "Current Session",
          children: [
            InfoRow("User", session.user),
            InfoRow("Authenticated", session.authenticated.toString()),
            if (session.expiresAt != null)
              InfoRow("Expires At", session.expiresAt.toString()),
            InfoRow("Volumes Locked", session.volumesLocked.toString()),
          ],
        ),
        const SizedBox(height: 32),
        Text("Actions", style: PiccoloTheme.textTheme.headlineSmall),
        const SizedBox(height: 16),
        Wrap(
          spacing: 16,
          runSpacing: 16,
          children: [
             OutlinedButton.icon(
              onPressed: () => _showChangePasswordDialog(context),
              icon: const Icon(PiccoloIcons.lockKey),
              label: const Text("Change Password"),
            ),
            FilledButton.icon(
              onPressed: () => controller.logout(onLogout ?? () {}),
              icon: const Icon(PiccoloIcons.logout),
              label: const Text("Logout"),
              style: FilledButton.styleFrom(
                 backgroundColor: PiccoloTheme.critical.withValues(alpha: 0.1),
                 foregroundColor: PiccoloTheme.critical,
              ),
            ),
          ],
        ),
      ],
    );
  }

  void _showChangePasswordDialog(BuildContext context) {
    final oldController = TextEditingController();
    final newController = TextEditingController();
    final confirmController = TextEditingController();

    showDialog(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text("Change Password"),
        // Make it scrollable in case keyboard covers it
        content: SingleChildScrollView(
          child: SizedBox(
            width: 400,
            child: AutofillGroup(
              child: Column(
                mainAxisSize: MainAxisSize.min,
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  const Text("Enter your current password and a new secure password."),
                  const SizedBox(height: 24),
                  TextField(
                    controller: oldController,
                    decoration: const InputDecoration(
                      labelText: "Current Password",
                    ),
                    obscureText: true,
                    autofillHints: const [AutofillHints.password],
                  ),
                  const SizedBox(height: 24),
                  PasswordSetForm(
                    passwordController: newController,
                    confirmController: confirmController,
                    passwordLabel: "New Password",
                    confirmLabel: "Confirm New Password",
                  ),
                ],
              ),
            ),
          ),
        ),
        actions: [
          TextButton(onPressed: () => Navigator.pop(context), child: const Text("Cancel")),
          FilledButton(
            onPressed: () async {
              // Basic validation guard
              if (oldController.text.isEmpty || newController.text.isEmpty) {
                 return;
              }
              if (newController.text != confirmController.text) {
                // Form shows error, just don't submit
                return;
              }

              try {
                await controller.changePassword(oldController.text, newController.text);
                if (context.mounted) {
                  Navigator.pop(context);
                  ScaffoldMessenger.of(context).showSnackBar(
                    const SnackBar(content: Text("Password changed successfully")),
                  );
                }
              } catch (e) {
                 if (context.mounted) {
                    ScaffoldMessenger.of(context).showSnackBar(
                      SnackBar(content: Text("Error: $e"), backgroundColor: PiccoloTheme.critical),
                    );
                 }
              }
            },
            child: const Text("Change"),
          ),
        ],
      ),
    );
  }
}

class _InfoCard extends StatelessWidget {
  final String title;
  final List<Widget> children;

  const _InfoCard({required this.title, required this.children});

  @override
  Widget build(BuildContext context) {
    return PiccoloCard(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(title, style: PiccoloTheme.textTheme.titleMedium),
          const Divider(),
          const SizedBox(height: Spacing.base),
          ...children,
        ],
      ),
    );
  }
}
