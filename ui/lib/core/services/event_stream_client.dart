import 'dart:async';
import 'dart:convert';

import 'package:flutter/foundation.dart';

import '../config/core_config.dart';
import '../models/app_status_event.dart';
import '../models/listener_health.dart';
import 'websocket_connection.dart';

/// Unified event stream client that multiplexes multiple event types
/// over a single WebSocket connection.
///
/// Supported topics:
/// - app_status: App status changes (installed, uninstalled, running, stopped, error, starting)
/// - listener_health: Listener health updates
/// - remote_config: Remote access configuration changes (admin only)
/// - certificate: Certificate status changes (admin only)
class EventStreamClient extends ChangeNotifier {
  final WebSocketConnection _connection;
  late final VoidCallback _connectionListener;

  StreamSubscription? _subscription;

  final StreamController<AppStatusEvent> _appStatusController =
      StreamController.broadcast();
  final StreamController<ListenerHealthEvent> _healthController =
      StreamController.broadcast();
  final StreamController<Map<String, dynamic>> _remoteConfigController =
      StreamController.broadcast();
  final StreamController<Map<String, dynamic>> _certificateController =
      StreamController.broadcast();

  bool _isDisposed = false;

  /// Stream of app status change events.
  Stream<AppStatusEvent> get appStatusEvents => _appStatusController.stream;

  /// Stream of listener health events.
  Stream<ListenerHealthEvent> get healthEvents => _healthController.stream;

  /// Stream of remote config events (payload is raw JSON).
  Stream<Map<String, dynamic>> get remoteConfigEvents =>
      _remoteConfigController.stream;

  /// Stream of certificate events (payload is raw JSON).
  Stream<Map<String, dynamic>> get certificateEvents =>
      _certificateController.stream;

  WebSocketConnectionState get state => _connection.state;

  /// Creates an EventStreamClient that subscribes to all event topics.
  EventStreamClient() : _connection = WebSocketConnection(_buildUrl()) {
    _connectionListener = _onConnectionStateChanged;
    _connection.addListener(_connectionListener);
  }

  static String _buildUrl() {
    // Subscribe to all topics
    const path = '/api/v1/events/stream';

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

  void _onConnectionStateChanged() {
    // When disconnected/error, cancel subscription for re-subscribe on reconnect
    if (_connection.state == WebSocketConnectionState.disconnected ||
        _connection.state == WebSocketConnectionState.error) {
      _subscription?.cancel();
      _subscription = null;
    }
    // Re-subscribe when reconnected
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

      final payload = decoded['payload'];
      if (payload is! Map<String, dynamic>) return;

      if (_isDisposed) return;

      switch (type) {
        case 'app_status':
          _appStatusController.add(AppStatusEvent.fromJson(payload));
          break;
        case 'listener_health':
          _healthController.add(ListenerHealthEvent.fromJson(payload));
          break;
        case 'remote_config':
          _remoteConfigController.add(payload);
          break;
        case 'certificate':
          _certificateController.add(payload);
          break;
      }
    } catch (e) {
      debugPrint('Event stream decode error: $e');
    }
  }

  @override
  void dispose() {
    _isDisposed = true;
    _subscription?.cancel();
    _subscription = null;
    _connection.removeListener(_connectionListener);
    _connection.dispose();
    _appStatusController.close();
    _healthController.close();
    _remoteConfigController.close();
    _certificateController.close();
    super.dispose();
  }
}
