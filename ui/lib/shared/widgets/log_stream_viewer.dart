import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:piccolo_os/core/config/core_config.dart';
import 'package:piccolo_os/core/services/error_reporter.dart';
import 'package:piccolo_os/core/services/log_stream_backend.dart';
import 'package:piccolo_os/core/services/websocket_connection.dart';
import 'package:piccolo_os/core/utils/clipboard/clipboard.dart' as clipboard_utils;
import 'package:piccolo_os/shared/widgets/render_error_boundary.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:xterm/xterm.dart';

class LogStreamViewer extends StatefulWidget {

  const LogStreamViewer({
    super.key,
    this.appName,
    this.systemUnit,
    this.serviceName,
    this.tailLines = 200,
    this.height,
    this.autoConnect = true,
  }) : assert(
         (appName == null) != (systemUnit == null),
         'Provide exactly one of appName or systemUnit',
       );
  final String? appName;
  final String? systemUnit;
  final String? serviceName;
  final int tailLines;
  final double? height;
  final bool autoConnect;

  @override
  State<LogStreamViewer> createState() => _LogStreamViewerState();
}

class _LogStreamViewerState extends State<LogStreamViewer> {
  late final Terminal _terminal;
  late final TerminalController _controller;
  late final ScrollController _scrollController;

  LogStreamBackend? _backend;

  @override
  void initState() {
    super.initState();
    _terminal = Terminal(maxLines: 10000);
    _controller = TerminalController();
    _scrollController = ScrollController();

    WidgetsBinding.instance.addPostFrameCallback((_) {
      if (!mounted) return;
      if (widget.autoConnect) {
        _connect();
      }
    });
  }

  @override
  void didUpdateWidget(LogStreamViewer oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (oldWidget.appName != widget.appName ||
        oldWidget.systemUnit != widget.systemUnit ||
        oldWidget.serviceName != widget.serviceName ||
        oldWidget.tailLines != widget.tailLines) {
      _disconnect();
      if (widget.autoConnect) {
        _connect();
      }
    }
  }

  @override
  void dispose() {
    _backend?.dispose();
    _scrollController.dispose();
    _controller.dispose();
    super.dispose();
  }

  void _connect() {
    final url = _buildUrl();
    _backend?.dispose();
    _backend = LogStreamBackend(_terminal, url);
    _backend!.connect();
    setState(() {}); // Single rebuild to update ListenableBuilder's listenable
  }

  void _disconnect() {
    _backend?.dispose();
    _backend = null;
    setState(() {});
  }

  String _buildUrl() {
    final path = _buildPath();

    final devBase = CoreConfig.wsBaseUrl;
    if (devBase.isNotEmpty) {
      final cleanBase = devBase.endsWith('/')
          ? devBase.substring(0, devBase.length - 1)
          : devBase;
      return '$cleanBase$path';
    }

    if (kIsWeb) {
      final uri = Uri.base;
      final scheme = uri.scheme == 'https' ? 'wss' : 'ws';
      final portPart = (uri.hasPort && uri.port != 0) ? ':${uri.port}' : '';
      return '$scheme://${uri.host}$portPart$path';
    }

    return 'ws://127.0.0.1$path';
  }

  String _buildPath() {
    final tail = widget.tailLines;
    if (widget.appName != null) {
      return buildAppLogStreamPath(
        appId: widget.appName!,
        tail: tail,
        serviceName: widget.serviceName,
      );
    }
    final unit = Uri.encodeQueryComponent(widget.systemUnit!);
    return '/api/v1/system/logs/stream?unit=$unit&tail=$tail';
  }

  String _statusLabel() {
    final state = _backend?.state ?? WebSocketConnectionState.disconnected;
    switch (state) {
      case WebSocketConnectionState.disconnected:
        return 'Disconnected';
      case WebSocketConnectionState.connecting:
        return 'Connecting';
      case WebSocketConnectionState.connected:
        return 'Connected';
      case WebSocketConnectionState.error:
        return 'Error';
    }
  }

  Color _statusColor() {
    final state = _backend?.state ?? WebSocketConnectionState.disconnected;
    switch (state) {
      case WebSocketConnectionState.connected:
        return PiccoloTheme.success;
      case WebSocketConnectionState.connecting:
        return PiccoloTheme.cobalt600;
      case WebSocketConnectionState.error:
        return PiccoloTheme.critical;
      case WebSocketConnectionState.disconnected:
        return PiccoloTheme.inkMuted;
    }
  }

  Widget _buildStatusBar() {
    final status = _statusLabel();
    final err = _backend?.lastError;

    return Padding(
      padding: const EdgeInsets.symmetric(
        horizontal: 12,
        vertical: 8,
      ),
      child: Row(
        children: [
          Container(
            width: 10,
            height: 10,
            decoration: BoxDecoration(
              color: _statusColor(),
              shape: BoxShape.circle,
            ),
          ),
          const SizedBox(width: 8),
          Expanded(
            child: Text(
              err == null ? status : '$status • $err',
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
              style: PiccoloTheme.textTheme.labelMedium?.copyWith(
                color: PiccoloTheme.inkMuted,
              ),
            ),
          ),
          TextButton(
            onPressed: _backend == null ? null : () => _backend!.clear(),
            child: const Text('Clear'),
          ),
          const SizedBox(width: 8),
          if (_backend == null ||
              _backend!.state == WebSocketConnectionState.disconnected ||
              _backend!.state == WebSocketConnectionState.error)
            FilledButton(
              onPressed: _connect,
              child: const Text('Connect'),
            )
          else
            OutlinedButton(
              onPressed: _disconnect,
              child: const Text('Disconnect'),
            ),
        ],
      ),
    );
  }

  /// Copy selected text to clipboard.
  Future<void> _copySelection() async {
    final selection = _controller.selection;
    if (selection == null) return;
    final text = _terminal.buffer.getText(selection);
    try {
      await clipboard_utils.copyText(text);
    } on Object catch (_) {
      // Clipboard unavailable - silently fail for log viewer
    }
  }

  /// Handle Ctrl+C shortcut: copy if selection exists.
  /// Log viewer is read-only so no SIGINT handling needed.
  void _handleCopyShortcut() {
    if (_controller.selection != null) {
      unawaited(_copySelection());
    }
  }

  /// Show context menu with copy option.
  Future<void> _showContextMenu(Offset position) async {
    final hasSelection = _controller.selection != null;
    final choice = await showMenu<String>(
      context: context,
      position: RelativeRect.fromLTRB(
        position.dx,
        position.dy,
        position.dx,
        position.dy,
      ),
      items: [
        PopupMenuItem<String>(
          value: 'copy',
          enabled: hasSelection,
          child: const Text('Copy'),
        ),
      ],
    );

    if (choice == 'copy') {
      await _copySelection();
    }
  }

  @override
  Widget build(BuildContext context) {
    // Terminal body with RepaintBoundary for paint isolation,
    // wrapped in RenderErrorBoundary for rendering error recovery.
    final terminalBody = RenderErrorBoundary(
      onError: (error) => ErrorReporter().report(
        type: 'render_error',
        message: 'Log viewer render error: $error',
        stack: error is Error ? error.stackTrace?.toString() : null,
      ),
      onRetry: _connect,
      fallbackBuilder: (error, retry) =>
          RenderErrorFallback(label: 'Log viewer', retry: retry),
      child: RepaintBoundary(
        child: ColoredBox(
          color: PiccoloTheme.terminalBg,
          child: Scrollbar(
            controller: _scrollController,
            thumbVisibility: true,
            child: CallbackShortcuts(
              bindings: {
                // Ctrl+Shift+C - explicit copy (Linux terminal convention)
                const SingleActivator(LogicalKeyboardKey.keyC, control: true, shift: true):
                    _handleCopyShortcut,
                // Ctrl+C to copy selected text (Linux/Windows)
                const SingleActivator(LogicalKeyboardKey.keyC, control: true):
                    _handleCopyShortcut,
                // Cmd+C on macOS
                const SingleActivator(LogicalKeyboardKey.keyC, meta: true):
                    _handleCopyShortcut,
              },
              child: TerminalView(
                _terminal,
                controller: _controller,
                scrollController: _scrollController,
                textStyle: const TerminalStyle(
                  fontFamily: 'JetBrainsMono',
                ),
                padding: const EdgeInsets.all(12),
                onSecondaryTapDown: (details, cell) =>
                    _showContextMenu(details.globalPosition),
              ),
            ),
          ),
        ),
      ),
    );

    return LayoutBuilder(
      builder: (context, constraints) {
        final terminalView = widget.height != null
            ? SizedBox(height: widget.height, child: terminalBody)
            : constraints.hasBoundedHeight
                ? Expanded(child: terminalBody)
                : SizedBox(height: 320, child: terminalBody);

        return Container(
          decoration: BoxDecoration(
            color: PiccoloTheme.porcelain,
            borderRadius: BorderRadius.circular(Radii.md),
            border: Border.all(color: PiccoloTheme.hairline),
          ),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              // Only status bar rebuilds on backend state changes
              if (_backend != null)
                ListenableBuilder(
                  listenable: _backend!,
                  builder: (context, _) => _buildStatusBar(),
                )
              else
                _buildStatusBar(),
              const Divider(height: 1),
              terminalView,
            ],
          ),
        );
      },
    );
  }
}

String buildAppLogStreamPath({
  required String appId,
  required int tail,
  String? serviceName,
}) {
  final id = Uri.encodeComponent(appId);

  final query = <String, String>{'tail': tail.toString(), 'timestamps': '1'};

  final svc = serviceName?.trim();
  if (svc != null && svc.isNotEmpty) {
    query['service'] = svc;
  }

  return Uri(
    path: '/api/v1/apps/$id/logs/stream',
    queryParameters: query,
  ).toString();
}
