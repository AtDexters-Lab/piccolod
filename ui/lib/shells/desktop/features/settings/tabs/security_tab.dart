import 'package:flutter/material.dart';
import '../../../../../theme/piccolo_icons.dart';
import '../../../../../theme/piccolo_theme.dart';
import '../settings_controller.dart';

class SecurityTab extends StatelessWidget {
  final SettingsController controller;

  const SecurityTab({super.key, required this.controller});

  @override
  Widget build(BuildContext context) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text("Security",
            style: PiccoloTheme.textTheme.headlineLarge),
        const SizedBox(height: 32),
        Text("HTTPS on LAN",
            style: PiccoloTheme.textTheme.titleMedium),
        const SizedBox(height: 16),
        _LANSecurityCard(controller: controller),
      ],
    );
  }
}

class _LANSecurityCard extends StatelessWidget {
  final SettingsController controller;
  const _LANSecurityCard({required this.controller});

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.all(24),
      decoration: BoxDecoration(
        color: PiccoloTheme.porcelain,
        borderRadius: BorderRadius.circular(Radii.md),
        border: Border.all(color: PiccoloTheme.hairline),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Row(
            children: [
              Icon(PiccoloIcons.lock, color: PiccoloTheme.success, size: 20),
              const SizedBox(width: 8),
              Text("HTTPS on LAN",
                style: PiccoloTheme.textTheme.bodyMedium?.copyWith(fontWeight: FontWeight.bold)),
            ],
          ),
          const SizedBox(height: 8),
          Text(
            controller.specificHostname != null
                ? "Access this portal securely via https://${controller.specificHostname}. "
                  "To avoid browser warnings, download and trust the CA certificate."
                : "Access this portal securely via HTTPS using your device-specific hostname. "
                  "To avoid browser warnings, download and trust the CA certificate.",
            style: PiccoloTheme.textTheme.labelSmall,
          ),
          const SizedBox(height: 16),
          OutlinedButton.icon(
            icon: const Icon(PiccoloIcons.download, size: 18),
            label: const Text("Download CA Certificate"),
            onPressed: () => controller.downloadCACertificate(),
          ),
        ],
      ),
    );
  }
}
