import 'package:flutter/material.dart';
import '../../theme/piccolo_theme.dart';

/// A dialog that confirms app uninstallation.
///
/// Use [show] to display the dialog and get the result.
/// Returns `true` if user confirms, or `null` if cancelled.
class UninstallConfirmationDialog extends StatelessWidget {
  final String appDisplayTitle;

  const UninstallConfirmationDialog({
    super.key,
    required this.appDisplayTitle,
  });

  /// Shows the dialog and returns the result.
  ///
  /// Returns true if confirmed, or null if cancelled.
  static Future<bool?> show(
    BuildContext context, {
    required String appDisplayTitle,
  }) async {
    return showDialog<bool>(
      context: context,
      builder: (context) => UninstallConfirmationDialog(
        appDisplayTitle: appDisplayTitle,
      ),
    );
  }

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: Text("Uninstall $appDisplayTitle?"),
      content: const Text(
        "This will remove the application and delete all its data. "
        "This action cannot be undone.",
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
          onPressed: () => Navigator.of(context).pop(true),
          child: const Text("Uninstall"),
        ),
      ],
    );
  }
}
