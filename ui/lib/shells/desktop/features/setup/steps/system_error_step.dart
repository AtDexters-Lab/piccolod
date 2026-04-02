import 'package:flutter/material.dart';
import 'package:piccolo_os/shared/widgets/diagnostic_log_download.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class SystemErrorStep extends StatelessWidget {
  const SystemErrorStep({required this.error, required this.onRetry, super.key});
  final String error;
  final VoidCallback onRetry;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          const Icon(PiccoloIcons.warning, color: PiccoloTheme.critical, size: 48),
          const SizedBox(height: 12),
          Text(
            'Storage operation failed',
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.w600),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 8),
          const Text(
            'This is a system error. Download the diagnostic log and contact support.',
            style: TextStyle(color: PiccoloTheme.inkMuted, fontSize: 13),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 12),
          ExpansionTile(
            tilePadding: EdgeInsets.zero,
            childrenPadding: EdgeInsets.zero,
            title: const Text('Details', style: TextStyle(fontSize: 13)),
            children: [
              ConstrainedBox(
                constraints: const BoxConstraints(maxHeight: 120),
                child: SingleChildScrollView(
                  padding: const EdgeInsets.symmetric(vertical: 8),
                  child: SelectionArea(
                    child: Text(
                      error,
                      style: const TextStyle(fontSize: 12, color: PiccoloTheme.inkMuted),
                    ),
                  ),
                ),
              ),
            ],
          ),
          const SizedBox(height: 24),
          const DiagnosticLogDownload(
            apiPath: '/api/v1/system/diagnostic-log',
            showTitle: false,
            showDescription: false,
          ),
          const SizedBox(height: 16),
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
