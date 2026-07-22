import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:web_socket_channel/web_socket_channel.dart';

enum WebSocketConnectionState { disconnected, connecting, connected, error }

typedef ReconnectScheduledCallback = void Function(Duration delay);
typedef SessionEndCallback = void Function();
typedef WebSocketChannelFactory = WebSocketChannel Function(Uri uri);

class WebSocketConnection extends ChangeNotifier {
  WebSocketConnection(
    this.url, {
    this.autoReconnect = true,
    this.initialReconnectDelay = const Duration(seconds: 2),
    this.maxReconnectDelay = const Duration(seconds: 30),
    this.onReconnectScheduled,
    this.onSessionEnd,
    WebSocketChannelFactory? channelFactory,
  }) : _channelFactory = channelFactory ?? WebSocketChannel.connect,
       _reconnectDelay = initialReconnectDelay;
  final String url;
  final bool autoReconnect;
  final Duration initialReconnectDelay;
  final Duration maxReconnectDelay;
  final ReconnectScheduledCallback? onReconnectScheduled;
  final SessionEndCallback? onSessionEnd;
  final WebSocketChannelFactory _channelFactory;

  WebSocketChannel? _channel;
  StreamSubscription<dynamic>? _subscription;
  final StreamController<dynamic> _messagesController =
      StreamController<dynamic>.broadcast();

  Timer? _reconnectTimer;
  Duration _reconnectDelay;

  bool _isDisposed = false;
  bool _isConnecting = false;
  bool _manualDisconnect = false;
  int _connectionAttempt = 0;

  WebSocketConnectionState _state = WebSocketConnectionState.disconnected;
  WebSocketConnectionState get state => _state;

  String? _lastError;
  String? get lastError => _lastError;

  Stream<dynamic> get messages => _messagesController.stream;

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

    final attempt = ++_connectionAttempt;
    unawaited(_connect(attempt));
  }

  Future<void> _connect(int attempt) async {
    WebSocketChannel? channel;

    try {
      channel = _channelFactory(Uri.parse(url));
      _channel = channel;

      // `connect()` only constructs the channel. The upgrade can still be
      // rejected asynchronously (for example when a restarted daemon no
      // longer knows a terminal session), so do not report connected until
      // the WebSocket handshake has actually completed.
      await channel.ready;
      if (!_isCurrentAttempt(attempt, channel)) {
        unawaited(channel.sink.close());
        return;
      }

      _subscription = channel.stream.listen(
        (message) {
          if (_isCurrentAttempt(attempt, channel!) && !_isDisposed) {
            _messagesController.add(message);
          }
        },
        onDone: () {
          if (!_isCurrentAttempt(attempt, channel!)) return;
          // Check close code:
          // 1000 = normal closure (shell exited via Ctrl+D)
          // 4000 = detached by another client attaching to the same session
          final closeCode = channel.closeCode;
          final isNormalClosure = closeCode == 1000;
          final isDetached = closeCode == 4000;
          _handleDisconnect(
            isDetached ? 'Detached by another client' : 'Connection closed',
            attempt: attempt,
            channel: channel,
            shouldReconnect: !isNormalClosure && !isDetached,
          );
          if (isNormalClosure || isDetached) {
            onSessionEnd?.call();
          }
        },
        onError: (Object error) {
          _handleDisconnect(
            'Connection error: $error',
            attempt: attempt,
            channel: channel!,
          );
        },
      );
      _reconnectDelay = initialReconnectDelay;
      _lastError = null;
      _setState(WebSocketConnectionState.connected);
    } on Object catch (e) {
      if (channel != null && _isCurrentAttempt(attempt, channel)) {
        _handleDisconnect(
          'Failed to connect: $e',
          attempt: attempt,
          channel: channel,
        );
      } else if (channel == null) {
        _handleConstructionFailure('Failed to connect: $e', attempt);
      }
    } finally {
      if (attempt == _connectionAttempt) {
        _isConnecting = false;
      }
    }
  }

  void disconnect({bool clearError = false}) {
    if (_isDisposed) return;
    _disconnect(clearError: clearError);
  }

  void _disconnect({required bool clearError}) {
    _manualDisconnect = true;
    _connectionAttempt++;
    _isConnecting = false;

    _reconnectTimer?.cancel();
    _reconnectTimer = null;

    unawaited(_subscription?.cancel());
    _subscription = null;

    unawaited(_channel?.sink.close());
    _channel = null;

    if (clearError) {
      _lastError = null;
    }
    _setState(WebSocketConnectionState.disconnected);
  }

  void send(dynamic data) {
    _channel?.sink.add(data);
  }

  bool _isCurrentAttempt(int attempt, WebSocketChannel channel) {
    return !_isDisposed &&
        attempt == _connectionAttempt &&
        identical(channel, _channel);
  }

  void _handleConstructionFailure(String reason, int attempt) {
    if (_isDisposed || attempt != _connectionAttempt) return;

    _connectionAttempt++;
    _isConnecting = false;
    _lastError = reason;
    _setState(WebSocketConnectionState.error);

    if (_manualDisconnect || !autoReconnect) return;
    _scheduleReconnect();
  }

  void _handleDisconnect(
    String reason, {
    required int attempt,
    required WebSocketChannel channel,
    bool shouldReconnect = true,
  }) {
    if (!_isCurrentAttempt(attempt, channel)) return;

    // Invalidate this attempt before cancelling its stream. A late onDone or
    // ready completion must not tear down a newer connection.
    _connectionAttempt++;
    _isConnecting = false;

    unawaited(_subscription?.cancel());
    _subscription = null;

    unawaited(_channel?.sink.close());
    _channel = null;

    _lastError = reason;
    _setState(WebSocketConnectionState.error);

    if (_manualDisconnect || !autoReconnect || !shouldReconnect) return;
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
    _reconnectDelay = Duration(seconds: nextSeconds);
  }

  void _setState(WebSocketConnectionState state) {
    if (_state == state) return;
    _state = state;
    notifyListeners();
  }

  @override
  void dispose() {
    if (_isDisposed) return;
    _disconnect(clearError: true);
    _isDisposed = true;
    unawaited(_messagesController.close());
    super.dispose();
  }
}
