import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/core/services/event_stream_client.dart';
import 'package:piccolo_os/core/services/websocket_connection.dart';
import 'package:piccolo_os/shells/desktop/features/terminal/terminal_backend.dart';
import 'package:web_socket_channel/io.dart';
import 'package:xterm/xterm.dart';

void main() {
  group('WebSocketConnection handshake', () {
    test('does not report connected before the upgrade is ready', () async {
      final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
      final accepted = Completer<WebSocket>();
      final serverSubscription = server
          .transform(WebSocketTransformer())
          .listen(accepted.complete);
      addTearDown(() async {
        if (accepted.isCompleted) {
          await (await accepted.future).close();
        }
        await serverSubscription.cancel();
        await server.close(force: true);
      });

      final connection = WebSocketConnection(
        'ws://${server.address.address}:${server.port}',
        autoReconnect: false,
      );
      addTearDown(connection.dispose);

      connection.connect();
      expect(connection.state, WebSocketConnectionState.connecting);

      await accepted.future;
      await _waitForState(connection, WebSocketConnectionState.connected);
      expect(connection.lastError, isNull);
    });

    test('a rejected upgrade becomes a real connection failure', () async {
      final socket = Completer<WebSocket>();
      final connection = WebSocketConnection(
        'ws://terminal.test/session/missing',
        autoReconnect: false,
        channelFactory: (_) => IOWebSocketChannel(socket.future),
      );
      addTearDown(connection.dispose);

      connection.connect();
      expect(connection.state, WebSocketConnectionState.connecting);
      socket.completeError(StateError('upgrade rejected'));

      await _waitForState(connection, WebSocketConnectionState.error);
      expect(connection.lastError, contains('upgrade rejected'));
    });

    test('a synchronous construction failure remains retryable', () async {
      var attempts = 0;
      final connection = WebSocketConnection(
        'ws://terminal.test/session/missing',
        autoReconnect: false,
        channelFactory: (_) {
          attempts++;
          throw StateError('factory rejected');
        },
      );
      addTearDown(connection.dispose);

      connection.connect();
      await _waitForState(connection, WebSocketConnectionState.error);
      expect(connection.lastError, contains('factory rejected'));

      connection.connect();
      await Future<void>.delayed(Duration.zero);
      expect(attempts, 2);
      expect(connection.state, WebSocketConnectionState.error);
    });

    test(
      'a stale upgrade failure cannot overwrite a manual disconnect',
      () async {
        final socket = Completer<WebSocket>();
        final connection = WebSocketConnection(
          'ws://events.test',
          autoReconnect: false,
          channelFactory: (_) => IOWebSocketChannel(socket.future),
        );
        addTearDown(connection.dispose);

        connection
          ..connect()
          ..disconnect(clearError: true);
        socket.completeError(StateError('late failure'));
        await Future<void>.delayed(Duration.zero);
        await Future<void>.delayed(Duration.zero);

        expect(connection.state, WebSocketConnectionState.disconnected);
        expect(connection.lastError, isNull);
      },
    );
  });

  test(
    'event stream probes boot state only after bounded post-connect failures',
    () async {
      final connection = _ControllableConnection();
      final client = EventStreamClient.withConnection(connection);
      addTearDown(client.dispose);
      var probes = 0;
      client.onRecoveryProbeRequired = () async {
        probes++;
        throw StateError('backend still restarting');
      };

      _emitFailures(connection, 3);
      await Future<void>.delayed(Duration.zero);
      expect(probes, 0, reason: 'cold-start failures have no prior desktop');

      connection.setState(WebSocketConnectionState.connected);
      _emitFailures(connection, 2);
      await Future<void>.delayed(Duration.zero);
      expect(probes, 0);

      _emitFailures(connection, 1);
      await Future<void>.delayed(Duration.zero);
      await Future<void>.delayed(Duration.zero);
      expect(probes, 1);
      expect(connection.disconnectCalls, 0);
      expect(connection.state, WebSocketConnectionState.error);

      // A failed public boot probe leaves reconnect ownership with the stream
      // and allows a later failure to try the probe again.
      _emitFailures(connection, 1);
      await Future<void>.delayed(Duration.zero);
      await Future<void>.delayed(Duration.zero);
      expect(probes, 2);
      expect(connection.disconnectCalls, 0);
    },
  );

  test('global recovery normal event clears retained suppression', () async {
    final connection = _ControllableConnection();
    final client = EventStreamClient.withConnection(connection);
    addTearDown(client.dispose);

    connection
      ..setState(WebSocketConnectionState.connected)
      ..emit(
        type: 'resource_pressure',
        payload: const {
          'resource': 'runtime',
          'severity': 'warn',
          'reason_code': 'automatic_recovery_suppressed',
          'app_instance_id': '',
        },
      );
    await Future<void>.delayed(Duration.zero);
    expect(client.lastGlobalRecoverySuppression, isNotNull);

    connection.emit(
      type: 'resource_pressure',
      payload: const {
        'resource': 'runtime',
        'severity': 'ok',
        'reason_code': 'normal',
        'app_instance_id': '',
      },
    );
    await Future<void>.delayed(Duration.zero);
    expect(client.lastGlobalRecoverySuppression, isNull);
  });

  test(
    'missing terminal session reaches onSessionLost after real failures',
    () async {
      var attempts = 0;
      final connection = WebSocketConnection(
        'ws://terminal.test/session/missing',
        initialReconnectDelay: Duration.zero,
        maxReconnectDelay: Duration.zero,
        channelFactory: (_) {
          attempts++;
          return IOWebSocketChannel(
            Future<WebSocket>.error(StateError('upgrade rejected')),
          );
        },
      );
      final terminal = Terminal();
      var sessionLost = 0;
      late final PiccoloTerminalBackend backend;
      backend = PiccoloTerminalBackend.withConnection(
        terminal,
        'ws://terminal.test/session/missing',
        connection,
        onSessionLost: () {
          sessionLost++;
          scheduleMicrotask(backend.dispose);
        },
      )..init();
      for (var i = 0; i < 50 && sessionLost == 0; i++) {
        await Future<void>.delayed(const Duration(milliseconds: 10));
      }

      expect(sessionLost, 1);
      expect(attempts, 3);
    },
  );
}

Future<void> _waitForState(
  WebSocketConnection connection,
  WebSocketConnectionState expected,
) async {
  for (var i = 0; i < 50; i++) {
    if (connection.state == expected) return;
    await Future<void>.delayed(const Duration(milliseconds: 10));
  }
  fail('Connection did not reach $expected; current=${connection.state}');
}

void _emitFailures(_ControllableConnection connection, int count) {
  for (var i = 0; i < count; i++) {
    connection
      ..setState(WebSocketConnectionState.connecting)
      ..setState(
        WebSocketConnectionState.error,
        error: 'Connection error: upgrade rejected',
      );
  }
}

class _ControllableConnection extends WebSocketConnection {
  _ControllableConnection() : super('ws://restart-recovery.test');

  final StreamController<dynamic> _messages =
      StreamController<dynamic>.broadcast();
  WebSocketConnectionState _testState = WebSocketConnectionState.disconnected;
  String? _testError;
  int disconnectCalls = 0;

  @override
  WebSocketConnectionState get state => _testState;

  @override
  String? get lastError => _testError;

  @override
  Stream<dynamic> get messages => _messages.stream;

  @override
  void connect() {}

  @override
  void disconnect({bool clearError = false}) {
    disconnectCalls++;
    if (clearError) _testError = null;
  }

  @override
  void send(dynamic data) {
    jsonEncode(data);
  }

  void setState(WebSocketConnectionState state, {String? error}) {
    _testState = state;
    _testError = error;
    notifyListeners();
  }

  void emit({required String type, required Map<String, dynamic> payload}) {
    _messages.add(jsonEncode({'type': type, 'payload': payload}));
  }

  @override
  void dispose() {
    unawaited(_messages.close());
    super.dispose();
  }
}
