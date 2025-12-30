import 'dart:async';
import 'dart:convert';

import 'package:flutter/foundation.dart';
import 'package:xterm/xterm.dart';

import 'websocket_connection.dart';

class LogStreamBackend extends ChangeNotifier {
  final Terminal terminal;
  final WebSocketConnection _connection;
  late final VoidCallback _connectionListener;

  StreamSubscription? _subscription;

  WebSocketConnectionState get state => _connection.state;
  String? get lastError => _connection.lastError;

  LogStreamBackend(this.terminal, String url)
      : _connection = WebSocketConnection(
          url,
          onReconnectScheduled: (delay) {
            terminal.write(
              '\r\n\x1b[33mReconnecting in ${delay.inSeconds}s...\x1b[0m\r\n',
            );
          },
        ) {
    _connectionListener = () => notifyListeners();
    _connection.addListener(_connectionListener);
  }

  void connect() {
    if (_subscription != null) {
      return;
    }
    _subscription = _connection.messages.listen(_handleMessage);
    _connection.connect();
  }

  void disconnect({bool clearError = false}) {
    _subscription?.cancel();
    _subscription = null;
    _connection.disconnect(clearError: clearError);
  }

  void clear() {
    terminal.write('\x1b[2J\x1b[3J\x1b[H');
  }

  void _handleMessage(dynamic message) {
    if (message is! String) return;
    try {
      final payload = jsonDecode(message);
      if (payload is! Map<String, dynamic>) return;

      final type = payload['type'];
      if (type != 'stdout' && type != 'error') return;

      final encoded = payload['data'];
      if (encoded is! String || encoded.isEmpty) return;

      final bytes = base64.decode(encoded);
      final text = utf8.decode(bytes, allowMalformed: true);
      terminal.write(_normalizeNewlines(text));
    } catch (e) {
      debugPrint('Log stream decode error: $e');
    }
  }

  String _normalizeNewlines(String input) {
    if (input.isEmpty) return input;
    // xterm treats '\n' as line-feed (no carriage return). Convert to CRLF so
    // each line starts at column 0, matching typical terminal output.
    final normalized = input.replaceAll('\r\n', '\n').replaceAll('\r', '\n');
    return normalized.replaceAll('\n', '\r\n');
  }

  @override
  void dispose() {
    _subscription?.cancel();
    _subscription = null;
    _connection.removeListener(_connectionListener);
    _connection.dispose();
    super.dispose();
  }
}
