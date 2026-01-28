import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:url_launcher/url_launcher.dart';
import '../../../core/models/listener_health.dart';
import '../../../core/services/app_service.dart';
import '../../../shells/desktop/desktop_controller.dart';
import '../../../shells/desktop/features/settings/settings_app.dart';
import '../../../theme/piccolo_theme.dart';
import '../app_launcher.dart';

class LocalFallbackOverlay extends StatelessWidget {
  final ListenerHealth health;
  final String appName;
  final String lanFallbackUrl;
  final AppService? appService;
  final DesktopController? desktopController;
  final String? actionableCertId;

  const LocalFallbackOverlay({
    super.key,
    required this.health,
    required this.appName,
    required this.lanFallbackUrl,
    this.appService,
    this.desktopController,
    this.actionableCertId,
  });

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
          const SizedBox(width: 12),
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
              const SizedBox(height: 8),
              Text(
                '${health.actionRequired ? 'Next check' : 'Next retry'}: ${health.recoveryEta}',
                style: PiccoloTheme.textTheme.labelSmall,
              ),
            ],
            const SizedBox(height: 16),
            if (isLocalAccess) ...[
              ElevatedButton.icon(
                onPressed: () {
                  Navigator.of(context).pop();
                  launchUrl(Uri.parse(lanFallbackUrl));
                },
                icon: const Icon(Icons.open_in_new, size: 16),
                label: const Text('Access Locally'),
              ),
            ] else ...[
              Text(
                'Local access available on your home network:',
                style: PiccoloTheme.textTheme.labelSmall,
              ),
              const SizedBox(height: 4),
              CopyableUrl(url: lanFallbackUrl),
            ],
            const SizedBox(height: 12),
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
    openOrFocusSettings(controller);
  }

  Future<void> _retryNow(BuildContext context) async {
    if (appService == null || actionableCertId == null) return;
    try {
      await appService!.renewCertificate(actionableCertId!);
      if (!context.mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(content: Text('Retry queued')),
      );
    } catch (e) {
      if (!context.mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(content: Text('Retry failed: $e')),
      );
    }
  }
}

void openOrFocusSettings(DesktopController controller) {
  const settingsId = 'settings';
  if (controller.isAppOpen(settingsId)) {
    controller.focusWindow(settingsId);
  } else {
    controller.openApp(
      settingsId,
      'Settings',
      Icons.settings_rounded,
      SettingsApp(onLogout: controller.logout),
      initialSize: const Size(1100, 750),
    );
  }
}

class CopyableUrl extends StatelessWidget {
  final String url;
  final bool compact;

  const CopyableUrl({super.key, required this.url, this.compact = false});

  @override
  Widget build(BuildContext context) {
    return InkWell(
      onTap: () {
        Clipboard.setData(ClipboardData(text: url));
        ScaffoldMessenger.of(context).showSnackBar(
          const SnackBar(content: Text('URL copied to clipboard')),
        );
      },
      borderRadius: BorderRadius.circular(4),
      child: Container(
        padding: EdgeInsets.symmetric(
          horizontal: compact ? 8 : 12,
          vertical: compact ? 4 : 8,
        ),
        decoration: BoxDecoration(
          color: PiccoloTheme.mist,
          borderRadius: BorderRadius.circular(4),
          border: Border.all(color: Colors.black12),
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
            const SizedBox(width: 8),
            Icon(
              Icons.copy,
              size: compact ? 14 : 16,
              color: PiccoloTheme.inkMuted,
            ),
          ],
        ),
      ),
    );
  }
}
