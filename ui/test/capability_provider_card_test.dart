import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/core/models/capability_models.dart';
import 'package:piccolo_os/features/apps/widgets/capability_provider_card.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

const disclosure =
    'Switching providers may interrupt requests. Provider-owned state is not migrated.';

void main() {
  testWidgets('shows when this app is the default provider', (tester) async {
    await tester.pumpWidget(
      _testApp(
        status: _status(defaultProvider: 'provider-one'),
      ),
    );

    expect(find.text('Default'), findsOneWidget);
    expect(find.text('Set as default'), findsNothing);
  });

  testWidgets('confirms the server disclosure before switching', (
    tester,
  ) async {
    var selectionCount = 0;
    await tester.pumpWidget(
      _testApp(
        status: _status(defaultProvider: 'provider-one'),
        appInstance: 'provider-two',
        onSetDefault: (_) async => selectionCount++,
      ),
    );

    await tester.tap(find.text('Set as default'));
    await tester.pumpAndSettle();

    expect(find.text('Set provider-two as default?'), findsOneWidget);
    expect(find.text(disclosure), findsOneWidget);
    expect(selectionCount, 0);

    await tester.tap(find.widgetWithText(FilledButton, 'Set as default').last);
    await tester.pumpAndSettle();

    expect(selectionCount, 1);
  });

  testWidgets('shows provider loading and retry states', (tester) async {
    var retryCount = 0;
    await tester.pumpWidget(
      _testApp(
        loading: true,
        onRetry: () => retryCount++,
      ),
    );

    expect(find.text('Loading provider status...'), findsOneWidget);
    expect(find.text('Retry'), findsNothing);

    await tester.pumpWidget(
      _testApp(
        error: 'Could not load provider status.',
        onRetry: () => retryCount++,
      ),
    );
    await tester.tap(find.text('Retry'));

    expect(retryCount, 1);
  });

  testWidgets('disables selection while the provider task is active', (
    tester,
  ) async {
    await tester.pumpWidget(
      _testApp(
        status: _status(defaultProvider: 'provider-one'),
        appInstance: 'provider-two',
        isSelecting: true,
        actionsPaused: true,
      ),
    );

    expect(find.text('Setting default...'), findsOneWidget);
    expect(
      tester
          .widget<FilledButton>(
            find.widgetWithText(FilledButton, 'Setting default...'),
          )
          .onPressed,
      isNull,
    );
  });
}

Widget _testApp({
  CapabilityStatus? status,
  String appInstance = 'provider-one',
  bool loading = false,
  String? error,
  bool isSelecting = false,
  bool actionsPaused = false,
  Future<void> Function(CapabilityStatus)? onSetDefault,
  VoidCallback? onRetry,
}) {
  return MaterialApp(
    theme: PiccoloTheme.lightTheme,
    home: Scaffold(
      body: SingleChildScrollView(
        child: CapabilityProviderCard(
          capability: 'ai.inference.openai.v1',
          status: status,
          appInstance: appInstance,
          loading: loading,
          error: error,
          isSelecting: isSelecting,
          actionsPaused: actionsPaused,
          onSetDefault: onSetDefault ?? (_) async {},
          onRetry: onRetry ?? () {},
        ),
      ),
    ),
  );
}

CapabilityStatus _status({required String defaultProvider}) {
  return CapabilityStatus(
    capability: 'ai.inference.openai.v1',
    defaultProvider: defaultProvider,
    providerChangeDisclosure: disclosure,
    providers: const [
      CapabilityProviderStatus(appInstance: 'provider-one', enabled: true),
      CapabilityProviderStatus(appInstance: 'provider-two', enabled: true),
    ],
  );
}
