import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class OnboardingStep extends StatefulWidget {

  const OnboardingStep({
    required this.onTryPiccolo, required this.onInstallDisk, super.key,
    this.error,
  });
  final Future<void> Function() onTryPiccolo;
  final Future<void> Function() onInstallDisk;
  final String? error;

  @override
  State<OnboardingStep> createState() => _OnboardingStepState();
}

class _OnboardingStepState extends State<OnboardingStep> {
  bool _isLoading = false;

  Future<void> _handleChoice(Future<void> Function() action) async {
    if (_isLoading) return;
    setState(() => _isLoading = true);
    await action();
    if (mounted) setState(() => _isLoading = false);
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          Text(
            'Welcome to Piccolo',
            style: PiccoloTheme.textTheme.displayLarge,
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 8),
          Text(
            'How would you like to use this device?',
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              color: PiccoloTheme.inkMuted,
            ),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 32),
          if (widget.error != null) ...[
            Container(
              padding: const EdgeInsets.all(12),
              decoration: BoxDecoration(
                color: PiccoloTheme.critical.withValues(alpha: 0.08),
                borderRadius: BorderRadius.circular(Radii.sm),
                border: Border.all(
                  color: PiccoloTheme.critical.withValues(alpha: 0.2),
                ),
              ),
              child: Text(
                widget.error!,
                style: PiccoloTheme.textTheme.labelMedium?.copyWith(
                  color: PiccoloTheme.critical,
                ),
              ),
            ),
            const SizedBox(height: 16),
          ],
          _ChoiceCard(
            icon: PiccoloIcons.usb,
            title: 'Try Piccolo',
            description:
                'Run Piccolo from this USB drive. Full functionality, no installation required.',
            onTap: _isLoading ? null : () => _handleChoice(widget.onTryPiccolo),
          ),
          const SizedBox(height: 12),
          _ChoiceCard(
            icon: PiccoloIcons.saveToDisk,
            title: 'Install to Disk',
            description:
                'Install Piccolo to an internal disk for permanent use. Requires internet connection.',
            onTap: _isLoading
                ? null
                : () => _handleChoice(widget.onInstallDisk),
          ),
          if (_isLoading) ...[
            const SizedBox(height: 24),
            const SizedBox(
              width: 24,
              height: 24,
              child: CircularProgressIndicator(
                strokeWidth: 2,
                color: PiccoloTheme.cobalt600,
              ),
            ),
          ],
        ],
      ),
    );
  }
}

class _ChoiceCard extends StatelessWidget {

  const _ChoiceCard({
    required this.icon,
    required this.title,
    required this.description,
    required this.onTap,
  });
  final IconData icon;
  final String title;
  final String description;
  final VoidCallback? onTap;

  @override
  Widget build(BuildContext context) {
    final isEnabled = onTap != null;

    return Material(
      color: PiccoloTheme.porcelain,
      borderRadius: BorderRadius.circular(Radii.sm),
      child: InkWell(
        borderRadius: BorderRadius.circular(Radii.sm),
        onTap: onTap,
        child: Container(
          padding: const EdgeInsets.all(20),
          decoration: BoxDecoration(
            borderRadius: BorderRadius.circular(Radii.sm),
            border: Border.all(
              color: isEnabled
                  ? PiccoloTheme.cobalt600.withValues(alpha: 0.3)
                  : PiccoloTheme.ink.withValues(alpha: 0.1),
            ),
          ),
          child: Row(
            children: [
              Container(
                padding: const EdgeInsets.all(12),
                decoration: BoxDecoration(
                  color: isEnabled
                      ? PiccoloTheme.cobalt600.withValues(alpha: 0.1)
                      : PiccoloTheme.mist,
                  borderRadius: BorderRadius.circular(10),
                ),
                child: Icon(
                  icon,
                  color: isEnabled
                      ? PiccoloTheme.cobalt600
                      : PiccoloTheme.inkMuted,
                  size: 24,
                ),
              ),
              const SizedBox(width: 16),
              Expanded(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Text(
                      title,
                      style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
                        fontWeight: FontWeight.w600,
                        color: isEnabled ? PiccoloTheme.ink : PiccoloTheme.inkMuted,
                      ),
                    ),
                    const SizedBox(height: 4),
                    Text(
                      description,
                      style: const TextStyle(
                        fontSize: 13,
                        color: PiccoloTheme.inkMuted,
                      ),
                    ),
                  ],
                ),
              ),
              Icon(
                PiccoloIcons.arrowForwardIos,
                size: 16,
                color: isEnabled
                    ? PiccoloTheme.inkMuted
                    : PiccoloTheme.ink.withValues(alpha: 0.1),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
