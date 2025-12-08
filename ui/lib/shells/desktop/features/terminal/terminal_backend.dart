import 'dart:async';
import 'dart:convert';

import 'package:web_socket_channel/web_socket_channel.dart';
import 'package:xterm/xterm.dart';

class PiccoloTerminalBackend {

  final Terminal terminal;

  final String url;

  WebSocketChannel? _channel;

  StreamSubscription? _subscription;

  Timer? _resizeDebounce;
  Timer? _reconnectTimer;

  // Exponential backoff for reconnects
  Duration _reconnectDelay = const Duration(seconds: 2);

  bool _isDisposed = false;
  bool _isConnecting = false;



  PiccoloTerminalBackend(this.terminal, this.url);



  void init() {

    _connect();



    // Handle user input (keystrokes)

    terminal.onOutput = (data) {

      _sendInput(data);

    };



    // Handle resize with debounce (cols, rows, pixelWidth, pixelHeight)

    terminal.onResize = (cols, rows, pixelWidth, pixelHeight) {

      if (_resizeDebounce?.isActive ?? false) _resizeDebounce!.cancel();

      _resizeDebounce = Timer(const Duration(milliseconds: 50), () {

        _sendResize(cols, rows);

      });

    };

  }



  void _connect() {

    if (_isDisposed || _isConnecting) return;

    _isConnecting = true;

    try {

      // In a real app, we might need to handle auth headers if cookies aren't enough,

      // but for browser-based, cookies are sent automatically.

      _channel = WebSocketChannel.connect(Uri.parse(url));



      _subscription = _channel!.stream.listen(
        (message) {
          _handleMessage(message);
        },
        onDone: () => _handleDisconnect('Connection closed'),
        onError: (error) => _handleDisconnect('Connection error: $error'),
      );

      

      // Initial resize to sync state

      // Give it a tiny delay to ensure socket is open

      Future.delayed(const Duration(milliseconds: 100), () {

        _sendResize(terminal.viewWidth, terminal.viewHeight);

      });

      // Reset backoff on successful connect
      _reconnectDelay = const Duration(seconds: 2);

      _isConnecting = false;
      

    } catch (e) {

      _isConnecting = false;

      _handleDisconnect('Failed to connect: $e');

    }

  }



  void _handleDisconnect(String reason) {

    if (_isDisposed) return;



    _subscription?.cancel();

    _subscription = null;



    _channel?.sink.close();

    _channel = null;



    terminal.write('\r\n\x1b[31m$reason\x1b[0m\r\n');

    _scheduleReconnect();

  }



  void _scheduleReconnect() {

    if (_reconnectTimer?.isActive ?? false) return;



    final delay = _reconnectDelay;

    terminal.write('\r\n\x1b[33mReconnecting in ${delay.inSeconds}s...\x1b[0m\r\n');



    _reconnectTimer = Timer(delay, () {

      _reconnectTimer = null;

      _connect();

    });



    // Exponential backoff with cap

    final nextSeconds = (_reconnectDelay.inSeconds * 2).clamp(2, 30);

    _reconnectDelay = Duration(seconds: nextSeconds);

  }



  void _handleMessage(dynamic message) {

    if (message is! String) return; // We expect JSON strings



    try {

      final Map<String, dynamic> payload = jsonDecode(message);

      final type = payload['type'];



      if (type == 'stdout') {

        final encoded = payload['data'] as String;

        // Robust Base64 decode

        final bytes = base64.decode(encoded);

        final text = utf8.decode(bytes);

        terminal.write(text);

      }

    } catch (e) {

      // Silently ignore malformed messages to avoid spamming the term

      // print('Terminal protocol error: $e');

    }

  }



  void _sendInput(String data) {

    if (_channel == null) return;

    

    final encoded = base64.encode(utf8.encode(data));

    final payload = jsonEncode({

      'type': 'stdin',

      'data': encoded,

    });

    

    _channel!.sink.add(payload);

  }



  void _sendResize(int cols, int rows) {

    if (_channel == null) return;

    

    // print('Terminal: Resizing to ${cols}x${rows}');



    final payload = jsonEncode({

      'type': 'resize',

      'cols': cols,

      'rows': rows,

    });



    _channel!.sink.add(payload);

  }



  void dispose() {

    _resizeDebounce?.cancel();

    _reconnectTimer?.cancel();

    _isDisposed = true;

    _subscription?.cancel();

    _channel?.sink.close();

  }

}
