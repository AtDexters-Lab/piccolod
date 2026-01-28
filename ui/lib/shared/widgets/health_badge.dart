import 'package:flutter/material.dart';
import '../../core/models/listener_health.dart';

class HealthBadge extends StatelessWidget {
  final ListenerHealth? health;

  const HealthBadge({super.key, required this.health});

  @override
  Widget build(BuildContext context) {
    if (health == null || health!.isOk) return const SizedBox.shrink();

    return Tooltip(
      message: health!.reason,
      child: Container(
        padding: const EdgeInsets.all(4),
        decoration: BoxDecoration(
          color: ListenerHealthVisuals.colorForStatus(health!.status),
          borderRadius: BorderRadius.circular(4),
        ),
        child: Icon(
          ListenerHealthVisuals.iconForStatus(health!.status),
          size: 16,
          color: Colors.white,
        ),
      ),
    );
  }
}
