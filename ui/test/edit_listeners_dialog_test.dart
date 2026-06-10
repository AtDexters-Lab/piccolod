import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/features/apps/widgets/edit_listeners_dialog.dart';

void main() {
  testWidgets('shows preserved draft error message', (tester) async {
    await tester.pumpWidget(
      MaterialApp(
        home: EditListenersDialog(
          initialListeners: [
            AppListener(name: 'web', guestPort: 8080),
          ],
          errorMessage:
              'Could not save listeners. Your changes are preserved; review and try again.',
        ),
      ),
    );

    expect(
      find.text(
        'Could not save listeners. Your changes are preserved; review and try again.',
      ),
      findsOneWidget,
    );
  });

  testWidgets('save returns edited listeners after closing dialog', (
    tester,
  ) async {
    List<AppListener>? result;

    await tester.pumpWidget(
      MaterialApp(
        home: Builder(
          builder: (context) => FilledButton(
            onPressed: () async {
              result = await showDialog<List<AppListener>>(
                context: context,
                builder: (context) => EditListenersDialog(
                  initialListeners: [
                    AppListener(name: 'web', guestPort: 8080),
                  ],
                ),
              );
            },
            child: const Text('Edit'),
          ),
        ),
      ),
    );

    await tester.tap(find.text('Edit'));
    await tester.pumpAndSettle();

    final fields = find.byType(TextFormField);
    await tester.enterText(fields.at(0), 'api');
    await tester.enterText(fields.at(1), '9090');
    await tester.tap(find.text('Save Changes'));
    await tester.pumpAndSettle();

    expect(find.text('Edit Listeners'), findsNothing);
    expect(result, isNotNull);
    expect(result!.single.name, 'api');
    expect(result!.single.guestPort, 9090);
  });

  testWidgets('cancel closes dialog without returning listeners', (
    tester,
  ) async {
    var completed = false;
    List<AppListener>? result;

    await tester.pumpWidget(
      MaterialApp(
        home: Builder(
          builder: (context) => FilledButton(
            onPressed: () async {
              result = await showDialog<List<AppListener>>(
                context: context,
                builder: (context) => EditListenersDialog(
                  initialListeners: [
                    AppListener(name: 'web', guestPort: 8080),
                  ],
                ),
              );
              completed = true;
            },
            child: const Text('Edit'),
          ),
        ),
      ),
    );

    await tester.tap(find.text('Edit'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('Cancel'));
    await tester.pumpAndSettle();

    expect(find.text('Edit Listeners'), findsNothing);
    expect(completed, isTrue);
    expect(result, isNull);
  });
}
