import 'package:flutter/material.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/remote_controller.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/widgets/namek_management_section.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/widgets/portal_list_card.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/widgets/remote_aliases_card.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/widgets/remote_certificates_card.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/widgets/remote_events_card.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/widgets/remote_setup_wizard.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class RemoteDashboard extends StatelessWidget {

  const RemoteDashboard({required this.controller, super.key});
  final RemoteController controller;

  @override
  Widget build(BuildContext context) {
    final status = controller.status;

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        _buildHeader(context),
        const SizedBox(height: Spacing.lg),
        if (status != null && status.warnings.isNotEmpty) ...[
          _buildWarnings(context, status.warnings),
          const SizedBox(height: Spacing.lg),
        ],
        PortalListCard(controller: controller),
        const SizedBox(height: Spacing.lg),
        NamekManagementSection(controller: controller),
        const SizedBox(height: Spacing.lg),
        _buildSelfHostedSection(context),
        const SizedBox(height: Spacing.lg),
        RemoteCertificatesCard(controller: controller),
        const SizedBox(height: Spacing.lg),
        RemoteAliasesCard(controller: controller),
        const SizedBox(height: Spacing.lg),
        RemoteEventsCard(controller: controller),
      ],
    );
  }

  Widget _buildHeader(BuildContext context) {
    var statusColor = PiccoloTheme.inkMuted;
    var statusText = 'Inactive';
    var statusIcon = PiccoloIcons.cloudOff;

    if (controller.hasAnyRemoteActive) {
      statusColor = PiccoloTheme.success;
      statusText = 'Active';
      statusIcon = PiccoloIcons.success;
    }

    final status = controller.status;
    if (status != null && status.state == 'error') {
      statusColor = PiccoloTheme.critical;
      statusText = 'Error';
      statusIcon = PiccoloIcons.error;
    } else if (status != null && status.warnings.isNotEmpty) {
      statusColor = PiccoloTheme.warning;
      statusText = 'Degraded';
      statusIcon = PiccoloIcons.warning;
    }

    return Row(
      mainAxisAlignment: MainAxisAlignment.spaceBetween,
      children: [
        Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text('Remote Access', style: PiccoloTheme.textTheme.headlineMedium),
            const SizedBox(height: Spacing.sm),
            Row(
              children: [
                Icon(statusIcon, color: statusColor, size: 20),
                const SizedBox(width: Spacing.sm),
                Text(statusText, style: TextStyle(color: statusColor, fontWeight: FontWeight.bold)),
              ],
            ),
          ],
        ),
        FilledButton.icon(
          onPressed: controller.refresh,
          icon: const Icon(PiccoloIcons.refresh),
          label: const Text('Refresh'),
        ),
      ],
    );
  }

  Widget _buildWarnings(BuildContext context, List<String> warnings) {
    return Container(
      padding: const EdgeInsets.all(Spacing.base),
      decoration: BoxDecoration(
        color: PiccoloTheme.warning.withValues(alpha: 0.1),
        borderRadius: BorderRadius.circular(Radii.sm),
        border: Border.all(color: PiccoloTheme.warning.withValues(alpha: 0.3)),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: warnings.map((w) => Padding(
          padding: const EdgeInsets.only(bottom: Spacing.xs),
          child: Row(
            children: [
              const Icon(PiccoloIcons.warning, color: PiccoloTheme.warning, size: 16),
              const SizedBox(width: Spacing.sm),
              Expanded(child: Text(w, style: const TextStyle(color: PiccoloTheme.ink))),
            ],
          ),
        )).toList(),
      ),
    );
  }

  Widget _buildSelfHostedSection(BuildContext context) {
    final status = controller.status;
    final selfHostedActive = status != null &&
        status.enabled &&
        status.portalHostname != null &&
        status.portalHostname!.isNotEmpty;

    if (selfHostedActive) {
      return Container(
        padding: const EdgeInsets.all(Spacing.base),
        decoration: BoxDecoration(
          color: PiccoloTheme.porcelain,
          borderRadius: BorderRadius.circular(Radii.sm),
          boxShadow: Elevation.elev1,
        ),
        child: Row(
          children: [
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text('Self-hosted Relay', style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
                  const SizedBox(height: Spacing.xs),
                  Text('Endpoint: ${status.endpoint ?? "Unknown"}', style: PiccoloTheme.textTheme.labelSmall),
                  Text('Hostname: ${status.portalHostname}', style: PiccoloTheme.textTheme.labelSmall),
                ],
              ),
            ),
            OutlinedButton(
              onPressed: () => RemoteSetupWizard.show(context, controller),
              child: const Text('Manage'),
            ),
          ],
        ),
      );
    }

    return Align(
      alignment: Alignment.centerLeft,
      child: TextButton.icon(
        onPressed: () => RemoteSetupWizard.show(context, controller),
        icon: const Icon(PiccoloIcons.router, size: 16),
        label: const Text('Advanced: Set up your own relay'),
      ),
    );
  }
}
