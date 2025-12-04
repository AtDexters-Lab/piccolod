import 'package:flutter/material.dart';
import '../../../../../theme/piccolo_theme.dart';
import '../settings_controller.dart';

class NetworkTab extends StatelessWidget {
  final SettingsController controller;

  const NetworkTab({super.key, required this.controller});

  @override
  Widget build(BuildContext context) {
    final status = controller.remoteStatus;
    
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        if (status != null) ...[
          Card(
             elevation: 0,
             color: Colors.white,
             shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(12)),
             child: Padding(
               padding: const EdgeInsets.all(24),
               child: Column(
                 crossAxisAlignment: CrossAxisAlignment.start,
                 children: [
                   Text("Remote Access", style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
                   const Divider(),
                   const SizedBox(height: 16),
                   _InfoRow("Enabled", status.enabled.toString()),
                   _InfoRow("State", status.state),
                   if (status.endpoint != null) _InfoRow("Endpoint", status.endpoint!),
                   if (status.tld != null) _InfoRow("TLD", status.tld!),
                 ],
               ),
             ),
          ),
          const SizedBox(height: 24),
          if (status.enabled)
            ElevatedButton.icon(
              onPressed: () => controller.disableRemote(),
              icon: const Icon(Icons.cloud_off),
              label: const Text("Disable Remote Access"),
              style: ElevatedButton.styleFrom(
                backgroundColor: PiccoloTheme.critical,
                foregroundColor: Colors.white,
              ),
            )
          else
             const Text("Remote access is currently disabled. Use the CLI or wait for the setup wizard implementation to enable it."),
        ] else
          const Text("No network details available."),
      ],
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
