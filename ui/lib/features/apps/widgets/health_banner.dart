import 'package:flutter/material.dart';
import 'package:url_launcher/url_launcher.dart';
import '../../../core/models/listener_health.dart';
import '../../../core/services/app_service.dart';
import '../../../theme/piccolo_theme.dart';
import '../../../theme/piccolo_icons.dart';
import '../../../shells/desktop/desktop_controller.dart';
import '../app_launcher.dart';
import 'local_fallback_overlay.dart';

class AppDetailHealthBanner extends StatefulWidget {
  final ListenerHealth health;
  final String lanFallbackUrl;
  final AppService appService;
  final DesktopController? desktopController;

  const AppDetailHealthBanner({
    super.key,
    required this.health,
    required this.lanFallbackUrl,
    required this.appService,
    this.desktopController,
  });

  @override
  State<AppDetailHealthBanner> createState() => _AppDetailHealthBannerState();
}

class _AppDetailHealthBannerState extends State<AppDetailHealthBanner> {
  bool _showDetails = false;

  bool get _isLocalAccess =>
      AppLauncher.isLocalAccess(Uri.base.host.toLowerCase());

  String? get _actionableCertId {
    final certs = widget.health.certStatuses;
    if (certs == null || certs.isEmpty) return null;
    for (final entry in certs.entries) {
      if (entry.value.reasonCode == widget.health.reasonCode) {
        return entry.key;
      }
    }
    for (final entry in certs.entries) {
      if (entry.value.status != 'ok') {
        return entry.key;
      }
    }
    return null;
  }

  @override
  Widget build(BuildContext context) {
    final health = widget.health;
    if (health.isOk) return const SizedBox.shrink();

    return Container(
      padding: const EdgeInsets.all(Spacing.base),
      color: ListenerHealthVisuals.backgroundForStatus(health.status),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Row(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Icon(
                ListenerHealthVisuals.iconForStatus(health.status),
                color: ListenerHealthVisuals.colorForStatus(health.status),
                size: 20,
              ),
              const SizedBox(width: Spacing.md),
              Expanded(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Text(
                      health.reason,
                      style: const TextStyle(fontWeight: FontWeight.bold),
                    ),
                    if (health.recoveryEta != null) ...[
                      const SizedBox(height: 2),
                      Text(
                        '${health.actionRequired ? 'Next check' : 'Next retry'}: ${health.recoveryEta}',
                        style: PiccoloTheme.textTheme.labelSmall,
                      ),
                    ],
                    const SizedBox(height: Spacing.xs),
                    if (health.actionRequired)
                      Text(
                        'Action required - check Remote Access settings.',
                        style: TextStyle(
                          fontSize: 12,
                          color: PiccoloTheme.warning,
                        ),
                      )
                    else
                      Text(
                        'No action needed - the system is working on it.',
                        style: TextStyle(
                          fontSize: 12,
                          color: PiccoloTheme.inkMuted,
                        ),
                      ),
                  ],
                ),
              ),
              // CTAs column
              Column(
                mainAxisSize: MainAxisSize.min,
                crossAxisAlignment: CrossAxisAlignment.end,
                children: [
                  if (health.actionRequired) ...[
                    TextButton(
                      onPressed: _navigateToRemoteSettings,
                      child: const Text('Settings'),
                    ),
                    TextButton(
                      onPressed: () => _retryNow(_actionableCertId),
                      child: const Text('Retry Now'),
                    ),
                  ],
                  if (_isLocalAccess)
                    TextButton(
                      onPressed: () =>
                          launchUrl(Uri.parse(widget.lanFallbackUrl)),
                      child: const Text('Access Locally'),
                    )
                  else if (widget.lanFallbackUrl.isNotEmpty)
                    CopyableUrl(url: widget.lanFallbackUrl, compact: true),
                ],
              ),
            ],
          ),
          // Collapsible details
          if (health.details != null && health.details!.isNotEmpty) ...[
            const SizedBox(height: Spacing.sm),
            GestureDetector(
              onTap: () => setState(() => _showDetails = !_showDetails),
              child: Row(
                children: [
                  Icon(
                    _showDetails ? PiccoloIcons.expandLess : PiccoloIcons.expandMore,
                    size: 16,
                    color: PiccoloTheme.cobalt600,
                  ),
                  Text(
                    _showDetails ? 'Hide details' : 'Show details',
                    style: const TextStyle(
                      fontSize: 12,
                      color: PiccoloTheme.cobalt600,
                    ),
                  ),
                ],
              ),
            ),
            if (_showDetails)
              Padding(
                padding: const EdgeInsets.only(top: Spacing.sm),
                child: Text(
                  health.details!,
                  style: const TextStyle(
                    fontSize: 11,
                    fontFamily: 'JetBrainsMono',
                  ),
                ),
              ),
          ],
        ],
      ),
    );
  }

  void _navigateToRemoteSettings() {
    final controller = widget.desktopController;
    if (controller == null) {
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(content: Text('Remote Access settings not available')),
      );
      return;
    }
    openOrFocusSettings(controller);
  }

  Future<void> _retryNow(String? certId) async {
    if (certId == null) return;
    try {
      await widget.appService.renewCertificate(certId);
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(content: Text('Retry queued')),
      );
    } catch (e) {
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(content: Text('Retry failed: $e')),
      );
    }
  }
}
