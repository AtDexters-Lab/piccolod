import 'package:flutter/material.dart';
import '../../theme/piccolo_theme.dart';

/// A dialog that confirms app uninstallation with an option to purge data.
///
/// Use [show] to display the dialog and get the result.
/// Returns a record `({bool confirmed, bool purgeData})` if user confirms,
/// or `null` if cancelled.
class UninstallConfirmationDialog extends StatefulWidget {
  final String appDisplayTitle;

  const UninstallConfirmationDialog({
    super.key,
    required this.appDisplayTitle,
  });

  /// Shows the dialog and returns the result.
  ///
  /// Returns a record of (confirmed, purgeData) or null if cancelled.
  static Future<({bool confirmed, bool purgeData})?> show(
    BuildContext context, {
    required String appDisplayTitle,
  }) async {
    return showDialog<({bool confirmed, bool purgeData})>(
      context: context,
      builder: (context) => UninstallConfirmationDialog(
        appDisplayTitle: appDisplayTitle,
      ),
    );
  }

  @override
  State<UninstallConfirmationDialog> createState() =>
      _UninstallConfirmationDialogState();
}

class _UninstallConfirmationDialogState
    extends State<UninstallConfirmationDialog> {
  bool _purgeData = false;

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: Text("Uninstall ${widget.appDisplayTitle}?"),
      content: Column(
        mainAxisSize: MainAxisSize.min,
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          const Text("This will remove the application container."),
          const SizedBox(height: 16),
          Container(
            decoration: BoxDecoration(
              color: PiccoloTheme.critical.withValues(alpha: 0.1),
              borderRadius: BorderRadius.circular(8),
              border: Border.all(
                color: PiccoloTheme.critical.withValues(alpha: 0.3),
              ),
            ),
            child: CheckboxListTile(
              title: const Text(
                "Delete Data Volumes",
                style: TextStyle(
                  color: PiccoloTheme.critical,
                  fontWeight: FontWeight.bold,
                ),
              ),
              subtitle: const Text("This action cannot be undone."),
              value: _purgeData,
              activeColor: PiccoloTheme.critical,
              onChanged: (val) => setState(() => _purgeData = val ?? false),
              controlAffinity: ListTileControlAffinity.leading,
              contentPadding: const EdgeInsets.symmetric(horizontal: 8),
            ),
          ),
        ],
      ),
      actions: [
        TextButton(
          onPressed: () => Navigator.of(context).pop(null),
          child: const Text("Cancel"),
        ),
        FilledButton(
          style: FilledButton.styleFrom(
            backgroundColor: PiccoloTheme.critical,
          ),
          onPressed: () => Navigator.of(context).pop(
            (confirmed: true, purgeData: _purgeData),
          ),
          child: const Text("Uninstall"),
        ),
      ],
    );
  }
}
