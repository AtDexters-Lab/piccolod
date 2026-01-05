import 'package:flutter/material.dart';
import '../../../../../theme/piccolo_theme.dart';
import '../../../../../core/models/os_update.dart';
import '../../../../../shared/widgets/log_stream_viewer.dart';
import '../settings_controller.dart';

class SystemTab extends StatelessWidget {
  final SettingsController controller;

  const SystemTab({super.key, required this.controller});

  @override
  Widget build(BuildContext context) {
    // If rebooting, show the modal-like overlay
    if (controller.isRebooting) {
      return const Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            CircularProgressIndicator(strokeWidth: 3),
            SizedBox(height: 32),
            Text(
              "System is restarting...",
              style: TextStyle(fontSize: 20, fontWeight: FontWeight.w600),
            ),
            SizedBox(height: 8),
            Text("You will be redirected to login once the system is back online."),
          ],
        ),
      );
    }

    final update = controller.osUpdate;
    final bool isBusy = controller.isUpdateInProgress || controller.isBackendBusy;

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text("System Update", style: PiccoloTheme.textTheme.displayLarge?.copyWith(fontSize: 28)),
        const SizedBox(height: 32),
        
        if (update != null || isBusy) ...[
          // 1. Hero Status Card
          _UpdateStatusCard(
            update: update,
            isChecking: isBusy,
            onCheck: controller.checkForUpdates,
            onReboot: controller.rebootOS,
          ),
          
          const SizedBox(height: 48),
          
          if (update != null) ...[
            // 2. Advanced / Danger Zone (Only show if we have data)
            Text("Advanced Options", style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
            const SizedBox(height: 16),
            Container(
              padding: const EdgeInsets.all(24),
              decoration: BoxDecoration(
                color: Colors.white,
                borderRadius: BorderRadius.circular(12),
                border: Border.all(color: PiccoloTheme.mist),
              ),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  _InfoRow("Current Version", update.currentVersion),
                  _InfoRow("Last Checked", _formatDate(update.lastChecked)),
                  const Divider(height: 32),
                  Row(
                    children: [
                      Expanded(
                        child: Column(
                          crossAxisAlignment: CrossAxisAlignment.start,
                          children: [
                            Text("Rollback System", style: PiccoloTheme.textTheme.bodyMedium?.copyWith(fontWeight: FontWeight.bold)),
                            const SizedBox(height: 4),
                            Text(
                              "Revert to the previous system snapshot. Useful if an update caused issues.",
                              style: PiccoloTheme.textTheme.labelSmall,
                            ),
                          ],
                        ),
                      ),
                      OutlinedButton(
                        onPressed: (controller.isLoading || isBusy) ? null : () => _showRollbackConfirmation(context),
                        style: OutlinedButton.styleFrom(
                          foregroundColor: PiccoloTheme.critical,
                          side: const BorderSide(color: PiccoloTheme.critical),
                        ),
                        child: const Text("Rollback"),
                      ),
                    ],
                  ),
                ],
              ),
            ),
          ],

          const SizedBox(height: 48),
          Text("Update Logs", style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
          const SizedBox(height: 16),
          LogStreamViewer(
            systemUnit: 'transactional-update',
            tailLines: 200,
            height: 320,
            autoConnect: isBusy,
          ),
        ] else 
           const Text("System information unavailable."),
      ],
    );
  }

  void _showRollbackConfirmation(BuildContext context) {
    showDialog(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text("Confirm Rollback"),
        content: const Text(
          "Are you sure you want to rollback the system to the previous snapshot?\n\n"
          "This will discard any system changes made since the last update. User data (files) will be preserved.",
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.pop(context),
            child: const Text("Cancel"),
          ),
          FilledButton(
            onPressed: () {
              Navigator.pop(context);
              controller.rollbackOS();
            },
            style: FilledButton.styleFrom(backgroundColor: PiccoloTheme.critical),
            child: const Text("Rollback"),
          ),
        ],
      ),
    );
  }

  String _formatDate(DateTime dt) {
    // Simple formatter, can be replaced with intl package later
    return "${dt.year}-${dt.month.toString().padLeft(2,'0')}-${dt.day.toString().padLeft(2,'0')} ${dt.hour.toString().padLeft(2,'0')}:${dt.minute.toString().padLeft(2,'0')}";
  }
}

class _UpdateStatusCard extends StatelessWidget {
  final OSUpdate? update; // Make nullable
  final bool isChecking;
  final VoidCallback onCheck;
  final VoidCallback onReboot;

  const _UpdateStatusCard({
    required this.update,
    required this.isChecking,
    required this.onCheck,
    required this.onReboot,
  });

  @override
  Widget build(BuildContext context) {
    // Determine State
    bool pendingReboot = update?.pending ?? false;
    
    Color accentColor = PiccoloTheme.success;
    IconData icon = Icons.check_circle_outline;
    String title = "System is up to date";
    String subtitle = update != null ? "Version ${update!.currentVersion}" : "Checking version...";
    Widget? action;

    if (isChecking) {
      accentColor = PiccoloTheme.cobalt600;
      icon = Icons.sync;
      title = "Checking for updates...";
      subtitle = "Please wait.";
      action = const SizedBox(
        height: 24, 
        width: 24, 
        child: CircularProgressIndicator(strokeWidth: 2)
      );
    } else if (pendingReboot) {
      accentColor = PiccoloTheme.cobalt600; // or Info color
      icon = Icons.system_update;
      title = "Update Available";
      subtitle = "Version ${update!.availableVersion} is ready to install.";
      action = ElevatedButton.icon(
        onPressed: onReboot,
        icon: const Icon(Icons.restart_alt),
        label: const Text("Restart Now"),
        style: ElevatedButton.styleFrom(
          backgroundColor: PiccoloTheme.cobalt600,
          foregroundColor: Colors.white,
          padding: const EdgeInsets.symmetric(horizontal: 24, vertical: 12),
        ),
      );
    } else if (update != null) {
      // Idle / Up to date
      action = OutlinedButton(
        onPressed: onCheck,
        child: const Text("Check for Updates"),
      );
    } else {
      // Update is null and not checking? Fallback
      title = "Unknown Status";
      subtitle = "";
    }

    return Container(
      padding: const EdgeInsets.all(32),
      decoration: BoxDecoration(
        color: Colors.white,
        borderRadius: BorderRadius.circular(16),
        boxShadow: [
          BoxShadow(
            color: PiccoloTheme.ink.withValues(alpha: 0.05),
            blurRadius: 20,
            offset: const Offset(0, 10),
          ),
        ],
      ),
      child: Row(
        children: [
          Container(
            padding: const EdgeInsets.all(16),
            decoration: BoxDecoration(
              color: accentColor.withValues(alpha: 0.1),
              shape: BoxShape.circle,
            ),
            child: isChecking 
              ? SizedBox(width: 32, height: 32, child: CircularProgressIndicator(color: accentColor))
              : Icon(icon, color: accentColor, size: 32),
          ),
          const SizedBox(width: 24),
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(title, style: PiccoloTheme.textTheme.displayLarge?.copyWith(fontSize: 20)),
                const SizedBox(height: 4),
                Text(subtitle, style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted)),
              ],
            ),
          ),
          if (action != null) ...[
            const SizedBox(width: 24),
            action,
          ]
        ],
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
