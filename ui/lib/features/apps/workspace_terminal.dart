import 'package:flutter/material.dart';

import '../../shared/widgets/terminal_widget_mixin.dart';

/// Workspace terminal widget.
/// Connects to a workspace container's shell via /api/v1/apps/:appId/terminal WebSocket.
class WorkspaceTerminal extends StatefulWidget {
  /// The app instance ID (name) to connect to.
  final String appId;

  /// Optional service/container name for multi-container apps.
  /// If omitted, the backend defaults to the primary service.
  final String? serviceName;

  /// Optional callback when terminal session ends normally (e.g., Ctrl+D).
  final void Function()? onSessionEnd;

  const WorkspaceTerminal({
    super.key,
    required this.appId,
    this.serviceName,
    this.onSessionEnd,
  });

  @override
  State<WorkspaceTerminal> createState() => _WorkspaceTerminalState();
}

class _WorkspaceTerminalState extends State<WorkspaceTerminal>
    with TerminalWidgetMixin {
  @override
  String getTerminalPath() {
    final id = Uri.encodeComponent(widget.appId);
    final path = '/api/v1/apps/$id/terminal';

    final svc = widget.serviceName?.trim();
    if (svc == null || svc.isEmpty) return path;

    return Uri(path: path, queryParameters: {'service': svc}).toString();
  }

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
