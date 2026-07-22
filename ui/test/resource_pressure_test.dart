import 'dart:async';
import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/core/models/app_status_event.dart';
import 'package:piccolo_os/core/models/resource_pressure.dart';
import 'package:piccolo_os/core/services/event_stream_client.dart';
import 'package:piccolo_os/core/services/websocket_connection.dart';
import 'package:piccolo_os/shells/desktop/widgets/dock.dart';
import 'package:piccolo_os/shells/desktop/widgets/dock_health_presentation.dart';

void main() {
  test('task monitor unavailable remains a warning state', () {
    final pressure = ResourcePressure.fromJson({
      'resource': 'tasks',
      'severity': 'warn',
      'reason_code': 'monitor_unavailable',
      'task_current': 34,
      'task_limit': 2311,
    });

    expect(pressure.isWarning, isTrue);
    expect(pressure.isMonitorUnavailable, isTrue);
    expect(pressure.taskCurrent, 34);
    expect(pressure.taskLimit, 2311);
  });

  test('runtime unknown and recovered events are app scoped', () {
    final unknown = ResourcePressure.fromJson({
      'resource': 'runtime',
      'severity': 'warn',
      'app_instance_id': 'namek',
    });
    final recovered = ResourcePressure.fromJson({
      'resource': 'runtime',
      'severity': 'ok',
      'app_instance_id': 'namek',
    });

    expect(unknown.isRuntimeUnknown, isTrue);
    expect(unknown.isRuntimeRecovered, isFalse);
    expect(recovered.isRuntimeRecovered, isTrue);
    expect(recovered.isRuntimeUnknown, isFalse);
  });

  test(
    'automatic recovery suppression is distinct from observation unknown',
    () {
      final suppressed = ResourcePressure.fromJson({
        'resource': 'runtime',
        'severity': 'warn',
        'reason_code': 'automatic_recovery_suppressed',
        'app_instance_id': 'namek',
      });

      expect(suppressed.isRecoverySuppressed, isTrue);
      expect(suppressed.isRuntimeUnknown, isFalse);
    },
  );

  test('dock presentation flows from disconnected to checking to hydrated', () {
    final disconnected = resolveDockHealthPresentation(
      connected: false,
      snapshotsPending: false,
      aggregateLevel: DockHealthLevel.healthy,
      taskCritical: true,
    );
    final checking = resolveDockHealthPresentation(
      connected: true,
      snapshotsPending: true,
      aggregateLevel: DockHealthLevel.healthy,
      taskCritical: true,
    );
    final hydrated = resolveDockHealthPresentation(
      connected: true,
      snapshotsPending: false,
      aggregateLevel: DockHealthLevel.healthy,
    );

    expect(disconnected.label, 'Offline');
    expect(disconnected.message, 'Connection lost - Reconnecting...');
    expect(checking.label, 'Checking');
    expect(checking.message, 'Connected - Waiting for health data...');
    expect(hydrated.label, 'Healthy');
    expect(hydrated.message, 'System Healthy');
  });

  test('dock recovery states use the exact neutral copy', () {
    final critical = resolveDockHealthPresentation(
      connected: true,
      snapshotsPending: false,
      aggregateLevel: DockHealthLevel.recovering,
      taskCritical: true,
    );
    final warning = resolveDockHealthPresentation(
      connected: true,
      snapshotsPending: false,
      aggregateLevel: DockHealthLevel.degraded,
      taskWarning: true,
    );
    final unavailable = resolveDockHealthPresentation(
      connected: true,
      snapshotsPending: false,
      aggregateLevel: DockHealthLevel.degraded,
      taskWarning: true,
      taskMonitorUnavailable: true,
    );
    final backoff = resolveDockHealthPresentation(
      connected: true,
      snapshotsPending: false,
      aggregateLevel: DockHealthLevel.degraded,
      automaticRecoveryBackoff: true,
    );
    final unknown = resolveDockHealthPresentation(
      connected: true,
      snapshotsPending: false,
      aggregateLevel: DockHealthLevel.degraded,
      unknownAppObservation: true,
    );

    expect(critical.label, 'Recovering');
    expect(critical.message, 'Piccolo is recovering.');
    expect(warning.label, 'Degraded');
    expect(
      warning.message,
      'Piccolo is under heavy load. Some actions may be temporarily unavailable.',
    );
    expect(unavailable.label, 'Degraded');
    expect(
      unavailable.message,
      'Piccolo cannot confirm system health right now. It will keep trying.',
    );
    expect(backoff.label, 'Degraded');
    expect(
      backoff.message,
      'Piccolo is recovering. It will retry automatically.',
    );
    expect(unknown.label, 'Degraded');
    expect(
      unknown.message,
      'Piccolo cannot confirm some app statuses right now. Last known status is shown.',
    );
  });

  test('dock recovery copy excludes internal implementation terminology', () {
    final messages = [
      resolveDockHealthPresentation(
        connected: true,
        snapshotsPending: false,
        aggregateLevel: DockHealthLevel.recovering,
        taskCritical: true,
      ).message,
      resolveDockHealthPresentation(
        connected: true,
        snapshotsPending: false,
        aggregateLevel: DockHealthLevel.degraded,
        taskWarning: true,
      ).message,
      resolveDockHealthPresentation(
        connected: true,
        snapshotsPending: false,
        aggregateLevel: DockHealthLevel.degraded,
        taskMonitorUnavailable: true,
      ).message,
      resolveDockHealthPresentation(
        connected: true,
        snapshotsPending: false,
        aggregateLevel: DockHealthLevel.degraded,
        automaticRecoveryBackoff: true,
      ).message,
      resolveDockHealthPresentation(
        connected: true,
        snapshotsPending: false,
        aggregateLevel: DockHealthLevel.degraded,
        unknownAppObservation: true,
      ).message,
    ];
    const forbiddenTerms = [
      'task exhaustion',
      'task pressure',
      'task-pressure',
      'pids',
      'cgroup',
      'culprit',
      'suspected owner',
    ];

    for (final message in messages) {
      final normalized = message.toLowerCase();
      for (final forbiddenTerm in forbiddenTerms) {
        expect(normalized, isNot(contains(forbiddenTerm)));
      }
    }
  });

  testWidgets(
    'mounted dock preserves health lifecycle without a healthy reconnect flash',
    (tester) async {
      final connection = _ControllableConnection();
      final client = EventStreamClient.withConnection(connection);
      addTearDown(client.dispose);

      await tester.pumpWidget(
        MaterialApp(
          home: Scaffold(
            body: Center(child: DockHealthIndicator(client: client)),
          ),
        ),
      );

      _expectHealthPresentation(
        tester,
        label: 'Offline',
        message: 'Connection lost - Reconnecting...',
      );

      connection.setState(WebSocketConnectionState.connected);
      await tester.pump();
      _expectHealthPresentation(
        tester,
        label: 'Checking',
        message: 'Connected - Waiting for health data...',
      );
      expect(find.text('Healthy'), findsNothing);

      connection
        ..emit(
          type: 'resource_pressure',
          payload: const {
            'resource': 'tasks',
            'severity': 'warn',
            'reason_code': 'warning_threshold',
          },
        )
        ..completeHealthSnapshot();
      await tester.pump();
      await tester.pump();
      _expectHealthPresentation(
        tester,
        label: 'Degraded',
        message:
            'Piccolo is under heavy load. Some actions may be temporarily unavailable.',
      );

      connection.emit(
        type: 'resource_pressure',
        payload: const {
          'resource': 'tasks',
          'severity': 'urgent',
          'reason_code': 'critical_threshold',
        },
      );
      await tester.pump();
      await tester.pump();
      _expectHealthPresentation(
        tester,
        label: 'Recovering',
        message: 'Piccolo is recovering.',
      );

      connection.setState(WebSocketConnectionState.disconnected);
      await tester.pump();
      _expectHealthPresentation(
        tester,
        label: 'Offline',
        message: 'Connection lost - Reconnecting...',
      );

      connection.setState(WebSocketConnectionState.connected);
      await tester.pump();
      _expectHealthPresentation(
        tester,
        label: 'Checking',
        message: 'Connected - Waiting for health data...',
      );
      expect(find.text('Healthy'), findsNothing);

      connection
        ..emit(
          type: 'resource_pressure',
          payload: const {
            'resource': 'tasks',
            'severity': 'warn',
            'reason_code': 'warning_threshold',
          },
        )
        ..completeHealthSnapshot();
      await tester.pump();
      await tester.pump();
      _expectHealthPresentation(
        tester,
        label: 'Degraded',
        message:
            'Piccolo is under heavy load. Some actions may be temporarily unavailable.',
      );
    },
  );

  testWidgets(
    'runtime unknown keeps the mounted app projection at last-known running',
    (tester) async {
      final connection = _ControllableConnection();
      final client = EventStreamClient.withConnection(connection);
      addTearDown(client.dispose);

      await tester.pumpWidget(
        MaterialApp(home: _AppCardStatusProbe(client: client)),
      );
      connection.setState(WebSocketConnectionState.connected);
      await tester.pump();
      expect(find.text('stopped'), findsOneWidget);

      connection.emit(
        type: 'app_status',
        payload: const {'app': 'namek', 'status': 'running'},
      );
      await tester.pump();
      await tester.pump();
      expect(find.text('Running'), findsOneWidget);

      connection.emit(
        type: 'resource_pressure',
        payload: const {
          'resource': 'runtime',
          'severity': 'warn',
          'reason_code': 'runtime_observation_unknown',
          'app_instance_id': 'namek',
        },
      );
      await tester.pump();
      await tester.pump();

      expect(find.text('Running'), findsOneWidget);
      expect(find.text('Last known status'), findsOneWidget);
      expect(find.text('stopped'), findsNothing);
      expect(find.text('error'), findsNothing);
    },
  );
}

void _expectHealthPresentation(
  WidgetTester tester, {
  required String label,
  required String message,
}) {
  expect(find.text(label), findsOneWidget);
  final tooltip = tester.widget<Tooltip>(find.byType(Tooltip));
  expect(tooltip.message, message);
}

class _ControllableConnection extends WebSocketConnection {
  _ControllableConnection() : super('ws://resource-pressure.test');

  final StreamController<dynamic> _messages =
      StreamController<dynamic>.broadcast();
  WebSocketConnectionState _testState = WebSocketConnectionState.disconnected;

  @override
  WebSocketConnectionState get state => _testState;

  @override
  Stream<dynamic> get messages => _messages.stream;

  void setState(WebSocketConnectionState state) {
    _testState = state;
    notifyListeners();
  }

  void emit({required String type, required Map<String, dynamic> payload}) {
    _messages.add(jsonEncode({'type': type, 'payload': payload}));
  }

  void completeHealthSnapshot() {
    for (final topic in const [
      'listener_health',
      'remote_config',
      'resource_pressure',
    ]) {
      emit(type: 'snapshot_complete', payload: {'topic': topic});
    }
  }

  @override
  void dispose() {
    unawaited(_messages.close());
    super.dispose();
  }
}

/// The production stage projects only [EventStreamClient.appStatusEvents] into
/// app cards; it deliberately receives runtime-unknown through the separate
/// resource-pressure stream. Mounting the full Stage would also initialize the
/// authenticated API/desktop shell, so this probe exercises that same stream
/// boundary without a network-heavy shell harness.
class _AppCardStatusProbe extends StatefulWidget {
  const _AppCardStatusProbe({required this.client});

  final EventStreamClient client;

  @override
  State<_AppCardStatusProbe> createState() => _AppCardStatusProbeState();
}

class _AppCardStatusProbeState extends State<_AppCardStatusProbe> {
  late App _app;
  StreamSubscription<AppStatusEvent>? _statusSubscription;
  StreamSubscription<ResourcePressure>? _pressureSubscription;
  bool _observationUnknown = false;

  @override
  void initState() {
    super.initState();
    _app = App(
      id: 'namek',
      name: 'namek',
      image: 'example/namek',
      type: 'user',
      status: AppStatusEvent.statusStopped,
    );
    _statusSubscription = widget.client.appStatusEvents.listen((event) {
      if (!mounted || event.app != _app.name) return;
      setState(() {
        _app = _app.copyWithStatus(event.status);
      });
    });
    _pressureSubscription = widget.client.resourcePressureEvents.listen((
      event,
    ) {
      if (!mounted || !event.isRuntimeUnknown) return;
      setState(() {
        _observationUnknown = true;
      });
    });
  }

  @override
  void dispose() {
    unawaited(_statusSubscription?.cancel());
    unawaited(_pressureSubscription?.cancel());
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return Column(
      children: [
        Text(_app.isRunning ? 'Running' : _app.status),
        if (_observationUnknown) const Text('Last known status'),
      ],
    );
  }
}
