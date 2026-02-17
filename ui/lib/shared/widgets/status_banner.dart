import 'package:flutter/material.dart';
import '../../theme/piccolo_icons.dart';
import '../../theme/piccolo_theme.dart';

/// Severity level for [StatusBanner].
enum BannerSeverity { info, warning, error }

/// Inline status banner replacing the various `_buildStartingBanner`,
/// `_buildErrorBanner`, etc. patterns across the codebase.
class StatusBanner extends StatelessWidget {
  final BannerSeverity severity;
  final String title;
  final String? message;
  final Widget? action;
  final IconData? icon;

  const StatusBanner({
    super.key,
    required this.severity,
    required this.title,
    this.message,
    this.action,
    this.icon,
  });

  Color get _accent {
    switch (severity) {
      case BannerSeverity.info:
        return PiccoloTheme.info;
      case BannerSeverity.warning:
        return PiccoloTheme.warning;
      case BannerSeverity.error:
        return PiccoloTheme.critical;
    }
  }

  IconData get _icon {
    switch (severity) {
      case BannerSeverity.info:
        return PiccoloIcons.info;
      case BannerSeverity.warning:
        return PiccoloIcons.warning;
      case BannerSeverity.error:
        return PiccoloIcons.error;
    }
  }

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.all(Spacing.md),
      decoration: BoxDecoration(
        color: _accent.withValues(alpha: 0.08),
        borderRadius: BorderRadius.circular(Radii.sm),
        border: Border.all(color: _accent.withValues(alpha: 0.2)),
      ),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Icon(icon ?? _icon, color: _accent, size: 20),
          const SizedBox(width: Spacing.sm),
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(
                  title,
                  style: Theme.of(context).textTheme.titleSmall?.copyWith(
                    color: _accent,
                  ),
                ),
                if (message != null) ...[
                  const SizedBox(height: Spacing.xs),
                  Text(
                    message!,
                    style: Theme.of(context).textTheme.bodySmall?.copyWith(
                      color: _accent,
                    ),
                  ),
                ],
                if (action != null) ...[
                  const SizedBox(height: Spacing.sm),
                  action!,
                ],
              ],
            ),
          ),
        ],
      ),
    );
  }
}
