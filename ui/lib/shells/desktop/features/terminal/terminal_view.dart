import 'package:flutter/material.dart';

import '../../../../shared/widgets/terminal_widget_mixin.dart';

/// Host terminal app widget.
/// Connects to the host system's shell via /api/v1/terminal WebSocket.
class TerminalApp extends StatefulWidget {
  /// Optional callback when terminal session ends normally (e.g., Ctrl+D).
  final void Function()? onSessionEnd;

  const TerminalApp({super.key, this.onSessionEnd});

  @override
  State<TerminalApp> createState() => _TerminalAppState();
}

class _TerminalAppState extends State<TerminalApp> with TerminalWidgetMixin {
  @override
  String getTerminalPath() => '/api/v1/terminal';

  @override
  void Function()? getOnSessionEnd() => widget.onSessionEnd;

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
