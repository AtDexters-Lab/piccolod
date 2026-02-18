import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/listener_health.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class HealthBadge extends StatelessWidget {

  const HealthBadge({required this.health, super.key});
  final ListenerHealth? health;

  @override
  Widget build(BuildContext context) {
    if (health == null || health!.isOk) return const SizedBox.shrink();

    return Tooltip(
      message: health!.reason,
      child: Container(
        padding: const EdgeInsets.all(Spacing.xs),
        decoration: BoxDecoration(
          color: ListenerHealthVisuals.colorForStatus(health!.status),
          borderRadius: BorderRadius.circular(Spacing.xs),
        ),
        child: Icon(
          ListenerHealthVisuals.iconForStatus(health!.status),
          size: 16,
          color: PiccoloTheme.porcelain,
        ),
      ),
    );
  }
}
