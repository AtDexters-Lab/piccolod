import 'package:piccolo_os/core/services/api_client.dart';

const staleUpdatePreviewMessage =
    'This preview is out of date. Preview the changes again before applying.';

String updateErrorMessage(Object error) {
  if (error is ApiException) return error.message;
  return error.toString();
}

bool isStaleUpdatePreviewError(Object error) {
  if (error is! ApiException || error.statusCode != 409) return false;
  final key = error.key?.toLowerCase();
  if (key == 'update_preview_stale') return true;
  final message = error.message.toLowerCase();
  return message.contains('dry run') ||
      message.contains('changed after') ||
      message.contains('changed during') ||
      message.contains('no longer matches') ||
      message.contains('does not match') ||
      message.contains('rerun') ||
      message.contains('token is expired');
}
