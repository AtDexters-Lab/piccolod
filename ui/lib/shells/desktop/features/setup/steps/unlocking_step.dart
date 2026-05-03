import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Transient state shown on the unlock screen while any post-decrypt
/// chain is in flight — auto-unlock pickup, manual /crypto/unlock from
/// another tab, or a recovery-key reset's transient unlock. Shown in
/// place of the password prompt; when the chain completes, the parent
/// re-routes via /system/boot (success → desktop; failure → password
/// prompt with no failure callout).
class UnlockingStep extends StatelessWidget {
  const UnlockingStep({super.key});

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
            'Unlocking…',
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
