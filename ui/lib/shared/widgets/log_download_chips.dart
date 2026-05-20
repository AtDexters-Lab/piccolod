import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Mode toggle chip shared by the diagnostic-log and app-log download widgets.
class LogModeChip extends StatelessWidget {
  const LogModeChip({
    required this.label,
    required this.isActive,
    required this.onTap,
    super.key,
  });

  final String label;
  final bool isActive;
  final VoidCallback onTap;

  @override
  Widget build(BuildContext context) {
    return InkWell(
      borderRadius: BorderRadius.circular(Radii.sm),
      onTap: onTap,
      child: Container(
        padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 6),
        decoration: BoxDecoration(
          borderRadius: BorderRadius.circular(Radii.sm),
          border: Border.all(
            color: isActive
                ? PiccoloTheme.cobalt600
                : PiccoloTheme.ink.withValues(alpha: 0.1),
          ),
          color:
              isActive ? PiccoloTheme.cobalt600.withValues(alpha: 0.05) : null,
        ),
        child: Text(
          label,
          style: PiccoloTheme.textTheme.labelSmall?.copyWith(
            color: isActive ? PiccoloTheme.cobalt600 : null,
            fontWeight: isActive ? FontWeight.w600 : null,
          ),
        ),
      ),
    );
  }
}

/// Collapsible From/To date chip shared by the log download widgets.
class LogDateChip extends StatelessWidget {
  const LogDateChip({
    required this.label,
    required this.value,
    required this.isActive,
    required this.onTap,
    super.key,
  });

  final String label;
  final String value;
  final bool isActive;
  final VoidCallback onTap;

  @override
  Widget build(BuildContext context) {
    return InkWell(
      borderRadius: BorderRadius.circular(Radii.sm),
      onTap: onTap,
      child: Container(
        padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 8),
        decoration: BoxDecoration(
          borderRadius: BorderRadius.circular(Radii.sm),
          border: Border.all(
            color: isActive
                ? PiccoloTheme.cobalt600
                : PiccoloTheme.ink.withValues(alpha: 0.1),
          ),
          color:
              isActive ? PiccoloTheme.cobalt600.withValues(alpha: 0.05) : null,
        ),
        child: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            Text('$label: ', style: PiccoloTheme.textTheme.labelSmall),
            Text(value,
                style: PiccoloTheme.textTheme.bodyMedium
                    ?.copyWith(fontWeight: FontWeight.w500)),
            const SizedBox(width: 4),
            Icon(
              isActive ? PiccoloIcons.expandLess : PiccoloIcons.calendar,
              size: 14,
              color: isActive ? PiccoloTheme.cobalt600 : PiccoloTheme.inkMuted,
            ),
          ],
        ),
      ),
    );
  }
}
