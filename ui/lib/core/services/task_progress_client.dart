import 'dart:async';
import 'dart:convert';

import 'package:flutter/foundation.dart';

import '../models/task_progress.dart';
import 'websocket_connection.dart';

class TaskProgressClient extends ChangeNotifier {
  final WebSocketConnection _connection;
  late final VoidCallback _connectionListener;

  StreamSubscription? _subscription;
  final StreamController<TaskProgressEvent> _eventsController =
      StreamController.broadcast();

  bool _isDisposed = false;
  bool _isComplete = false;

  Stream<TaskProgressEvent> get events => _eventsController.stream;

  WebSocketConnectionState get state => _connection.state;
  String? get lastError => _connection.lastError;

  TaskProgressClient(String url) : _connection = WebSocketConnection(url) {
    _connectionListener = () => notifyListeners();
    _connection.addListener(_connectionListener);
  }

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
    _subscription?.cancel();
    _subscription = null;
    _connection.disconnect(clearError: clearError);
  }

  void _handleMessage(dynamic message) {
    if (message is! String) return;
    try {
      final decoded = jsonDecode(message);
      if (decoded is! Map<String, dynamic>) return;

      final type = decoded['type'];
      if (type == 'keepalive') return;
      if (type != 'task_progress') return;

      final payload = decoded['payload'];
      if (payload is! Map<String, dynamic>) return;

      final evt = TaskProgressEvent.fromJson(payload);
      _eventsController.add(evt);

      if (evt.isComplete) {
        _isComplete = true;
        disconnect();
      }
    } catch (e) {
      debugPrint('Task progress decode error: $e');
    }
  }

  @override
  void dispose() {
    _isDisposed = true;
    _subscription?.cancel();
    _subscription = null;
    _connection.removeListener(_connectionListener);
    _connection.dispose();
    _eventsController.close();
    super.dispose();
  }
}
