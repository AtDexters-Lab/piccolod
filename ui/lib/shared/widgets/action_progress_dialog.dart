import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/utils/task_id.dart';
import 'package:piccolo_os/shared/widgets/task_progress_panel.dart';

/// Runs an async action with a progress dialog overlay.
///
/// Shows a dialog with [TaskProgressPanel], executes [action], waits briefly
/// for the progress stream to finish, then dismisses the dialog.
///
/// Returns `true` if the action completed without error, `false` otherwise.
/// If an error occurs, a [SnackBar] is shown with the error message.
Future<bool> runWithProgressDialog({
  required BuildContext context,
  required String title,
  required String taskType,
  required Future<void> Function(String taskId) action,
}) async {
  final taskId = generateTaskId();
  final progressDone = Completer<void>();

  unawaited(showDialog<void>(
    context: context,
    barrierDismissible: false,
    builder: (context) => AlertDialog(
      title: Text(title),
      content: SizedBox(
        width: 520,
        child: TaskProgressPanel(
          taskId: taskId,
          taskType: taskType,
          onComplete: (_) {
            if (!progressDone.isCompleted) progressDone.complete();
          },
        ),
      ),
    ),
  ));

  final navigator = Navigator.of(context);
  final messenger = ScaffoldMessenger.of(context);
  Object? error;

  try {
    await action(taskId);
    await progressDone.future.timeout(
      const Duration(seconds: 2),
      onTimeout: () {},
    );
  } on Object catch (e) {
    error = e;
  } finally {
    if (navigator.canPop()) {
      navigator.pop();
    }
  }

  if (error != null) {
    messenger.showSnackBar(
      SnackBar(content: Text('$title failed: $error')),
    );
    return false;
  }

  return true;
}
