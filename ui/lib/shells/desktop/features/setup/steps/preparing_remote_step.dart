import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class PreparingRemoteStep extends StatelessWidget {
  const PreparingRemoteStep({
    required this.relayReady,
    required this.certReady,
    required this.onSkip,
    super.key,
  });

  final bool relayReady;
  final bool certReady;
  final VoidCallback onSkip;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 16, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          const SizedBox(height: 8),
          const CircularProgressIndicator(color: PiccoloTheme.cobalt600),
          const SizedBox(height: 24),
          Text(
            'Preparing your remote address\u2026',
            style: PiccoloTheme.textTheme.titleMedium,
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 24),
          _readinessRow('Connecting relay', relayReady),
          const SizedBox(height: 8),
          _readinessRow('Securing certificate', certReady),
          const SizedBox(height: 24),
          TextButton(
            onPressed: onSkip,
            child: const Text('Continue on local network'),
          ),
        ],
      ),
    );
  }

  Widget _readinessRow(String label, bool done) {
    return Row(
      mainAxisSize: MainAxisSize.min,
      children: [
        if (done)
          const Icon(PiccoloIcons.success, size: 18, color: PiccoloTheme.success)
        else
          const SizedBox(
            width: 18,
            height: 18,
            child: CircularProgressIndicator(strokeWidth: 2, color: PiccoloTheme.cobalt600),
          ),
        const SizedBox(width: 8),
        Text(label, style: PiccoloTheme.textTheme.bodyMedium),
      ],
    );
  }
}
