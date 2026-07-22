import 'dart:async';
import 'dart:convert';

import 'package:flutter/foundation.dart';

import 'package:piccolo_os/core/config/core_config.dart';
import 'package:piccolo_os/core/models/app_status_event.dart';
import 'package:piccolo_os/core/models/listener_health.dart';
import 'package:piccolo_os/core/models/network_models.dart';
import 'package:piccolo_os/core/models/resource_pressure.dart';
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
/// - network_status: Typed active-uplink, connectivity, interface, AP, and WiFi status
class EventStreamClient extends ChangeNotifier {

  /// Creates an EventStreamClient that subscribes to all event topics.
  EventStreamClient() : this.withConnection(WebSocketConnection(_buildUrl()));

  /// Creates a client over an injected transport.
  ///
  /// This keeps connection/event integration tests deterministic without
  /// changing the production transport path.
  @visibleForTesting
  EventStreamClient.withConnection(this._connection) {
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
  final StreamController<ResourcePressure> _resourcePressureController =
      StreamController<ResourcePressure>.broadcast();
  final StreamController<String> _snapshotCompleteController =
      StreamController<String>.broadcast();

  /// Called when a reconnect attempt indicates an auth failure (e.g. session expired).
  /// Set by the shell to trigger the re-auth overlay for passive WebSocket-only failures.
  VoidCallback? onAuthFailure;

  /// Called after several failed reconnects following a previously healthy
  /// connection. The shell uses the public boot endpoint to distinguish a
  /// restarting backend from a backend that restarted into a locked state.
  Future<void> Function()? onRecoveryProbeRequired;

  bool _isDisposed = false;
  bool _hasConnected = false;
  int _consecutiveReconnectFailures = 0;
  bool _recoveryProbeInFlight = false;
  static const _failuresBeforeRecoveryProbe = 3;

  /// Last received network peers event, cached for late subscribers.
  /// Broadcast streams drop events when no one is listening; widgets that
  /// mount after the initial snapshot can read this to hydrate immediately.
  NetworkPeersEvent? _lastNetworkPeersEvent;
  NetworkPeersEvent? get lastNetworkPeersEvent => _lastNetworkPeersEvent;
  ResourcePressure? _lastTaskPressure;
  ResourcePressure? get lastTaskPressure => _lastTaskPressure;
  ResourcePressure? _lastGlobalRecoverySuppression;
  ResourcePressure? get lastGlobalRecoverySuppression =>
      _lastGlobalRecoverySuppression;
  final Map<String, ResourcePressure> _lastRuntimePressure = {};
  Iterable<ResourcePressure> get lastRuntimePressure =>
      _lastRuntimePressure.values;
  final Map<String, ListenerHealthEvent> _lastListenerHealthEvents = {};
  Iterable<ListenerHealthEvent> get lastListenerHealthEvents =>
      _lastListenerHealthEvents.values;
  Map<String, dynamic>? _lastRemoteConfig;
  Map<String, dynamic>? get lastRemoteConfig => _lastRemoteConfig;
  final Set<String> _completedSnapshots = {};
  Set<String> get completedSnapshots => Set.unmodifiable(_completedSnapshots);

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

  Stream<ResourcePressure> get resourcePressureEvents =>
      _resourcePressureController.stream;

  Stream<String> get snapshotCompleteEvents =>
      _snapshotCompleteController.stream;

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
    if (_connection.state == WebSocketConnectionState.connected) {
      _hasConnected = true;
      _consecutiveReconnectFailures = 0;
    }

    // When disconnected/error, cancel subscription for re-subscribe on reconnect
    if (_connection.state == WebSocketConnectionState.disconnected ||
        _connection.state == WebSocketConnectionState.error) {
      unawaited(_subscription?.cancel());
      _subscription = null;
      _lastNetworkPeersEvent = null;
      _lastTaskPressure = null;
      _lastGlobalRecoverySuppression = null;
      _lastRuntimePressure.clear();
      _lastListenerHealthEvents.clear();
      _lastRemoteConfig = null;
      _completedSnapshots.clear();
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
        notifyListeners();
        return;
      }
    }

    if (_connection.state == WebSocketConnectionState.error && _hasConnected) {
      _consecutiveReconnectFailures++;
      if (_consecutiveReconnectFailures >= _failuresBeforeRecoveryProbe) {
        unawaited(_requestRecoveryProbe());
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

  Future<void> _requestRecoveryProbe() async {
    final callback = onRecoveryProbeRequired;
    if (_isDisposed || _recoveryProbeInFlight || callback == null) return;

    _recoveryProbeInFlight = true;
    try {
      await callback();
    } on Object catch (e) {
      // The event stream owns reconnecting; a failed probe must not terminate
      // that loop or force a pre-desktop screen without a confirmed boot state.
      debugPrint('Event stream recovery probe failed: $e');
    } finally {
      _recoveryProbeInFlight = false;
    }
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
          final event = ListenerHealthEvent.fromJson(payload);
          _lastListenerHealthEvents['${event.app}:${event.listener}'] = event;
          _healthController.add(event);
        case 'remote_config':
          _lastRemoteConfig = payload;
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
        case 'resource_pressure':
          final event = ResourcePressure.fromJson(payload);
          if (event.isTaskPressure) _lastTaskPressure = event;
          if (event.isRuntimeUnknown) {
            _lastRuntimePressure[event.appInstanceId] = event;
          } else if (event.isRecoverySuppressed) {
            if (event.appInstanceId.isEmpty) {
              _lastGlobalRecoverySuppression = event;
            } else {
              _lastRuntimePressure[event.appInstanceId] = event;
            }
          } else if (event.isRuntimeObservation &&
              event.severity == 'ok' &&
              event.appInstanceId.isEmpty) {
            _lastGlobalRecoverySuppression = null;
          } else if (event.isRuntimeRecovered) {
            _lastRuntimePressure.remove(event.appInstanceId);
          }
          _resourcePressureController.add(event);
        case 'snapshot_complete':
          final topic = payload['topic'];
          if (topic is String) {
            _completedSnapshots.add(topic);
            _snapshotCompleteController.add(topic);
          }
      }
    } on Object catch (e) {
      debugPrint('Event stream decode error: $e');
    }
  }

  @override
  void dispose() {
    _isDisposed = true;
    onAuthFailure = null;
    onRecoveryProbeRequired = null;
    _lastNetworkPeersEvent = null;
    _lastTaskPressure = null;
    _lastGlobalRecoverySuppression = null;
    _lastRuntimePressure.clear();
    _lastListenerHealthEvents.clear();
    _lastRemoteConfig = null;
    _completedSnapshots.clear();
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
    unawaited(_resourcePressureController.close());
    unawaited(_snapshotCompleteController.close());
    super.dispose();
  }
}
