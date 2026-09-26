import 'dart:async';

import 'package:http/http.dart' show ClientException;
import 'package:piccolo_os/core/models/task_progress.dart';

// A missing response does not establish whether the server committed the update.
bool isAmbiguousApplyResponseError(Object error) =>
    error is ClientException || error is TimeoutException;

const pendingApplyOutcomeMessage =
    'Connection lost while applying. Waiting for the original operation result. '
    'You can close this window and check the app’s progress.';

String? applyAccessRepairMessage(TaskProgressEvent event, String fallback) {
  if (event.metadata?['access_repair_pending'] != true) return null;
  final message = event.metadata?['access_repair_message'];
  return message is String && message.isNotEmpty ? message : fallback;
}
