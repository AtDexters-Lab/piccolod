import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:xterm/xterm.dart';

import '../../core/config/core_config.dart';
import '../../core/services/log_stream_backend.dart';
import '../../core/services/websocket_connection.dart';
import '../../theme/piccolo_theme.dart';

class LogStreamViewer extends StatefulWidget {
  final String? appName;
  final String? systemUnit;
  final int tailLines;
  final double? height;
  final bool autoConnect;

  const LogStreamViewer({
    super.key,
    this.appName,
    this.systemUnit,
    this.tailLines = 200,
    this.height,
    this.autoConnect = true,
  }) : assert(
          (appName == null) != (systemUnit == null),
          'Provide exactly one of appName or systemUnit',
        );

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
    _terminal = Terminal(maxLines: 50000);
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
    _backend = LogStreamBackend(_terminal, url)..addListener(_onBackendUpdate);
    _backend!.connect();
  }

  void _disconnect() {
    _backend?.removeListener(_onBackendUpdate);
    _backend?.dispose();
    _backend = null;
    setState(() {});
  }

  void _onBackendUpdate() {
    if (!mounted) return;
    setState(() {});
  }

  String _buildUrl() {
    final path = _buildPath();

    final devBase = CoreConfig.wsBaseUrl;
    if (devBase.isNotEmpty) {
      final cleanBase =
          devBase.endsWith('/') ? devBase.substring(0, devBase.length - 1) : devBase;
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
      final id = Uri.encodeComponent(widget.appName!);
      return '/api/v1/apps/$id/logs/stream?tail=$tail&timestamps=1';
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

  @override
  Widget build(BuildContext context) {
    final status = _statusLabel();
    final err = _backend?.lastError;

    final terminalBody = Container(
      color: const Color(0xFF1E1E1E),
      child: Scrollbar(
        controller: _scrollController,
        thumbVisibility: true,
        child: TerminalView(
          _terminal,
          controller: _controller,
          scrollController: _scrollController,
          textStyle: const TerminalStyle(
            fontFamily: 'JetBrainsMono',
            height: 1.2,
          ),
          padding: const EdgeInsets.all(12.0),
          autofocus: false,
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
            color: Colors.white,
            borderRadius: BorderRadius.circular(12),
            border: Border.all(color: PiccoloTheme.mist),
          ),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              Padding(
                padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 8),
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
              ),
              const Divider(height: 1),
              terminalView,
            ],
          ),
        );
      },
    );
  }
}
