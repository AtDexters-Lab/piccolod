import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Transient state shown on the unlock screen while the auto-unlock
/// pickup goroutine is in flight. Shown in place of the password prompt;
/// when the goroutine completes, the parent re-routes via /system/boot
/// (success → desktop; failure → password prompt with no failure callout).
class AutoUnlockingStep extends StatelessWidget {
  const AutoUnlockingStep({super.key});

  @override
  Widget build(BuildContext context) {
    return const Padding(
      padding: EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          SizedBox(
            width: 48,
            height: 48,
            child: CircularProgressIndicator(
              strokeWidth: 3,
              color: PiccoloTheme.cobalt600,
            ),
          ),
          SizedBox(height: 24),
          Text(
            'Auto-unlocking…',
            style: TextStyle(fontSize: 20, fontWeight: FontWeight.w600),
          ),
          SizedBox(height: 8),
          Text(
            'Restoring your encrypted disk.',
            style: TextStyle(color: PiccoloTheme.inkMuted),
            textAlign: TextAlign.center,
          ),
        ],
      ),
    );
  }
}
