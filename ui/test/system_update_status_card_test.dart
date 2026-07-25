@TestOn('browser')
library;

import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/shells/desktop/features/settings/settings_controller.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/system_tab.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

void main() {
  Map<String, dynamic> statusJson({
    Map<String, dynamic> meta = const {},
    bool pending = false,
  }) => {
    'current_version': 'v0.2.40',
    'available_version': pending ? 'v0.2.40 (System Update 1014)' : 'v0.2.40',
    'pending': pending,
    'requires_reboot': pending,
    'last_checked': '2026-07-24T10:00:00Z',
    'meta': meta,
  };

  Widget systemScreen(SettingsController controller) => MaterialApp(
    theme: PiccoloTheme.lightTheme,
    home: Scaffold(
      body: SingleChildScrollView(
        child: AnimatedBuilder(
          animation: controller,
          builder: (context, _) => SystemTab(controller: controller),
        ),
      ),
    ),
  );

  testWidgets(
    'uncertain status renders retry state instead of update spinner',
    (
      tester,
    ) async {
      var requestCount = 0;
      final retry = Completer<dynamic>();
      final controller = SettingsController(
        osUpdateStatusFetcher: () async {
          requestCount++;
          if (requestCount == 1) {
            return statusJson(
              meta: {
                'degraded': true,
                'refreshing': true,
                'cache_empty': true,
              },
            );
          }
          if (requestCount == 2) {
            return retry.future;
          }
          throw StateError('network unavailable');
        },
      );
      await controller.fetchOSUpdate();

      await tester.pumpWidget(systemScreen(controller));
      await tester.pump();

      expect(
        find.text('Update status temporarily unavailable'),
        findsOneWidget,
      );
      expect(find.text('Retry'), findsOneWidget);
      expect(find.text('System update in progress...'), findsNothing);

      await tester.tap(find.text('Retry'));
      await tester.pump();
      expect(find.text('Refreshing update status...'), findsOneWidget);
      expect(controller.isLoading, isFalse);
      expect(controller.isOSUpdateLoading, isTrue);

      retry.complete(
        statusJson(
          meta: {
            'degraded': true,
            'refreshing': true,
            'cache_empty': true,
          },
        ),
      );
      await tester.pump();
      await tester.tap(find.text('Retry'));
      await tester.pump();
      expect(find.text('Unable to refresh update status.'), findsOneWidget);
      expect(find.text('Retry'), findsOneWidget);
      expect(find.text('Error loading settings'), findsNothing);
      expect(controller.osUpdate?.isUncertain, isTrue);
      expect(controller.error, isNull);

      await tester.pumpWidget(const SizedBox.shrink());
      controller.dispose();
    },
  );

  testWidgets(
    'pending snapshot stays actionable when enrichment is uncertain',
    (
      tester,
    ) async {
      var requestCount = 0;
      final controller = SettingsController(
        osUpdateStatusFetcher: () async {
          requestCount++;
          if (requestCount == 1) {
            return statusJson(
              pending: true,
              meta: {
                'degraded': true,
                'refreshing': true,
              },
            );
          }
          throw StateError('network unavailable');
        },
      );
      await controller.fetchOSUpdate();

      await tester.pumpWidget(systemScreen(controller));
      await tester.pump();

      expect(find.text('Update Available'), findsOneWidget);
      expect(find.text('Restart Now'), findsOneWidget);
      expect(find.text('Update status temporarily unavailable'), findsNothing);

      await controller.fetchOSUpdate();
      await tester.pump();
      expect(find.text('Update Available'), findsOneWidget);
      expect(find.text('Restart Now'), findsOneWidget);
      expect(find.text('Retry'), findsOneWidget);
      expect(
        find.textContaining('Some update details could not be refreshed.'),
        findsOneWidget,
      );

      await tester.pumpWidget(const SizedBox.shrink());
      controller.dispose();
    },
  );

  testWidgets(
    'first-load progress does not claim a retained system state',
    (tester) async {
      final response = Completer<dynamic>();
      final controller = SettingsController(
        osUpdateStatusFetcher: () => response.future,
      );
      final fetch = controller.fetchOSUpdate();

      await tester.pumpWidget(systemScreen(controller));
      await tester.pump();

      expect(find.text('Loading update status...'), findsOneWidget);
      expect(find.text('Please wait.'), findsOneWidget);
      expect(
        find.text('The latest known system state will remain available.'),
        findsNothing,
      );

      response.complete(statusJson());
      await fetch;
      await tester.pumpWidget(const SizedBox.shrink());
      controller.dispose();
    },
  );

  testWidgets(
    'first-load failure does not claim a retained system state',
    (tester) async {
      final controller = SettingsController(
        osUpdateStatusFetcher: () async {
          throw StateError('network unavailable');
        },
      );
      await controller.fetchOSUpdate();

      await tester.pumpWidget(systemScreen(controller));
      await tester.pump();

      expect(find.text('Update status could not be loaded'), findsOneWidget);
      expect(
        find.text('No previous update status is available.'),
        findsOneWidget,
      );
      expect(find.text('Showing the latest known system state.'), findsNothing);
      expect(find.text('Retry'), findsOneWidget);

      await tester.pumpWidget(const SizedBox.shrink());
      controller.dispose();
    },
  );
}
