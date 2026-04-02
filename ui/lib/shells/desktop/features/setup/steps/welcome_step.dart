import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class WelcomeStep extends StatelessWidget {
  const WelcomeStep({required this.onNext, super.key});
  final VoidCallback onNext;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          Text(
            'Hello',
            style: PiccoloTheme.textTheme.displayLarge,
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 16),
          Text(
            "Let's set up your Piccolo.\nCreate an encryption password and save a recovery key. Takes about a minute.",
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              color: PiccoloTheme.inkMuted,
            ),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 40),
          FilledButton(
            onPressed: onNext,
            child: const Text('Start setup'),
          ),
        ],
      ),
    );
  }
}
