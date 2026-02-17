import 'package:flutter/material.dart';
import '../../theme/piccolo_theme.dart';

/// Key-value row used in settings info cards and profile panels.
///
/// Replaces the duplicated `_InfoRow` widgets in `profile_tab.dart`
/// and `system_tab.dart`.
class InfoRow extends StatelessWidget {
  final String label;
  final String value;

  const InfoRow(this.label, this.value, {super.key});

  @override
  Widget build(BuildContext context) {
    final tt = Theme.of(context).textTheme;
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: Spacing.sm),
      child: Row(
        mainAxisAlignment: MainAxisAlignment.spaceBetween,
        children: [
          Text(label, style: tt.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted)),
          const SizedBox(width: Spacing.base),
          Expanded(
            child: Text(
              value,
              style: tt.bodyMedium,
              textAlign: TextAlign.right,
              overflow: TextOverflow.ellipsis,
            ),
          ),
        ],
      ),
    );
  }
}
