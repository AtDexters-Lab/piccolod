import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/remote_controller.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/widgets/self_hosted_setup_dialog.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class SelfHostedSection extends StatelessWidget {

  const SelfHostedSection({required this.controller, super.key});
  final RemoteController controller;

  @override
  Widget build(BuildContext context) {
    if (!controller.isSelfHostedActive) {
      return Align(
        alignment: Alignment.centerLeft,
        child: TextButton.icon(
          onPressed: () => SelfHostedSetupDialog.show(context, controller),
          icon: const Icon(PiccoloIcons.router, size: 16),
          label: const Text('Set up your own relay'),
        ),
      );
    }

    final status = controller.status!;
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
          Text('Self-hosted Relay', style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
          const SizedBox(height: Spacing.base),
          _infoLine('Endpoint', status.endpoint ?? 'Unknown'),
          const SizedBox(height: Spacing.xs),
          _infoLine('Hostname', status.portalHostname ?? 'Unknown'),
          const SizedBox(height: Spacing.sm),
          Wrap(
            spacing: Spacing.base,
            runSpacing: Spacing.xs,
            children: [
              _actionLink(
                context,
                label: 'Rotate credentials',
                icon: PiccoloIcons.refresh,
                onPressed: () => _confirmRotate(context),
              ),
              _actionLink(
                context,
                label: 'Disable',
                icon: PiccoloIcons.close,
                onPressed: () => _confirmDisable(context),
                destructive: true,
              ),
            ],
          ),
        ],
      ),
    );
  }

  Widget _infoLine(String label, String value) {
    return Row(
      children: [
        Text('$label: ', style: PiccoloTheme.textTheme.bodySmall?.copyWith(color: PiccoloTheme.inkMuted)),
        Flexible(
          child: Text(
            value,
            style: PiccoloTheme.textTheme.bodySmall,
            overflow: TextOverflow.ellipsis,
          ),
        ),
      ],
    );
  }

  Widget _actionLink(BuildContext context, {required String label, required IconData icon, required VoidCallback onPressed, bool destructive = false}) {
    return TextButton.icon(
      onPressed: onPressed,
      icon: Icon(icon, size: 14),
      label: Text(label),
      style: TextButton.styleFrom(
        foregroundColor: destructive ? PiccoloTheme.critical : null,
        padding: const EdgeInsets.symmetric(horizontal: Spacing.sm, vertical: Spacing.xs),
        minimumSize: Size.zero,
        tapTargetSize: MaterialTapTargetSize.shrinkWrap,
      ),
    );
  }

  void _confirmDisable(BuildContext context) {
    unawaited(showDialog<void>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: const Text('Disable Self-hosted Remote Access?'),
        content: const Text('This will stop the self-hosted relay connection. Managed (piccolospace) access is unaffected.'),
        actions: [
          TextButton(onPressed: () => Navigator.pop(ctx), child: const Text('Cancel')),
          FilledButton(
            style: FilledButton.styleFrom(backgroundColor: PiccoloTheme.critical),
            onPressed: () async {
              Navigator.pop(ctx);
              await controller.disableRemote();
            },
            child: const Text('Disable'),
          ),
        ],
      ),
    ));
  }

  void _confirmRotate(BuildContext context) {
    unawaited(showDialog<void>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: const Text('Rotate Credentials?'),
        content: const Text('This will briefly disconnect the tunnel. Ensure your configuration is backed up.'),
        actions: [
          TextButton(onPressed: () => Navigator.pop(ctx), child: const Text('Cancel')),
          FilledButton(
            onPressed: () async {
              Navigator.pop(ctx);
              final secret = await controller.rotateCredentials();
              if (secret != null && context.mounted) {
                _showSecretDialog(context, secret);
              }
            },
            child: const Text('Rotate'),
          ),
        ],
      ),
    ));
  }

  void _showSecretDialog(BuildContext context, String secret) {
    unawaited(showDialog<void>(
      context: context,
      barrierDismissible: false,
      builder: (ctx) => AlertDialog(
        title: const Text('New Device Secret'),
        content: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            const Text(
              'Credentials rotated successfully. Update your Nexus Relay with this new secret to reconnect.',
              style: TextStyle(color: PiccoloTheme.ink),
            ),
            const SizedBox(height: Spacing.base),
            Container(
              padding: const EdgeInsets.all(Spacing.base),
              width: double.infinity,
              decoration: BoxDecoration(
                color: PiccoloTheme.mist,
                borderRadius: BorderRadius.circular(Radii.sm),
                border: Border.all(color: PiccoloTheme.cobalt600),
              ),
              child: SelectableText(
                secret,
                style: PiccoloTheme.mono.copyWith(fontWeight: FontWeight.bold, fontSize: 16),
              ),
            ),
          ],
        ),
        actions: [
          FilledButton(
            onPressed: () {
              Navigator.pop(ctx);
              unawaited(controller.refresh());
            },
            child: const Text('Done'),
          ),
        ],
      ),
    ));
  }
}
