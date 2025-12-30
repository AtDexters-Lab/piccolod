import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:web_socket_channel/web_socket_channel.dart';

enum WebSocketConnectionState { disconnected, connecting, connected, error }

typedef ReconnectScheduledCallback = void Function(Duration delay);

class WebSocketConnection extends ChangeNotifier {
  final String url;
  final bool autoReconnect;
  final Duration initialReconnectDelay;
  final Duration maxReconnectDelay;
  final ReconnectScheduledCallback? onReconnectScheduled;

  WebSocketChannel? _channel;
  StreamSubscription? _subscription;
  final StreamController<dynamic> _messagesController =
      StreamController<dynamic>.broadcast();

  Timer? _reconnectTimer;
  Duration _reconnectDelay;

  bool _isDisposed = false;
  bool _isConnecting = false;
  bool _manualDisconnect = false;

  WebSocketConnectionState _state = WebSocketConnectionState.disconnected;
  WebSocketConnectionState get state => _state;

  String? _lastError;
  String? get lastError => _lastError;

  Stream<dynamic> get messages => _messagesController.stream;

  WebSocketConnection(
    this.url, {
    this.autoReconnect = true,
    this.initialReconnectDelay = const Duration(seconds: 2),
    this.maxReconnectDelay = const Duration(seconds: 30),
    this.onReconnectScheduled,
  }) : _reconnectDelay = initialReconnectDelay;

  void connect() {
    if (_isDisposed || _isConnecting) return;

    _manualDisconnect = false;
    _reconnectTimer?.cancel();
    _reconnectTimer = null;

    if (_state == WebSocketConnectionState.connected ||
        _state == WebSocketConnectionState.connecting) {
      return;
    }

    _isConnecting = true;
    _setState(WebSocketConnectionState.connecting);

    try {
      _channel = WebSocketChannel.connect(Uri.parse(url));
      _subscription = _channel!.stream.listen(
        (message) => _messagesController.add(message),
        onDone: () => _handleDisconnect('Connection closed'),
        onError: (error) => _handleDisconnect('Connection error: $error'),
      );
      _reconnectDelay = initialReconnectDelay;
      _lastError = null;
      _setState(WebSocketConnectionState.connected);
    } catch (e) {
      _handleDisconnect('Failed to connect: $e');
    } finally {
      _isConnecting = false;
    }
  }

  void disconnect({bool clearError = false}) {
    if (_isDisposed) return;
    _manualDisconnect = true;

    _reconnectTimer?.cancel();
    _reconnectTimer = null;

    _subscription?.cancel();
    _subscription = null;

    _channel?.sink.close();
    _channel = null;

    if (clearError) {
      _lastError = null;
    }
    _setState(WebSocketConnectionState.disconnected);
  }

  void send(dynamic data) {
    _channel?.sink.add(data);
  }

  void _handleDisconnect(String reason) {
    if (_isDisposed) return;

    _subscription?.cancel();
    _subscription = null;

    _channel?.sink.close();
    _channel = null;

    _lastError = reason;
    _setState(WebSocketConnectionState.error);

    if (_manualDisconnect || !autoReconnect) return;
    _scheduleReconnect();
  }

  void _scheduleReconnect() {
    if (_isDisposed) return;
    if (_reconnectTimer?.isActive ?? false) return;

    final delay = _reconnectDelay;
    onReconnectScheduled?.call(delay);

    _reconnectTimer = Timer(delay, () {
      _reconnectTimer = null;
      connect();
    });

    final nextSeconds = (_reconnectDelay.inSeconds * 2).clamp(
      initialReconnectDelay.inSeconds,
      maxReconnectDelay.inSeconds,
    );
    _reconnectDelay = Duration(seconds: nextSeconds.toInt());
  }

  void _setState(WebSocketConnectionState state) {
    if (_state == state) return;
    _state = state;
    notifyListeners();
  }

  @override
  void dispose() {
    _isDisposed = true;
    disconnect(clearError: true);
    _messagesController.close();
    super.dispose();
  }
}
