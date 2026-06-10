import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/features/apps/app_operation_lifecycle.dart';

void main() {
  group('app operation lifecycle policies', () {
    test('maps backend task types to typed operations', () {
      expect(
        appOperationTypeFromTaskType('update_image'),
        AppOperationType.updateImage,
      );
      expect(
        appOperationTypeFromTaskType('uninstall_app'),
        AppOperationType.uninstall,
      );
      expect(appOperationTypeFromTaskType('unknown'), isNull);
    });

    test('classifies deterministic submit rejection statuses', () {
      for (final statusCode in [400, 401, 403, 404, 409, 422, 423]) {
        expect(
          isDeterministicAppOperationSubmitRejectionStatus(statusCode),
          isTrue,
          reason: '$statusCode should be safe to treat as rejected',
        );
      }

      for (final statusCode in [408, 429, 500, 502, 503, 504]) {
        expect(
          isDeterministicAppOperationSubmitRejectionStatus(statusCode),
          isFalse,
          reason: '$statusCode may be transport or server-unclear',
        );
      }
    });

    test(
      'submit rejection without progress closes dialog and clears state',
      () {
        final settlement = AppOperationType.updateListeners.policy.settle(
          AppOperationOutcome.submitRejected,
        );

        expect(settlement.clearActiveOperation, isTrue);
        expect(settlement.clearRecentSubmission, isTrue);
        expect(settlement.closeProgressDialog, isTrue);
        expect(settlement.keepTracking, isFalse);
        expect(settlement.detailAction, AppOperationDetailAction.none);
        expect(settlement.isTerminal, isTrue);
      },
    );

    test('submit rejection with observed progress keeps tracking', () {
      final settlement = AppOperationType.updateListeners.policy.settle(
        AppOperationOutcome.submitRejected,
        hasProgress: true,
      );

      expect(settlement.clearActiveOperation, isFalse);
      expect(settlement.clearRecentSubmission, isFalse);
      expect(settlement.closeProgressDialog, isFalse);
      expect(settlement.keepTracking, isTrue);
      expect(settlement.detailAction, AppOperationDetailAction.none);
      expect(settlement.isTerminal, isFalse);
    });

    test('unclear submit failure keeps reattachment alive', () {
      final settlement = AppOperationType.updateListeners.policy.settle(
        AppOperationOutcome.submitUnclear,
      );

      expect(settlement.clearActiveOperation, isFalse);
      expect(settlement.clearRecentSubmission, isFalse);
      expect(settlement.closeProgressDialog, isFalse);
      expect(settlement.keepTracking, isTrue);
      expect(settlement.detailAction, AppOperationDetailAction.none);
      expect(settlement.isTerminal, isFalse);
    });

    test('http success completion respects operation semantics', () {
      final listenerSuccess = AppOperationType.updateListeners.policy
          .settleAfterHttpSuccess(isStillTracked: true);
      final updateStillTracked = AppOperationType.updateImage.policy
          .settleAfterHttpSuccess(isStillTracked: true);
      final updateDetached = AppOperationType.updateImage.policy
          .settleAfterHttpSuccess(isStillTracked: false);
      final rollbackStillTracked = AppOperationType.rollback.policy
          .settleAfterHttpSuccess(isStillTracked: true);

      expect(listenerSuccess, isNotNull);
      expect(listenerSuccess!.isTerminal, isTrue);
      expect(listenerSuccess.detailAction, AppOperationDetailAction.refresh);
      expect(listenerSuccess.notifyAppsChanged, isTrue);

      expect(updateStillTracked, isNull);
      expect(rollbackStillTracked, isNull);

      expect(updateDetached, isNotNull);
      expect(updateDetached!.isTerminal, isTrue);
      expect(updateDetached.readiness?.updateCompleted, isTrue);
      expect(updateDetached.notifyAppsChanged, isTrue);
    });

    test(
      'image update success and missing progress start readiness observation',
      () {
        final success = AppOperationType.updateImage.policy.settle(
          AppOperationOutcome.progressSucceeded,
        );
        final missingProgress = AppOperationType.updateImage.policy.settle(
          AppOperationOutcome.progressMissing,
        );

        expect(success.detailAction, AppOperationDetailAction.none);
        expect(success.readiness?.updateCompleted, isTrue);
        expect(success.notifyAppsChanged, isTrue);
        expect(success.isTerminal, isTrue);

        expect(missingProgress.detailAction, AppOperationDetailAction.none);
        expect(missingProgress.readiness?.updateCompleted, isFalse);
        expect(missingProgress.clearRecentSubmission, isTrue);
        expect(missingProgress.closeProgressDialog, isTrue);
        expect(missingProgress.isTerminal, isFalse);
      },
    );

    test('generic missing progress refreshes detail', () {
      final settlement = AppOperationType.updateListeners.policy.settle(
        AppOperationOutcome.progressMissing,
      );
      final missingTask = AppOperationType.updateListeners.policy.settle(
        AppOperationOutcome.activeTaskMissing,
      );

      expect(settlement.clearActiveOperation, isTrue);
      expect(settlement.clearRecentSubmission, isTrue);
      expect(settlement.detailAction, AppOperationDetailAction.refresh);
      expect(settlement.closeProgressDialog, isTrue);
      expect(settlement.readiness, isNull);
      expect(settlement.isTerminal, isFalse);

      expect(missingTask.clearActiveOperation, isTrue);
      expect(missingTask.clearRecentSubmission, isFalse);
      expect(missingTask.detailAction, AppOperationDetailAction.refresh);
      expect(missingTask.closeProgressDialog, isTrue);
      expect(missingTask.readiness, isNull);
      expect(missingTask.isTerminal, isFalse);
    });

    test(
      'uninstall clears only observed success and verifies fallback paths',
      () {
        final success = AppOperationType.uninstall.policy.settle(
          AppOperationOutcome.progressSucceeded,
        );
        final missingProgress = AppOperationType.uninstall.policy.settle(
          AppOperationOutcome.progressMissing,
        );
        final missingTask = AppOperationType.uninstall.policy.settle(
          AppOperationOutcome.activeTaskMissing,
        );

        expect(success.clearActiveOperation, isTrue);
        expect(success.detailAction, AppOperationDetailAction.clearDeletedApp);
        expect(success.notifyAppsChanged, isTrue);
        expect(success.closeProgressDialog, isTrue);
        expect(success.readiness, isNull);
        expect(success.isTerminal, isTrue);

        expect(missingProgress.clearActiveOperation, isTrue);
        expect(
          missingProgress.detailAction,
          AppOperationDetailAction.verifyDeletedApp,
        );
        expect(missingProgress.notifyAppsChanged, isTrue);
        expect(missingProgress.closeProgressDialog, isTrue);
        expect(missingProgress.readiness, isNull);
        expect(missingProgress.isTerminal, isFalse);

        expect(missingTask.clearActiveOperation, isTrue);
        expect(
          missingTask.detailAction,
          AppOperationDetailAction.verifyDeletedApp,
        );
        expect(missingTask.notifyAppsChanged, isTrue);
        expect(missingTask.closeProgressDialog, isTrue);
        expect(missingTask.readiness, isNull);
        expect(missingTask.isTerminal, isFalse);

        expect(success.clearRecentSubmission, isTrue);
        expect(missingProgress.clearRecentSubmission, isTrue);
        expect(missingTask.clearRecentSubmission, isFalse);
      },
    );
  });

  test('recent app operation serializes task type at the boundary', () {
    final submittedAt = DateTime.utc(2026, 6, 9, 12);
    final expiresAt = submittedAt.add(const Duration(minutes: 35));
    final record = RecentAppOperation(
      appId: 'drawguess',
      taskId: 'task-1',
      type: AppOperationType.uninstall,
      submittedAt: submittedAt,
      expiresAt: expiresAt,
    );

    final restored = RecentAppOperation.fromJson(record.toJson());

    expect(restored.appId, 'drawguess');
    expect(restored.taskId, 'task-1');
    expect(restored.type, AppOperationType.uninstall);
    expect(restored.submittedAt, submittedAt);
    expect(restored.expiresAt, expiresAt);
  });
}
