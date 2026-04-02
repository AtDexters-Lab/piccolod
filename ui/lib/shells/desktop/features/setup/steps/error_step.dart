import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class ErrorStep extends StatelessWidget {
  const ErrorStep({required this.error, required this.onRetry, super.key});
  final String error;
  final VoidCallback onRetry;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          const Icon(
            PiccoloIcons.wifiOff,
            color: PiccoloTheme.critical,
            size: 48,
          ),
          const SizedBox(height: 12),
          Text(
            "We couldn't reach Piccolo.",
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              fontWeight: FontWeight.w600,
            ),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 8),
          const Text(
            'Make sure your Piccolo is powered on and connected to the same network. If it just booted, wait a minute and retry.',
            style: TextStyle(color: PiccoloTheme.inkMuted, fontSize: 13),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 12),
          ExpansionTile(
            tilePadding: EdgeInsets.zero,
            childrenPadding: EdgeInsets.zero,
            title: const Text('Details', style: TextStyle(fontSize: 13)),
            children: [
              SelectionArea(
                child: Text(
                  error,
                  style: const TextStyle(
                    fontSize: 12,
                    color: PiccoloTheme.inkMuted,
                  ),
                ),
              ),
            ],
          ),
          const SizedBox(height: 24),
          FilledButton.icon(
            onPressed: onRetry,
            icon: const Icon(PiccoloIcons.refresh),
            label: const Text('Retry'),
            style: FilledButton.styleFrom(
              backgroundColor: PiccoloTheme.mist,
              foregroundColor: PiccoloTheme.ink,
            ),
          ),
        ],
      ),
    );
  }
}
