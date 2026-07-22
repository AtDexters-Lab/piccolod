import 'dart:async';
import 'dart:convert';

import 'package:flutter/foundation.dart';
import 'package:piccolo_os/core/services/websocket_connection.dart';
import 'package:xterm/xterm.dart';

class PiccoloTerminalBackend {

  PiccoloTerminalBackend(
    Terminal terminal,
    String url, {
    void Function()? onSessionEnd,
    void Function()? onSessionLost,
  }) : this.withConnection(
         terminal,
         url,
         WebSocketConnection(
           url,
           onReconnectScheduled: (delay) {
             terminal.write(
               '\r\n\x1b[33mReconnecting in ${delay.inSeconds}s...\x1b[0m\r\n',
             );
           },
           onSessionEnd: onSessionEnd,
         ),
         onSessionEnd: onSessionEnd,
         onSessionLost: onSessionLost,
       );

  @visibleForTesting
  PiccoloTerminalBackend.withConnection(
    this.terminal,
    this.url,
    WebSocketConnection connection, {
    this.onSessionEnd,
    this.onSessionLost,
  }) {
    _connection = connection;
    _connectionListener = _handleConnectionUpdate;
    _connection.addListener(_connectionListener);
  }
  final Terminal terminal;
  final String url;
  final void Function()? onSessionEnd;
  final void Function()? onSessionLost;

  late final WebSocketConnection _connection;
  late final void Function() _connectionListener;

  StreamSubscription<dynamic>? _subscription;
  Timer? _resizeDebounce;

  WebSocketConnectionState _lastState = WebSocketConnectionState.disconnected;
  String? _lastErrorShown;

  /// Tracks consecutive connection failures to detect dead sessions.
  int _consecutiveFailures = 0;
  static const _maxFailuresBeforeSessionLost = 3;

  void init() {
    _subscription = _connection.messages.listen(_handleMessage);
    _connection.connect();

    terminal.onOutput = _sendInput;

    terminal.onResize = (cols, rows, pixelWidth, pixelHeight) {
      if (_resizeDebounce?.isActive ?? false) _resizeDebounce!.cancel();
      _resizeDebounce = Timer(const Duration(milliseconds: 50), () {
        _sendResize(cols, rows);
      });
    };
  }

  void _handleConnectionUpdate() {
    final state = _connection.state;
    if (state == _lastState) return;
    _lastState = state;

    if (state == WebSocketConnectionState.connected) {
      _consecutiveFailures = 0;
      _lastErrorShown = null;
      _sendResize(terminal.viewWidth, terminal.viewHeight);
      return;
    }

    if (state == WebSocketConnectionState.error) {
      _consecutiveFailures++;
      if (_consecutiveFailures >= _maxFailuresBeforeSessionLost &&
          onSessionLost != null) {
        // Session is likely dead — trigger fresh session creation
        onSessionLost!();
        return;
      }
      final err = _connection.lastError;
      if (err != null && err.isNotEmpty && err != _lastErrorShown) {
        terminal.write('\r\n\x1b[31m$err\x1b[0m\r\n');
        _lastErrorShown = err;
      }
    }
  }

  void _handleMessage(dynamic message) {
    if (message is! String) return;

    try {
      final payload = jsonDecode(message) as Map<String, dynamic>;
      final type = payload['type'];

      if (type == 'stdout') {
        final encoded = payload['data'] as String;
        final bytes = base64.decode(encoded);
        final text = utf8.decode(bytes);
        terminal.write(text);
      }
    } on Object catch (_) {
      // Ignore malformed messages to avoid spamming the terminal.
    }
  }

  void _sendInput(String data) {
    if (_connection.state != WebSocketConnectionState.connected) return;

    final encoded = base64.encode(utf8.encode(data));
    final payload = jsonEncode({
      'type': 'stdin',
      'data': encoded,
    });

    _connection.send(payload);
  }

  void _sendResize(int cols, int rows) {
    if (_connection.state != WebSocketConnectionState.connected) return;

    final payload = jsonEncode({
      'type': 'resize',
      'cols': cols,
      'rows': rows,
    });

    _connection.send(payload);
  }

  void dispose() {
    _resizeDebounce?.cancel();
    unawaited(_subscription?.cancel());
    _subscription = null;

    _connection
      ..removeListener(_connectionListener)
      ..dispose();
  }
}
