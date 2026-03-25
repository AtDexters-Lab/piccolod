import 'package:flutter/material.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/remote_controller.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/widgets/namek_management_section.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/widgets/portal_list_card.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/widgets/remote_aliases_card.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/widgets/remote_certificates_card.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/widgets/remote_events_card.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/widgets/self_hosted_section.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class RemoteDashboard extends StatelessWidget {

  const RemoteDashboard({required this.controller, super.key});
  final RemoteController controller;

  @override
  Widget build(BuildContext context) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        _buildHeader(),
        const SizedBox(height: Spacing.lg),
        ..._buildRelayWarnings(),
        PortalListCard(controller: controller),
        const SizedBox(height: Spacing.lg),
        RemoteCertificatesCard(controller: controller),
        const SizedBox(height: Spacing.lg),
        RemoteAliasesCard(controller: controller),
        const SizedBox(height: Spacing.lg),
        _buildSettingsDivider(),
        const SizedBox(height: Spacing.lg),
        NamekManagementSection(controller: controller),
        const SizedBox(height: Spacing.lg),
        SelfHostedSection(controller: controller),
        const SizedBox(height: Spacing.lg),
        RemoteEventsCard(controller: controller),
      ],
    );
  }

  Widget _buildHeader() {
    return Row(
      mainAxisAlignment: MainAxisAlignment.spaceBetween,
      children: [
        Text('Remote Access', style: PiccoloTheme.textTheme.headlineMedium),
        FilledButton.icon(
          onPressed: controller.refresh,
          icon: const Icon(PiccoloIcons.refresh),
          label: const Text('Refresh'),
        ),
      ],
    );
  }

  /// Shows relay connection warnings only — cert/alias warnings are covered
  /// by per-item health in the certificates and aliases cards.
  List<Widget> _buildRelayWarnings() {
    final warnings = controller.status?.warnings ?? [];
    // Filter to relay-level warnings (not cert-derived).
    // Relay warnings contain "relay" from relayWarning() in the backend.
    final relayWarnings = warnings.where((w) => w.toLowerCase().contains('relay')).toList();
    if (relayWarnings.isEmpty) return [];

    return [
      Container(
        padding: const EdgeInsets.all(Spacing.md),
        decoration: BoxDecoration(
          color: PiccoloTheme.warning.withValues(alpha: 0.1),
          borderRadius: BorderRadius.circular(Radii.sm),
          border: Border.all(color: PiccoloTheme.warning.withValues(alpha: 0.3)),
        ),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: relayWarnings.map((w) => Row(
            children: [
              const Icon(PiccoloIcons.warning, color: PiccoloTheme.warning, size: 16),
              const SizedBox(width: Spacing.sm),
              Expanded(child: Text(w, style: const TextStyle(color: PiccoloTheme.ink))),
            ],
          )).toList(),
        ),
      ),
      const SizedBox(height: Spacing.lg),
    ];
  }

  Widget _buildSettingsDivider() {
    return Row(
      children: [
        const Expanded(child: Divider()),
        Padding(
          padding: const EdgeInsets.symmetric(horizontal: Spacing.base),
          child: Text(
            'Settings',
            style: PiccoloTheme.textTheme.labelSmall?.copyWith(
              color: PiccoloTheme.inkMuted,
              fontWeight: FontWeight.w600,
              letterSpacing: 1.2,
            ),
          ),
        ),
        const Expanded(child: Divider()),
      ],
    );
  }
}
