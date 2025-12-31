import 'package:flutter/material.dart';

import '../../shared/widgets/terminal_widget_mixin.dart';

/// Workspace terminal widget.
/// Connects to a workspace container's shell via /api/v1/apps/:appId/terminal WebSocket.
class WorkspaceTerminal extends StatefulWidget {
  /// The app instance ID (name) to connect to.
  final String appId;

  const WorkspaceTerminal({super.key, required this.appId});

  @override
  State<WorkspaceTerminal> createState() => _WorkspaceTerminalState();
}

class _WorkspaceTerminalState extends State<WorkspaceTerminal>
    with TerminalWidgetMixin {
  @override
  String getTerminalPath() => '/api/v1/apps/${widget.appId}/terminal';

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
