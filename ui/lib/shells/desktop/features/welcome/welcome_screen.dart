import 'package:flutter/material.dart';
import 'package:piccolo_os/shared/piccolo_wordmark.dart';
import 'package:piccolo_os/shells/desktop/desktop_controller.dart';
import 'package:piccolo_os/shells/desktop/features/settings/settings_app.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Post-setup welcome screen shown as a desktop window after first boot.
///
/// Displays the Piccolo wordmark, a welcome tagline, and quick-start
/// action cards that open core apps.
class WelcomeScreen extends StatelessWidget {

  const WelcomeScreen({required this.controller, super.key});
  final DesktopController controller;

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      backgroundColor: PiccoloTheme.mist,
      body: Center(
        child: SingleChildScrollView(
          padding: const EdgeInsets.all(Spacing.xxl),
          child: Column(
            mainAxisSize: MainAxisSize.min,
            children: [
              const PiccoloWordmark(height: 32, color: PiccoloTheme.ink),
              const SizedBox(height: Spacing.lg),
              Text(
                'Hello, and welcome to your personal\ncorner of the internet.',
                style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
                  color: PiccoloTheme.inkMuted,
                ),
                textAlign: TextAlign.center,
              ),
              const SizedBox(height: Spacing.xxl),
              Wrap(
                spacing: Spacing.base,
                runSpacing: Spacing.base,
                alignment: WrapAlignment.center,
                children: [
                  _ActionCard(
                    icon: PiccoloIcons.store,
                    title: 'Browse Apps',
                    subtitle: 'Install your first app\nfrom the store',
                    onTap: controller.openAppStore,
                  ),
                  _ActionCard(
                    icon: PiccoloIcons.cloud,
                    title: 'Access Anywhere',
                    subtitle: 'Connect to your\nPiccolo remotely',
                    onTap: () => controller.openSettings(
                      initialTab: SettingsTab.remoteAccess,
                    ),
                  ),
                  _ActionCard(
                    icon: PiccoloIcons.settings,
                    title: 'Explore Settings',
                    subtitle: 'Users, network, and\nsystem preferences',
                    onTap: controller.openSettings,
                  ),
                ],
              ),
            ],
          ),
        ),
      ),
    );
  }
}

class _ActionCard extends StatefulWidget {

  const _ActionCard({
    required this.icon,
    required this.title,
    required this.subtitle,
    required this.onTap,
  });
  final IconData icon;
  final String title;
  final String subtitle;
  final VoidCallback onTap;

  @override
  State<_ActionCard> createState() => _ActionCardState();
}

class _ActionCardState extends State<_ActionCard> {
  bool _hovered = false;

  @override
  Widget build(BuildContext context) {
    final tt = PiccoloTheme.textTheme;
    return MouseRegion(
      onEnter: (_) => setState(() => _hovered = true),
      onExit: (_) => setState(() => _hovered = false),
      cursor: SystemMouseCursors.click,
      child: GestureDetector(
        onTap: widget.onTap,
        child: AnimatedContainer(
          duration: Motion.fast,
          curve: Motion.standard,
          width: 172,
          padding: const EdgeInsets.all(Spacing.lg),
          decoration: BoxDecoration(
            color: PiccoloTheme.porcelain,
            borderRadius: BorderRadius.circular(Radii.md),
            border: Border.all(
              color: _hovered ? PiccoloTheme.cobalt600 : PiccoloTheme.hairline,
            ),
            boxShadow: _hovered ? Elevation.elev2 : Elevation.elev0,
          ),
          child: Column(
            mainAxisSize: MainAxisSize.min,
            children: [
              Icon(
                widget.icon,
                size: 28,
                color: _hovered ? PiccoloTheme.cobalt600 : PiccoloTheme.inkMuted,
              ),
              const SizedBox(height: Spacing.md),
              Text(
                widget.title,
                style: tt.titleSmall,
                textAlign: TextAlign.center,
              ),
              const SizedBox(height: Spacing.xs),
              Text(
                widget.subtitle,
                style: tt.labelSmall,
                textAlign: TextAlign.center,
              ),
            ],
          ),
        ),
      ),
    );
  }
}
