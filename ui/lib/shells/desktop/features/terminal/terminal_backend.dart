import 'dart:async';
import 'dart:convert';

import 'package:xterm/xterm.dart';

import '../../../../core/services/websocket_connection.dart';

class PiccoloTerminalBackend {
  final Terminal terminal;
  final String url;

  late final WebSocketConnection _connection;
  late final void Function() _connectionListener;

  StreamSubscription? _subscription;
  Timer? _resizeDebounce;

  WebSocketConnectionState _lastState = WebSocketConnectionState.disconnected;
  String? _lastErrorShown;

  PiccoloTerminalBackend(this.terminal, this.url) {
    _connection = WebSocketConnection(
      url,
      onReconnectScheduled: (delay) {
        terminal.write(
          '\r\n\x1b[33mReconnecting in ${delay.inSeconds}s...\x1b[0m\r\n',
        );
      },
    );
    _connectionListener = _handleConnectionUpdate;
    _connection.addListener(_connectionListener);
  }

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
      _lastErrorShown = null;
      _sendResize(terminal.viewWidth, terminal.viewHeight);
      return;
    }

    if (state == WebSocketConnectionState.error) {
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
      final Map<String, dynamic> payload = jsonDecode(message);
      final type = payload['type'];

      if (type == 'stdout') {
        final encoded = payload['data'] as String;
        final bytes = base64.decode(encoded);
        final text = utf8.decode(bytes);
        terminal.write(text);
      }
    } catch (_) {
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
    _subscription?.cancel();
    _subscription = null;

    _connection.removeListener(_connectionListener);
    _connection.dispose();
  }
}

