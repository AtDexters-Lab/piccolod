import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/shared/widgets/terminal_widget_mixin.dart';

void main() {
  testWidgets('task-pressure terminal creation offers explicit retry', (
    tester,
  ) async {
    var attempts = 0;
    await tester.pumpWidget(
      MaterialApp(
        home: Scaffold(
          body: _TerminalHarness(
            createSession: () async {
              attempts++;
              throw ApiException(
                503,
                '{"error":"busy","code":"task_pressure","retryable":true}',
              );
            },
          ),
        ),
      ),
    );
    await tester.pump();

    expect(attempts, 1);
    expect(
      find.text(TerminalWidgetMixin.terminalTaskPressureMessage),
      findsOneWidget,
    );
    expect(find.widgetWithText(FilledButton, 'Retry'), findsOneWidget);
    await tester.pump(const Duration(seconds: 5));
    expect(attempts, 1, reason: 'task pressure must not trigger polling');

    await tester.tap(find.widgetWithText(FilledButton, 'Retry'));
    await tester.pump();
    await tester.pump();

    expect(attempts, 2);
    expect(
      find.text(TerminalWidgetMixin.terminalTaskPressureMessage),
      findsOneWidget,
    );
  });

  testWidgets(
    'generic terminal creation errors keep the existing output path',
    (
      tester,
    ) async {
      await tester.pumpWidget(
        MaterialApp(
          home: Scaffold(
            body: _TerminalHarness(
              createSession: () async {
                throw ApiException(503, '{"error":"service unavailable"}');
              },
            ),
          ),
        ),
      );
      await tester.pump();

      expect(
        find.text(TerminalWidgetMixin.terminalTaskPressureMessage),
        findsNothing,
      );
      expect(find.widgetWithText(FilledButton, 'Retry'), findsNothing);
      final state = tester.state<_TerminalHarnessState>(
        find.byType(_TerminalHarness),
      );
      expect(
        state.terminal.buffer.getText(),
        contains('Failed to create terminal session: service unavailable'),
      );
    },
  );
}

class _TerminalHarness extends StatefulWidget {
  const _TerminalHarness({required this.createSession});

  final Future<Object?> Function() createSession;

  @override
  State<_TerminalHarness> createState() => _TerminalHarnessState();
}

class _TerminalHarnessState extends State<_TerminalHarness>
    with TerminalWidgetMixin<_TerminalHarness> {
  @override
  String getSessionCreatePath() => '/api/v1/terminal/sessions';

  @override
  String getSessionAttachPath(String sessionId) =>
      '/api/v1/terminal/sessions/$sessionId/attach';

  @override
  Future<Object?> createTerminalSession() => widget.createSession();

  @override
  void initState() {
    super.initState();
    initTerminal();
    WidgetsBinding.instance.addPostFrameCallback((_) => connectTerminal());
  }

  @override
  void dispose() {
    disposeTerminal();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => buildTerminalView();
}
