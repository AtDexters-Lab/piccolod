import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';

import '../../core/config/core_config.dart';
import '../../core/models/task_progress.dart';
import '../../core/services/task_progress_client.dart';
import '../../core/services/websocket_connection.dart';
import '../../theme/piccolo_theme.dart';

class TaskProgressPanel extends StatefulWidget {
  final String taskId;
  final String taskType;
  final VoidCallback? onComplete;

  /// Optional URL path override for the WebSocket progress stream.
  /// Defaults to `/api/v1/events/progress/stream`.
  final String? urlPath;

  const TaskProgressPanel({
    super.key,
    required this.taskId,
    required this.taskType,
    this.onComplete,
    this.urlPath,
  });

  @override
  State<TaskProgressPanel> createState() => _TaskProgressPanelState();
}

class _TaskProgressPanelState extends State<TaskProgressPanel> {
  TaskProgressClient? _client;
  StreamSubscription? _sub;

  final List<TaskProgressEvent> _history = [];
  TaskProgressEvent? _latest;

  List<_ProgressSubtask> _subtasksFromMetadata(Map<String, dynamic>? metadata) {
    if (metadata == null) return const [];
    final raw = metadata['subtasks'];
    if (raw is! List) return const [];

    final out = <_ProgressSubtask>[];
    for (final item in raw) {
      if (item is! Map) continue;
      final map = Map<String, dynamic>.from(item);
      final name = (map['name'] ?? map['service'] ?? '').toString().trim();
      if (name.isEmpty) continue;

      final message = (map['message'] ?? '').toString().trim();

      final rawProgress = map['progress'];
      var progress = -1;
      if (rawProgress is int) {
        progress = rawProgress;
      } else if (rawProgress != null) {
        progress = int.tryParse(rawProgress.toString()) ?? -1;
      }

      out.add(
        _ProgressSubtask(name: name, message: message, progress: progress),
      );
    }
    return out;
  }

  @override
  void initState() {
    super.initState();
    _connect();
  }

  @override
  void didUpdateWidget(TaskProgressPanel oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (oldWidget.taskId != widget.taskId) {
      _disconnect();
      _history.clear();
      _latest = null;
      _connect();
    }
  }

  @override
  void dispose() {
    _disconnect();
    super.dispose();
  }

  void _connect() {
    final url = _buildUrl(widget.taskId);
    _client = TaskProgressClient(url)..addListener(_onClientUpdate);
    _sub = _client!.events.listen((evt) {
      if (!mounted) return;
      setState(() {
        _latest = evt;
        if (_history.isNotEmpty && _history.last.phase == evt.phase) {
          _history[_history.length - 1] = evt;
        } else {
          _history.add(evt);
        }
      });
      if (evt.isComplete && (evt.error == null || evt.error!.isEmpty)) {
        widget.onComplete?.call();
      }
    });
    _client!.connect();
  }

  void _disconnect() {
    _sub?.cancel();
    _sub = null;
    _client?.removeListener(_onClientUpdate);
    _client?.dispose();
    _client = null;
  }

  void _onClientUpdate() {
    if (!mounted) return;
    setState(() {});
  }

  String _buildUrl(String taskId) {
    final encoded = Uri.encodeQueryComponent(taskId);
    final basePath = widget.urlPath ?? '/api/v1/events/progress/stream';
    final path = '$basePath?task_id=$encoded';

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

  String _statusText() {
    final state = _client?.state ?? WebSocketConnectionState.disconnected;
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
    final state = _client?.state ?? WebSocketConnectionState.disconnected;
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
    final latest = _latest;
    final title = latest?.message.isNotEmpty == true
        ? latest!.message
        : 'Working…';
    final progress = latest?.progress ?? -1;
    final isIndeterminate = progress < 0;
    final error = latest?.error;
    final subtasks = _subtasksFromMetadata(latest?.metadata);

    return Container(
      padding: const EdgeInsets.all(16),
      decoration: BoxDecoration(
        color: Colors.white,
        borderRadius: BorderRadius.circular(12),
        border: Border.all(color: PiccoloTheme.mist),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          Row(
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
                  '${widget.taskType} • ${_statusText()}',
                  style: PiccoloTheme.textTheme.labelMedium?.copyWith(
                    color: PiccoloTheme.inkMuted,
                  ),
                ),
              ),
            ],
          ),
          const SizedBox(height: 12),
          Text(
            title,
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              fontWeight: FontWeight.w600,
            ),
          ),
          const SizedBox(height: 12),
          if (error != null && error.isNotEmpty) ...[
            Container(
              padding: const EdgeInsets.all(12),
              decoration: BoxDecoration(
                color: PiccoloTheme.critical.withValues(alpha: 0.08),
                borderRadius: BorderRadius.circular(8),
                border: Border.all(
                  color: PiccoloTheme.critical.withValues(alpha: 0.2),
                ),
              ),
              child: Text(
                error,
                style: PiccoloTheme.textTheme.labelMedium?.copyWith(
                  color: PiccoloTheme.critical,
                ),
              ),
            ),
            const SizedBox(height: 12),
          ],
          LinearProgressIndicator(
            value: isIndeterminate ? null : (progress / 100.0),
            minHeight: 8,
            color: PiccoloTheme.cobalt600,
            backgroundColor: PiccoloTheme.mist,
          ),
          const SizedBox(height: 12),
          ExpansionTile(
            tilePadding: EdgeInsets.zero,
            childrenPadding: EdgeInsets.zero,
            title: Text(
              'Details',
              style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
                fontWeight: FontWeight.w600,
              ),
            ),
            children: [
              if (subtasks.isNotEmpty) ...[
                Padding(
                  padding: const EdgeInsets.only(bottom: 8),
                  child: Text(
                    'Containers',
                    style: PiccoloTheme.textTheme.labelMedium?.copyWith(
                      color: PiccoloTheme.inkMuted,
                    ),
                  ),
                ),
                for (final st in subtasks)
                  Padding(
                    padding: const EdgeInsets.only(bottom: 8),
                    child: Row(
                      crossAxisAlignment: CrossAxisAlignment.start,
                      children: [
                        SizedBox(
                          width: 120,
                          child: Text(
                            st.name,
                            style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                              color: PiccoloTheme.inkMuted,
                            ),
                          ),
                        ),
                        const SizedBox(width: 8),
                        Expanded(
                          child: Text(
                            st.message,
                            style: PiccoloTheme.textTheme.bodyMedium,
                          ),
                        ),
                        if (st.progress >= 0) ...[
                          const SizedBox(width: 8),
                          Text(
                            '${st.progress}%',
                            style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                              color: PiccoloTheme.inkMuted,
                            ),
                          ),
                        ],
                      ],
                    ),
                  ),
                const Divider(height: 24),
              ],
              for (final evt in _history)
                Padding(
                  padding: const EdgeInsets.only(bottom: 8),
                  child: Row(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      SizedBox(
                        width: 120,
                        child: Text(
                          evt.phase,
                          style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                            color: PiccoloTheme.inkMuted,
                          ),
                        ),
                      ),
                      const SizedBox(width: 8),
                      Expanded(
                        child: Text(
                          evt.message,
                          style: PiccoloTheme.textTheme.bodyMedium,
                        ),
                      ),
                    ],
                  ),
                ),
            ],
          ),
        ],
      ),
    );
  }
}

class _ProgressSubtask {
  final String name;
  final String message;
  final int progress;

  const _ProgressSubtask({
    required this.name,
    required this.message,
    required this.progress,
  });
}

