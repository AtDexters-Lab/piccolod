import 'dart:async';
import 'dart:convert';

import 'package:flutter/foundation.dart';

import 'package:piccolo_os/core/config/core_config.dart';
import 'package:piccolo_os/core/models/app_status_event.dart';
import 'package:piccolo_os/core/models/listener_health.dart';
import 'package:piccolo_os/core/models/network_models.dart';
import 'package:piccolo_os/core/services/websocket_connection.dart';

/// Unified event stream client that multiplexes multiple event types
/// over a single WebSocket connection.
///
/// Supported topics:
/// - app_status: App status changes (installed, uninstalled, running, stopped, error, starting)
/// - listener_health: Listener health updates
/// - remote_config: Remote access configuration changes (admin only)
/// - certificate: Certificate status changes (admin only)
/// - network_peers: Network peer discovery updates (LAN-only, stripped by server for remote)
class EventStreamClient extends ChangeNotifier {

  /// Creates an EventStreamClient that subscribes to all event topics.
  EventStreamClient() : _connection = WebSocketConnection(_buildUrl()) {
    _connectionListener = _onConnectionStateChanged;
    _connection.addListener(_connectionListener);
  }
  final WebSocketConnection _connection;
  late final VoidCallback _connectionListener;

  StreamSubscription<dynamic>? _subscription;

  final StreamController<AppStatusEvent> _appStatusController =
      StreamController<AppStatusEvent>.broadcast();
  final StreamController<ListenerHealthEvent> _healthController =
      StreamController<ListenerHealthEvent>.broadcast();
  final StreamController<Map<String, dynamic>> _remoteConfigController =
      StreamController<Map<String, dynamic>>.broadcast();
  final StreamController<Map<String, dynamic>> _certificateController =
      StreamController<Map<String, dynamic>>.broadcast();
  final StreamController<NetworkPeersEvent> _networkPeersController =
      StreamController<NetworkPeersEvent>.broadcast();
  final StreamController<Map<String, dynamic>> _identityController =
      StreamController<Map<String, dynamic>>.broadcast();
  final StreamController<Map<String, dynamic>> _networkStatusController =
      StreamController<Map<String, dynamic>>.broadcast();

  /// Called when a reconnect attempt indicates an auth failure (e.g. session expired).
  /// Set by the shell to trigger the re-auth overlay for passive WebSocket-only failures.
  VoidCallback? onAuthFailure;

  bool _isDisposed = false;

  /// Last received network peers event, cached for late subscribers.
  /// Broadcast streams drop events when no one is listening; widgets that
  /// mount after the initial snapshot can read this to hydrate immediately.
  NetworkPeersEvent? _lastNetworkPeersEvent;
  NetworkPeersEvent? get lastNetworkPeersEvent => _lastNetworkPeersEvent;

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

  /// Stream of network peers events.
  Stream<NetworkPeersEvent> get networkPeersEvents =>
      _networkPeersController.stream;

  /// Stream of identity status events (payload is raw JSON).
  Stream<Map<String, dynamic>> get identityEvents =>
      _identityController.stream;

  Stream<Map<String, dynamic>> get networkStatusEvents =>
      _networkStatusController.stream;

  WebSocketConnectionState get state => _connection.state;

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
      unawaited(_subscription?.cancel());
      _subscription = null;
      _lastNetworkPeersEvent = null;
    }

    // Detect auth failures on reconnect (WebSocket upgrade rejected with 401).
    // Browser WebSocket error messages are not standardized; string matching is
    // the best heuristic available. False negatives are acceptable — the HTTP
    // 401 interceptor in ApiClient is the primary re-auth trigger.
    if (_connection.state == WebSocketConnectionState.error &&
        onAuthFailure != null) {
      final err = _connection.lastError ?? '';
      if (err.contains('401') || err.contains('Unauthorized')) {
        // Stop reconnect attempts — session is expired, not a transient failure.
        _connection.disconnect();
        onAuthFailure!();
      }
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
      final payload = decoded['payload'];
      if (payload is! Map<String, dynamic>) return;

      if (_isDisposed) return;

      switch (type) {
        case 'app_status':
          _appStatusController.add(AppStatusEvent.fromJson(payload));
        case 'listener_health':
          _healthController.add(ListenerHealthEvent.fromJson(payload));
        case 'remote_config':
          _remoteConfigController.add(payload);
        case 'certificate':
          _certificateController.add(payload);
        case 'network_peers':
          final event = NetworkPeersEvent.fromJson(payload);
          _lastNetworkPeersEvent = event;
          _networkPeersController.add(event);
        case 'identity':
          _identityController.add(payload);
        case 'network_status':
          _networkStatusController.add(payload);
      }
    } on Object catch (e) {
      debugPrint('Event stream decode error: $e');
    }
  }

  @override
  void dispose() {
    _isDisposed = true;
    _lastNetworkPeersEvent = null;
    unawaited(_subscription?.cancel());
    _subscription = null;
    _connection
      ..removeListener(_connectionListener)
      ..dispose();
    unawaited(_appStatusController.close());
    unawaited(_healthController.close());
    unawaited(_remoteConfigController.close());
    unawaited(_certificateController.close());
    unawaited(_networkPeersController.close());
    unawaited(_identityController.close());
    unawaited(_networkStatusController.close());
    super.dispose();
  }
}
