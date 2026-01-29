import 'dart:async';
import 'dart:convert';

import 'package:flutter/foundation.dart';

import '../config/core_config.dart';
import '../models/listener_health.dart';
import 'websocket_connection.dart';

class HealthStreamClient extends ChangeNotifier {
  final WebSocketConnection _connection;
  late final VoidCallback _connectionListener;

  StreamSubscription? _subscription;
  final StreamController<ListenerHealthEvent> _eventsController =
      StreamController.broadcast();

  bool _isDisposed = false;

  Stream<ListenerHealthEvent> get events => _eventsController.stream;

  WebSocketConnectionState get state => _connection.state;

  HealthStreamClient({String? app})
      : _connection = WebSocketConnection(_buildUrl(app)) {
    _connectionListener = _onConnectionStateChanged;
    _connection.addListener(_connectionListener);
  }

  static String _buildUrl(String? app) {
    final path = StringBuffer('/api/v1/events/health/stream');
    var sep = '?';
    if (app != null && app.isNotEmpty) {
      path.write('${sep}app=');
      path.write(Uri.encodeComponent(app));
      sep = '&';
    }
    final pathStr = path.toString();

    final devBase = CoreConfig.wsBaseUrl;
    if (devBase.isNotEmpty) {
      final cleanBase = devBase.endsWith('/')
          ? devBase.substring(0, devBase.length - 1)
          : devBase;
      return '$cleanBase$pathStr';
    }

    if (kIsWeb) {
      final uri = Uri.base;
      final scheme = uri.scheme == 'https' ? 'wss' : 'ws';
      final portPart = (uri.hasPort && uri.port != 0) ? ':${uri.port}' : '';
      return '$scheme://${uri.host}$portPart$pathStr';
    }

    return 'ws://127.0.0.1$pathStr';
  }

  void _onConnectionStateChanged() {
    // When the connection disconnects or errors, null out the subscription
    // so that connect() can re-establish it on reconnect.
    if (_connection.state == WebSocketConnectionState.disconnected ||
        _connection.state == WebSocketConnectionState.error) {
      _subscription?.cancel();
      _subscription = null;
    }
    // When reconnected, re-subscribe to messages
    if (_connection.state == WebSocketConnectionState.connected &&
        _subscription == null &&
        !_isDisposed) {
      _subscription = _connection.messages.listen(_handleMessage);
    }
    notifyListeners();
  }

  void connect() {
    if (_isDisposed) return;
    _subscription ??= _connection.messages.listen(_handleMessage);
    _connection.connect();
  }

  void disconnect({bool clearError = false}) {
    if (_isDisposed) return;
    _subscription?.cancel();
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
      if (type == 'keepalive') return;
      if (type != 'listener_health') return;

      final payload = decoded['payload'];
      if (payload is! Map<String, dynamic>) return;

      if (!_isDisposed) {
        _eventsController.add(ListenerHealthEvent.fromJson(payload));
      }
    } catch (e) {
      debugPrint('Health stream decode error: $e');
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
