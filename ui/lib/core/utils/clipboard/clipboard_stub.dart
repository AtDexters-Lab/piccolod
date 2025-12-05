import 'package:flutter/services.dart';

Future<void> copyText(String text) async {
  await Clipboard.setData(ClipboardData(text: text));
}
