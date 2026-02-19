import 'dart:async';
import 'dart:convert';

import 'package:flutter/foundation.dart';

import 'package:piccolo_os/core/models/task_progress.dart';
import 'package:piccolo_os/core/services/websocket_connection.dart';

class TaskProgressClient extends ChangeNotifier {

  TaskProgressClient(String url) : _connection = WebSocketConnection(url) {
    _connectionListener = notifyListeners;
    _connection.addListener(_connectionListener);
  }
  final WebSocketConnection _connection;
  late final VoidCallback _connectionListener;

  StreamSubscription<dynamic>? _subscription;
  final StreamController<TaskProgressEvent> _eventsController =
      StreamController<TaskProgressEvent>.broadcast();

  bool _isDisposed = false;
  bool _isComplete = false;

  Stream<TaskProgressEvent> get events => _eventsController.stream;

  WebSocketConnectionState get state => _connection.state;
  String? get lastError => _connection.lastError;

  void connect() {
    if (_isDisposed || _isComplete) return;
    if (_subscription != null) {
      return;
    }
    _subscription = _connection.messages.listen(_handleMessage);
    _connection.connect();
  }

  void disconnect({bool clearError = false}) {
    if (_isDisposed) return;
    unawaited(_subscription?.cancel());
    _subscription = null;
    _connection.disconnect(clearError: clearError);
  }

  void _handleMessage(dynamic message) {
    if (_isDisposed) return;
    if (message is! String) return;
    try {
      final decoded = jsonDecode(message);
      if (decoded is! Map<String, dynamic>) return;

      final type = decoded['type'];
      if (type != 'task_progress') return;

      final payload = decoded['payload'];
      if (payload is! Map<String, dynamic>) return;

      final evt = TaskProgressEvent.fromJson(payload);
      if (!_isDisposed) {
        _eventsController.add(evt);
      }

      if (evt.isComplete) {
        _isComplete = true;
        disconnect();
      }
    } on Object catch (e) {
      debugPrint('Task progress decode error: $e');
    }
  }

  @override
  void dispose() {
    _isDisposed = true;
    unawaited(_subscription?.cancel());
    _subscription = null;
    _connection
      ..removeListener(_connectionListener)
      ..dispose();
    unawaited(_eventsController.close());
    super.dispose();
  }
}
