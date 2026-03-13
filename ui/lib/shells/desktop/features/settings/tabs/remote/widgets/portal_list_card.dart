import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/remote_models.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/remote_controller.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/widgets/remote_setup_wizard.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:url_launcher/url_launcher.dart';

class PortalListCard extends StatelessWidget {

  const PortalListCard({required this.controller, super.key});
  final RemoteController controller;

  @override
  Widget build(BuildContext context) {
    final portals = controller.portals;

    if (portals.isEmpty) {
      return _buildZeroState(context);
    }

    return Container(
      padding: const EdgeInsets.all(Spacing.base),
      decoration: BoxDecoration(
        color: PiccoloTheme.porcelain,
        borderRadius: BorderRadius.circular(Radii.sm),
        boxShadow: Elevation.elev1,
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text('Portals', style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
          const SizedBox(height: Spacing.base),
          ...portals.map((portal) => _buildPortalRow(context, portal)),
        ],
      ),
    );
  }

  Widget _buildZeroState(BuildContext context) {
    return Container(
      width: double.infinity,
      padding: const EdgeInsets.all(Spacing.xl),
      decoration: BoxDecoration(
        color: PiccoloTheme.porcelain,
        borderRadius: BorderRadius.circular(Radii.sm),
        boxShadow: Elevation.elev1,
      ),
      child: Column(
        children: [
          const Icon(PiccoloIcons.cloudOff, size: 48, color: PiccoloTheme.inkMuted),
          const SizedBox(height: Spacing.base),
          Text(
            'No Remote Access Configured',
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold),
          ),
          const SizedBox(height: Spacing.sm),
          Text(
            'Set up remote access to reach your device from anywhere.',
            style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: Spacing.lg),
          if (controller.isNamekAvailable && !controller.isNamekEnrolled) ...[
            FilledButton.icon(
              onPressed: controller.enrollNamek,
              icon: const Icon(PiccoloIcons.cloud),
              label: const Text('Set up remote access'),
            ),
            const SizedBox(height: Spacing.base),
          ],
          TextButton.icon(
            onPressed: () => RemoteSetupWizard.show(context, controller),
            icon: const Icon(PiccoloIcons.router, size: 16),
            label: const Text('Advanced: Set up your own relay'),
          ),
        ],
      ),
    );
  }

  Widget _buildPortalRow(BuildContext context, Portal portal) {
    final badgeLabel = portal.source == PortalSource.namek ? 'Managed' : 'Self-hosted';
    final badgeColor = portal.source == PortalSource.namek ? PiccoloTheme.cobalt600 : PiccoloTheme.inkMuted;

    Color stateColor;
    switch (portal.state) {
      case 'active':
        stateColor = PiccoloTheme.success;
      case 'error':
        stateColor = PiccoloTheme.critical;
      default:
        stateColor = PiccoloTheme.warning;
    }

    return Padding(
      padding: const EdgeInsets.only(bottom: Spacing.sm),
      child: Row(
        children: [
          Container(
            width: 10,
            height: 10,
            decoration: BoxDecoration(
              color: stateColor,
              shape: BoxShape.circle,
            ),
          ),
          const SizedBox(width: Spacing.sm),
          Container(
            padding: const EdgeInsets.symmetric(horizontal: Spacing.sm, vertical: 2),
            decoration: BoxDecoration(
              color: badgeColor.withValues(alpha: 0.1),
              borderRadius: BorderRadius.circular(Radii.xs),
              border: Border.all(color: badgeColor.withValues(alpha: 0.3)),
            ),
            child: Text(
              badgeLabel,
              style: TextStyle(fontSize: 11, color: badgeColor, fontWeight: FontWeight.w600),
            ),
          ),
          const SizedBox(width: Spacing.sm),
          Expanded(
            child: MouseRegion(
              cursor: portal.hostname.isNotEmpty ? SystemMouseCursors.click : SystemMouseCursors.basic,
              child: GestureDetector(
                onTap: portal.hostname.isNotEmpty
                    ? () => launchUrl(Uri.parse('https://${portal.hostname}'))
                    : null,
                child: Text(
                  portal.hostname.isNotEmpty ? portal.hostname : 'Configuring...',
                  style: TextStyle(
                    fontWeight: FontWeight.w600,
                    color: portal.hostname.isNotEmpty ? PiccoloTheme.cobalt600 : PiccoloTheme.inkMuted,
                  ),
                  overflow: TextOverflow.ellipsis,
                ),
              ),
            ),
          ),
          if (portal.endpoint != null) ...[
            const SizedBox(width: Spacing.sm),
            Text(
              portal.endpoint!,
              style: PiccoloTheme.textTheme.labelSmall?.copyWith(color: PiccoloTheme.inkMuted),
              overflow: TextOverflow.ellipsis,
            ),
          ],
        ],
      ),
    );
  }
}
