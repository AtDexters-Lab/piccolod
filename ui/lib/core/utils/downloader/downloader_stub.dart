import 'package:flutter/foundation.dart';

/// Throws an error on non-web platforms where dart:html is unavailable.
void downloadTextFile(String content, String filename) {
  // On Desktop, we would typically use file_selector or path_provider.
  // Since we are keeping dependencies minimal, we log this for now.
  debugPrint(
    "Download not supported on this platform without external packages.",
  );
}
