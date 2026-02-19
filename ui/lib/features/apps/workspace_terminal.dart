import 'package:flutter/material.dart';

import 'package:piccolo_os/shared/widgets/terminal_widget_mixin.dart';

/// Workspace terminal widget.
/// Connects to a workspace container's shell via persistent terminal sessions.
class WorkspaceTerminal extends StatefulWidget {

  const WorkspaceTerminal({
    required this.appId, super.key,
    this.serviceName,
    this.onSessionEnd,
  });
  /// The app instance ID (name) to connect to.
  final String appId;

  /// Optional service/container name for multi-container apps.
  /// If omitted, the backend defaults to the primary service.
  final String? serviceName;

  /// Optional callback when terminal session ends normally (e.g., Ctrl+D).
  final void Function()? onSessionEnd;

  @override
  State<WorkspaceTerminal> createState() => _WorkspaceTerminalState();
}

class _WorkspaceTerminalState extends State<WorkspaceTerminal>
    with TerminalWidgetMixin {
  @override
  String getSessionCreatePath() {
    final id = Uri.encodeComponent(widget.appId);
    final path = '/api/v1/apps/$id/terminal/sessions';

    final svc = widget.serviceName?.trim();
    if (svc == null || svc.isEmpty) return path;

    return Uri(path: path, queryParameters: {'service': svc}).toString();
  }

  @override
  String getSessionAttachPath(String sessionId) {
    final id = Uri.encodeComponent(widget.appId);
    return '/api/v1/apps/$id/terminal/sessions/$sessionId/attach';
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
