import 'package:flutter/material.dart';
import 'shells/desktop/desktop_shell.dart';
import 'theme/piccolo_theme.dart';

void main() {
  runApp(const PiccoloApp());
}

class PiccoloApp extends StatelessWidget {
  const PiccoloApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Piccolo',
      debugShowCheckedModeBanner: false,
      theme: PiccoloTheme.lightTheme,
      // In the future, we can detect platform/screen size here to choose shell.
      home: const DesktopShell(),
    );
  }
}
