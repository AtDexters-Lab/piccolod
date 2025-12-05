import 'package:flutter/material.dart';
import '../../../../../theme/piccolo_theme.dart';
import '../../../../../shared/widgets/password_set_form.dart';
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
            _InfoRow("User", session.user),
            _InfoRow("Authenticated", session.authenticated.toString()),
            if (session.expiresAt != null)
              _InfoRow("Expires At", session.expiresAt.toString()),
            _InfoRow("Volumes Locked", session.volumesLocked.toString()),
          ],
        ),
        const SizedBox(height: 32),
        Text("Actions", style: PiccoloTheme.textTheme.displayLarge?.copyWith(fontSize: 20)),
        const SizedBox(height: 16),
        Wrap(
          spacing: 16,
          runSpacing: 16,
          children: [
             ElevatedButton.icon(
              onPressed: () => _showChangePasswordDialog(context),
              icon: const Icon(Icons.lock_reset),
              label: const Text("Change Password"),
              style: ButtonStyle(
                 backgroundColor: WidgetStateProperty.all(Colors.white),
                 foregroundColor: WidgetStateProperty.all(PiccoloTheme.ink),
                 elevation: WidgetStateProperty.all(0),
                 side: WidgetStateProperty.all(const BorderSide(color: PiccoloTheme.mist)),
                 padding: WidgetStateProperty.all(const EdgeInsets.symmetric(horizontal: 24, vertical: 16)),
                 overlayColor: WidgetStateProperty.resolveWith((states) {
                   if (states.contains(WidgetState.hovered)) {
                     return PiccoloTheme.ink.withValues(alpha: 0.05);
                   }
                   return null;
                 }),
              ),
            ),
            ElevatedButton.icon(
              onPressed: () => controller.logout(onLogout ?? () {}), // This will invalidate session
              icon: const Icon(Icons.logout),
              label: const Text("Logout"),
              style: ButtonStyle(
                 backgroundColor: WidgetStateProperty.all(PiccoloTheme.critical.withValues(alpha: 0.1)),
                 foregroundColor: WidgetStateProperty.all(PiccoloTheme.critical),
                 elevation: WidgetStateProperty.all(0),
                 padding: WidgetStateProperty.all(const EdgeInsets.symmetric(horizontal: 24, vertical: 16)),
                 overlayColor: WidgetStateProperty.resolveWith((states) {
                   if (states.contains(WidgetState.hovered)) {
                     return PiccoloTheme.critical.withValues(alpha: 0.2);
                   }
                   return null;
                 }),
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
                    decoration: InputDecoration(
                      labelText: "Current Password",
                       border: OutlineInputBorder(borderRadius: BorderRadius.circular(12)),
                       filled: true,
                       fillColor: Colors.white,
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
    return Card(
      elevation: 0,
      color: Colors.white,
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(12)),
      child: Padding(
        padding: const EdgeInsets.all(24),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(title, style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
            const Divider(),
            const SizedBox(height: 16),
            ...children,
          ],
        ),
      ),
    );
  }
}

class _InfoRow extends StatelessWidget {
  final String label;
  final String value;

  const _InfoRow(this.label, this.value);

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: 8.0),
      child: Row(
        mainAxisAlignment: MainAxisAlignment.spaceBetween,
        children: [
          Text(label, style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted)),
          const SizedBox(width: 16),
          Expanded(
            child: Text(
              value,
              style: PiccoloTheme.textTheme.bodyMedium,
              textAlign: TextAlign.right,
              overflow: TextOverflow.ellipsis,
            ),
          ),
        ],
      ),
    );
  }
}
