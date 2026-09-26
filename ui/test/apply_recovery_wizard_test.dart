import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/core/models/task_progress.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/app_service.dart';
import 'package:piccolo_os/features/apps/apply_settlement.dart';
import 'package:piccolo_os/features/apps/installed_config_wizard.dart';
import 'package:piccolo_os/features/apps/manifest_update_wizard.dart';
import 'package:piccolo_os/features/apps/update_error_messages.dart';
import 'package:piccolo_os/shared/widgets/task_progress_panel.dart';

class _ApplyService extends AppService {
  _ApplyService() : super(ApiClient());
  Completer<dynamic> response = Completer<dynamic>();
  int posts = 0;
  String? taskId;

  final configPreview = InstalledConfigUpdateResult.fromJson({
    'instance_id': 'landing',
    'applicable': true,
    'dry_run_token': 'config-token',
    'metadata_only': false,
    'diff_kind': 'config',
  });
  final manifestPreview = ManifestUpdateResult.fromJson({
    'instance_id': 'landing',
    'applicable': true,
    'dry_run_token': 'manifest-token',
    'metadata_only': false,
    'diff_kind': 'manifest',
  });

  @override
  Future<InstalledConfigReadResult> getInstalledConfig(String appId) async =>
      const InstalledConfigReadResult(
        instanceId: 'landing',
        ledgerHealth: 'complete',
      );

  @override
  Future<InstalledConfigUpdateResult> dryRunInstalledConfigUpdate(
    String appId, {
    required Map<String, dynamic> inputs,
    required Map<String, String> secretActions,
    required List<String> regenerateInputs,
    required InstalledConfigReadResult base,
  }) async => configPreview;

  @override
  Future<InstalledConfigUpdateResult> applyInstalledConfigUpdate(
    String appId,
    InstalledConfigUpdateResult dryRun, {
    String? taskId,
  }) {
    posts++;
    this.taskId = taskId;
    return response.future.then(
      (value) => value as InstalledConfigUpdateResult,
    );
  }

  @override
  Future<ManifestUpdateConfigureResult> configureManifestUpdate(
    String appId,
    String yamlContent, {
    bool catalogPending = false,
  }) async => const ManifestUpdateConfigureResult(
    inputs: {},
    fields: [],
    secretGeneratedPreflight: [],
    eligible: true,
  );

  @override
  Future<ManifestUpdateResult> dryRunManifestUpdate(
    String appId,
    String yamlContent,
    Map<String, dynamic> inputs,
    List<String> regenerateInputs, {
    List<String> clearInputs = const [],
    bool catalogPending = false,
  }) async => manifestPreview;

  @override
  Future<ManifestUpdateResult> applyManifestUpdate(
    String appId,
    ManifestUpdateResult dryRun, {
    String? taskId,
    List<String> confirmations = const [],
    bool catalogPending = false,
  }) {
    posts++;
    this.taskId = taskId;
    return response.future.then((value) => value as ManifestUpdateResult);
  }
}

class _Harness {
  _Harness(this.tester, {required this.config});
  final WidgetTester tester;
  final bool config;
  final service = _ApplyService();
  int applied = 0;
  late TaskProgressPanel panel;
  Finder get wizard =>
      find.byType(config ? InstalledConfigWizard : ManifestUpdateWizard);
  Finder get apply =>
      find.widgetWithText(FilledButton, config ? 'Apply' : 'Update');

  Future<void> start() async {
    tester.view.physicalSize = const Size(1400, 1200);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);
    await tester.pumpWidget(
      MaterialApp(
        home: Scaffold(
          body: Builder(
            builder: (context) => TextButton(
              onPressed: () {
                unawaited(
                  showDialog<void>(
                    context: context,
                    builder: (_) => config
                        ? InstalledConfigWizard(
                            appId: 'landing',
                            appService: service,
                            onApplied: () async {
                              applied++;
                            },
                          )
                        : ManifestUpdateWizard(
                            appId: 'landing',
                            appService: service,
                            catalogPending: true,
                            onApplied: () async {
                              applied++;
                            },
                          ),
                  ),
                );
              },
              child: const Text('Open'),
            ),
          ),
        ),
      ),
    );
    await tester.tap(find.text('Open'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('Preview Changes'));
    await tester.pumpAndSettle();
    await tester.ensureVisible(apply);
    await tester.tap(apply);
    await tester.pump();
    panel = tester.widget<TaskProgressPanel>(find.byType(TaskProgressPanel));
    expect(service.posts, 1);
    expect(panel.taskId, service.taskId);
  }

  TaskProgressEvent terminal({
    String? error,
    Map<String, dynamic>? metadata,
    String? taskId,
  }) => TaskProgressEvent(
    taskId: taskId ?? panel.taskId,
    taskType: panel.taskType,
    phase: 'complete',
    progress: 100,
    message: 'Complete',
    isComplete: true,
    error: error,
    metadata: metadata,
  );

  Future<void> emit(TaskProgressEvent event) async {
    panel.onComplete!(event);
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 300));
  }

  Future<void> loseResponse({bool timeout = false}) async {
    service.response.completeError(
      timeout
          ? TimeoutException('lost response')
          : http.ClientException('lost response'),
    );
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 300));
  }

  void expectWaiting() {
    expect(find.byType(TaskProgressPanel), findsOneWidget);
    expect(
      tester.widget<TaskProgressPanel>(find.byType(TaskProgressPanel)).taskId,
      service.taskId,
    );
    expect(find.text(pendingApplyOutcomeMessage), findsOneWidget);
    final button = tester.widget<FilledButton>(find.byType(FilledButton).last);
    expect(button.onPressed, isNull);
    expect(service.posts, 1);
    expect(applied, 0);
  }

  Future<void> dispose() async {
    await tester.pumpWidget(const SizedBox());
    await tester.pump(const Duration(milliseconds: 300));
  }
}

void main() {
  for (final config in [true, false]) {
    final label = config ? 'config' : 'manifest';
    testWidgets('$label transport loss retains original task until success', (
      tester,
    ) async {
      final h = _Harness(tester, config: config);
      await h.start();
      await h.loseResponse();
      h.expectWaiting();
      await h.emit(h.terminal(taskId: 'another-task'));
      h.expectWaiting();
      await h.emit(h.terminal());
      expect(h.applied, 1);
      expect(h.wizard, findsNothing);
      await h.emit(h.terminal());
      expect(h.applied, 1);
      expect(h.service.posts, 1);
      await h.dispose();
    });

    testWidgets('$label timeout waits and displays terminal failure', (
      tester,
    ) async {
      final h = _Harness(tester, config: config);
      await h.start();
      await h.loseResponse(timeout: true);
      h.expectWaiting();
      await h.emit(
        h.terminal(error: 'Update failed; previous runtime restored'),
      );
      expect(
        find.text('Update failed; previous runtime restored'),
        findsOneWidget,
      );
      expect(find.byType(TaskProgressPanel), findsNothing);
      expect(h.applied, 0);
      expect(h.service.posts, 1);
      await h.dispose();
    });

    for (final outcome in ['success', 'failure', 'repair']) {
      testWidgets('$label cached $outcome settles after lost response', (
        tester,
      ) async {
        final h = _Harness(tester, config: config);
        await h.start();
        await h.emit(
          h.terminal(
            error: outcome == 'failure' ? 'Runtime switch rolled back' : null,
            metadata: outcome == 'repair'
                ? {
                    'access_repair_pending': true,
                    'access_repair_message':
                        'Committed; publication needs repair',
                  }
                : null,
          ),
        );
        expect(h.applied, 0);
        await h.loseResponse();
        if (outcome == 'failure') {
          expect(find.text('Runtime switch rolled back'), findsOneWidget);
          expect(h.applied, 0);
        } else if (outcome == 'repair') {
          expect(
            find.text('Committed; publication needs repair'),
            findsOneWidget,
          );
          expect(h.wizard, findsOneWidget);
          expect(h.applied, 1);
          await h.emit(h.terminal());
          expect(h.applied, 1);
        } else {
          expect(h.applied, 1);
          expect(h.wizard, findsNothing);
        }
        expect(h.service.posts, 1);
        await h.dispose();
      });
    }

    testWidgets('$label later repair event keeps warning open', (tester) async {
      final h = _Harness(tester, config: config);
      await h.start();
      await h.loseResponse();
      await h.emit(h.terminal(metadata: {'access_repair_pending': true}));
      expect(
        find.text(
          config
              ? 'Config committed, but access publication needs repair.'
              : 'Update committed, but access publication needs repair.',
        ),
        findsOneWidget,
      );
      expect(h.wizard, findsOneWidget);
      expect(h.applied, 1);
      await h.dispose();
    });

    for (final stale in [false, true]) {
      testWidgets(
        '$label HTTP ${stale ? 'stale preview' : 'rejection'} is authoritative',
        (tester) async {
          final h = _Harness(tester, config: config);
          await h.start();
          await h.emit(h.terminal());
          h.service.response.completeError(
            ApiException(
              stale ? 409 : 500,
              stale
                  ? '{"key":"update_preview_stale","message":"Expired preview"}'
                  : '{"message":"Request rejected"}',
            ),
          );
          await tester.pump();
          await tester.pump(const Duration(milliseconds: 300));
          expect(
            find.text(stale ? staleUpdatePreviewMessage : 'Request rejected'),
            findsOneWidget,
          );
          expect(h.applied, 0);
          expect(find.text(pendingApplyOutcomeMessage), findsNothing);
          expect(find.byType(TaskProgressPanel), findsNothing);
          await h.emit(h.terminal());
          expect(h.applied, 0);
          expect(h.service.posts, 1);
          await h.dispose();
        },
      );
    }

    testWidgets('$label HTTP repair takes precedence over cached success', (
      tester,
    ) async {
      final h = _Harness(tester, config: config);
      await h.start();
      await h.emit(h.terminal());
      h.service.response.complete(
        config
            ? InstalledConfigUpdateResult.fromJson({
                'instance_id': 'landing',
                'access_repair_pending': true,
                'access_repair_message': 'HTTP repair warning',
              })
            : ManifestUpdateResult.fromJson({
                'instance_id': 'landing',
                'access_repair_pending': true,
                'access_repair_message': 'HTTP repair warning',
              }),
      );
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 300));
      expect(find.text('HTTP repair warning'), findsOneWidget);
      expect(h.wizard, findsOneWidget);
      expect(h.applied, 1);
      await h.emit(h.terminal());
      expect(h.applied, 1);
      await h.dispose();
    });

    testWidgets('$label stale terminal cannot settle a later manual attempt', (
      tester,
    ) async {
      final h = _Harness(tester, config: config);
      await h.start();
      final oldPanel = h.panel;
      final oldTerminal = h.terminal();
      h.service.response.completeError(
        ApiException(500, '{"message":"Rejected"}'),
      );
      await tester.pump();
      h.service.response = Completer<dynamic>();
      await tester.tap(h.apply);
      await tester.pump();
      h.panel = tester.widget<TaskProgressPanel>(
        find.byType(TaskProgressPanel),
      );
      expect(h.panel.taskId, isNot(oldPanel.taskId));
      oldPanel.onComplete!(oldTerminal);
      await tester.pump();
      expect(h.applied, 0);
      await h.loseResponse();
      expect(h.applied, 0);
      await h.emit(h.terminal());
      expect(h.applied, 1);
      expect(h.service.posts, 2); // The second POST was explicitly requested.
      await h.dispose();
    });

    testWidgets('$label HTTP success after disposal has no callback', (
      tester,
    ) async {
      final h = _Harness(tester, config: config);
      await h.start();
      await h.dispose();
      h.service.response.complete(
        config ? h.service.configPreview : h.service.manifestPreview,
      );
      await tester.pump();
      await h.emit(h.terminal());
      expect(h.applied, 0);
      expect(h.service.posts, 1);
    });

    testWidgets(
      '$label unknown result permits close without retry or late callback',
      (tester) async {
        final h = _Harness(tester, config: config);
        await h.start();
        await h.loseResponse();
        h.expectWaiting();
        await tester.tap(find.text('Close'));
        await tester.pump();
        await tester.pump(const Duration(milliseconds: 300));
        expect(h.wizard, findsNothing);
        await h.emit(h.terminal());
        expect(h.applied, 0);
        expect(h.service.posts, 1);
        await h.dispose();
      },
    );
  }
}
