import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:piccolo_os/core/models/listener_health.dart';
import 'package:piccolo_os/core/services/app_service.dart';
import 'package:piccolo_os/features/apps/app_launcher.dart';
import 'package:piccolo_os/shells/desktop/desktop_controller.dart';
import 'package:piccolo_os/shells/desktop/features/settings/settings_app.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:url_launcher/url_launcher.dart';

class LocalFallbackOverlay extends StatelessWidget {

  const LocalFallbackOverlay({
    required this.health, required this.appName, required this.lanFallbackUrl, super.key,
    this.appService,
    this.desktopController,
    this.actionableCertId,
  });
  final ListenerHealth health;
  final String appName;
  final String lanFallbackUrl;
  final AppService? appService;
  final DesktopController? desktopController;
  final String? actionableCertId;

  bool get isLocalAccess =>
      AppLauncher.isLocalAccess(Uri.base.host.toLowerCase());

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: Row(
        children: [
          Icon(
            ListenerHealthVisuals.iconForStatus(health.status),
            color: ListenerHealthVisuals.colorForStatus(health.status),
          ),
          const SizedBox(width: Spacing.md),
          Expanded(
            child: Text(
              'Remote Access Unavailable',
              style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
                fontWeight: FontWeight.bold,
              ),
            ),
          ),
        ],
      ),
      content: SizedBox(
        width: 420,
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(health.reason),
            if (health.recoveryEta != null) ...[
              const SizedBox(height: Spacing.sm),
              Text(
                '${health.actionRequired ? 'Next check' : 'Next retry'}: ${health.recoveryEta}',
                style: PiccoloTheme.textTheme.labelSmall,
              ),
            ],
            const SizedBox(height: Spacing.base),
            if (isLocalAccess) ...[
              FilledButton.icon(
                onPressed: () {
                  Navigator.of(context).pop();
                  unawaited(launchUrl(Uri.parse(lanFallbackUrl)));
                },
                icon: const Icon(PiccoloIcons.openExternal, size: 16),
                label: const Text('Access Locally'),
              ),
            ] else ...[
              Text(
                'Local access available on your home network:',
                style: PiccoloTheme.textTheme.labelSmall,
              ),
              const SizedBox(height: Spacing.xs),
              CopyableUrl(url: lanFallbackUrl),
            ],
            const SizedBox(height: Spacing.md),
            Text(
              'Local access requires being on the same network as your Piccolo device',
              style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                color: PiccoloTheme.inkMuted,
              ),
            ),
          ],
        ),
      ),
      actions: [
        if (health.actionRequired) ...[
          TextButton(
            onPressed: () => _openSettings(context),
            child: const Text('Settings'),
          ),
          TextButton(
            onPressed: () => _retryNow(context),
            child: const Text('Retry Now'),
          ),
        ],
        TextButton(
          onPressed: () => Navigator.of(context).pop(),
          child: const Text('Close'),
        ),
      ],
    );
  }

  void _openSettings(BuildContext context) {
    final controller = desktopController;
    if (controller == null) return;
    Navigator.of(context).pop();
    controller.openSettings(initialTab: SettingsTab.remoteAccess);
  }

  Future<void> _retryNow(BuildContext context) async {
    if (appService == null || actionableCertId == null) return;
    try {
      await appService!.renewCertificate(actionableCertId!);
      if (!context.mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(content: Text('Retry queued')),
      );
    } on Object catch (e) {
      if (!context.mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(content: Text('Retry failed: $e')),
      );
    }
  }
}

class CopyableUrl extends StatelessWidget {

  const CopyableUrl({required this.url, super.key, this.compact = false});
  final String url;
  final bool compact;

  @override
  Widget build(BuildContext context) {
    return InkWell(
      onTap: () {
        unawaited(Clipboard.setData(ClipboardData(text: url)));
        ScaffoldMessenger.of(context).showSnackBar(
          const SnackBar(content: Text('URL copied to clipboard')),
        );
      },
      borderRadius: BorderRadius.circular(Radii.xxs),
      child: Container(
        padding: EdgeInsets.symmetric(
          horizontal: compact ? Spacing.sm : Spacing.md,
          vertical: compact ? Spacing.xs : Spacing.sm,
        ),
        decoration: BoxDecoration(
          color: PiccoloTheme.mist,
          borderRadius: BorderRadius.circular(Radii.xxs),
          border: Border.all(color: PiccoloTheme.hairline),
        ),
        child: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            Flexible(
              child: Text(
                url,
                style: TextStyle(
                  fontFamily: 'JetBrainsMono',
                  fontSize: compact ? 11 : 12,
                  color: PiccoloTheme.cobalt600,
                ),
                overflow: TextOverflow.ellipsis,
              ),
            ),
            const SizedBox(width: Spacing.sm),
            Icon(
              PiccoloIcons.copy,
              size: compact ? 14 : 16,
              color: PiccoloTheme.inkMuted,
            ),
          ],
        ),
      ),
    );
  }
}
