import 'package:flutter/material.dart';
import 'package:web/web.dart' as web;
import 'shells/desktop/desktop_shell.dart';
import 'shells/gateway/gateway_shell.dart';
import 'theme/piccolo_theme.dart';

void main() {
  // Determine if this is a gateway access (piccolo.local)
  final isGateway = _isGatewayAccess();

  if (isGateway) {
    runApp(const GatewayShell());
  } else {
    runApp(const PiccoloApp());
  }
}

/// Checks if the current access is via the gateway domain (piccolo.local).
/// Returns true if the hostname is exactly "piccolo.local".
bool _isGatewayAccess() {
  final host = web.window.location.hostname.toLowerCase();
  return host == 'piccolo.local';
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
