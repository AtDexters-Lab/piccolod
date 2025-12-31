import 'package:flutter/material.dart';

import '../../../../shared/widgets/terminal_widget_mixin.dart';

/// Host terminal app widget.
/// Connects to the host system's shell via /api/v1/terminal WebSocket.
class TerminalApp extends StatefulWidget {
  const TerminalApp({super.key});

  @override
  State<TerminalApp> createState() => _TerminalAppState();
}

class _TerminalAppState extends State<TerminalApp> with TerminalWidgetMixin {
  @override
  String getTerminalPath() => '/api/v1/terminal';

  @override
  void initState() {
    super.initState();
    initTerminal();
    WidgetsBinding.instance.addPostFrameCallback((_) => connectTerminal());
  }

  @override
  void dispose() {
    disposeTerminal();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => buildTerminalView();
}
