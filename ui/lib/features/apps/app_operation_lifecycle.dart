import 'package:piccolo_os/core/models/task_progress.dart';

const _legacyUpdateManifestTaskType = 'update_manifest';

enum AppOperationType {
  updateImage,
  updateConfig,
  updateManifest,
  updateListeners,
  start,
  stop,
  rollback,
  uninstall,
}

enum AppOperationPhase { submitting, running, failed }

enum AppOperationOutcome {
  submitRejected,
  submitUnclear,
  progressSucceeded,
  progressFailed,
  progressMissing,
  activeTaskMissing,
}

enum AppOperationDetailAction {
  none,
  refresh,
  clearDeletedApp,
  verifyDeletedApp,
}

bool isDeterministicAppOperationSubmitRejectionStatus(int statusCode) {
  return switch (statusCode) {
    400 || 401 || 403 || 404 || 409 || 422 || 423 => true,
    _ => false,
  };
}

class AppReadinessObservationRequest {
  const AppReadinessObservationRequest({required this.updateCompleted});

  final bool updateCompleted;
}

class AppOperationSettlement {
  const AppOperationSettlement({
    required this.clearActiveOperation,
    required this.clearRecentSubmission,
    required this.notifyAppsChanged,
    required this.detailAction,
    this.isTerminal = false,
    this.closeProgressDialog = false,
    this.keepTracking = false,
    this.readiness,
  });

  final bool clearActiveOperation;
  final bool clearRecentSubmission;
  final bool notifyAppsChanged;
  final AppOperationDetailAction detailAction;
  final bool isTerminal;
  final bool closeProgressDialog;
  final bool keepTracking;
  final AppReadinessObservationRequest? readiness;
}

class AppOperationPolicy {
  const AppOperationPolicy({
    required this.type,
    required this.taskType,
    required this.label,
    this.observesReadiness = false,
    this.clearsAppDetail = false,
    this.httpSuccessCompletesOperation = false,
  });

  final AppOperationType type;
  final String taskType;
  final String label;
  final bool observesReadiness;
  final bool clearsAppDetail;
  final bool httpSuccessCompletesOperation;

  AppOperationSettlement? settleAfterHttpSuccess({
    required bool isStillTracked,
  }) {
    if (isStillTracked && !httpSuccessCompletesOperation) return null;
    return settle(AppOperationOutcome.progressSucceeded);
  }

  AppOperationSettlement settle(
    AppOperationOutcome outcome, {
    bool hasProgress = false,
  }) {
    switch (outcome) {
      case AppOperationOutcome.submitRejected:
        if (hasProgress) {
          return const AppOperationSettlement(
            clearActiveOperation: false,
            clearRecentSubmission: false,
            notifyAppsChanged: false,
            detailAction: AppOperationDetailAction.none,
            keepTracking: true,
          );
        }
        return const AppOperationSettlement(
          clearActiveOperation: true,
          clearRecentSubmission: true,
          notifyAppsChanged: false,
          detailAction: AppOperationDetailAction.none,
          isTerminal: true,
          closeProgressDialog: true,
        );
      case AppOperationOutcome.submitUnclear:
        return const AppOperationSettlement(
          clearActiveOperation: false,
          clearRecentSubmission: false,
          notifyAppsChanged: false,
          detailAction: AppOperationDetailAction.none,
          keepTracking: true,
        );
      case AppOperationOutcome.progressSucceeded:
        return AppOperationSettlement(
          clearActiveOperation: true,
          clearRecentSubmission: true,
          notifyAppsChanged: true,
          detailAction: clearsAppDetail
              ? AppOperationDetailAction.clearDeletedApp
              : observesReadiness
              ? AppOperationDetailAction.none
              : AppOperationDetailAction.refresh,
          readiness: observesReadiness
              ? const AppReadinessObservationRequest(updateCompleted: true)
              : null,
          isTerminal: true,
          closeProgressDialog: true,
        );
      case AppOperationOutcome.progressFailed:
        return const AppOperationSettlement(
          clearActiveOperation: true,
          clearRecentSubmission: true,
          notifyAppsChanged: false,
          detailAction: AppOperationDetailAction.refresh,
          isTerminal: true,
        );
      case AppOperationOutcome.progressMissing:
        return AppOperationSettlement(
          clearActiveOperation: true,
          clearRecentSubmission: true,
          notifyAppsChanged: clearsAppDetail,
          detailAction: clearsAppDetail
              ? AppOperationDetailAction.verifyDeletedApp
              : observesReadiness
              ? AppOperationDetailAction.none
              : AppOperationDetailAction.refresh,
          readiness: observesReadiness
              ? const AppReadinessObservationRequest(updateCompleted: false)
              : null,
          closeProgressDialog: true,
        );
      case AppOperationOutcome.activeTaskMissing:
        return AppOperationSettlement(
          clearActiveOperation: true,
          clearRecentSubmission: false,
          notifyAppsChanged: clearsAppDetail,
          detailAction: clearsAppDetail
              ? AppOperationDetailAction.verifyDeletedApp
              : observesReadiness
              ? AppOperationDetailAction.none
              : AppOperationDetailAction.refresh,
          readiness: observesReadiness
              ? const AppReadinessObservationRequest(updateCompleted: false)
              : null,
          closeProgressDialog: true,
        );
    }
  }
}

class TrackedAppOperation {
  const TrackedAppOperation({
    required this.taskId,
    required this.type,
    required this.phase,
    required this.submittedAt,
    this.latest,
  });

  final String taskId;
  final AppOperationType type;
  final AppOperationPhase phase;
  final DateTime submittedAt;
  final TaskProgressEvent? latest;

  String get taskType => type.policy.taskType;
  bool get hasProgress => latest != null;

  TrackedAppOperation copyWith({
    AppOperationPhase? phase,
    TaskProgressEvent? latest,
  }) {
    return TrackedAppOperation(
      taskId: taskId,
      type: type,
      phase: phase ?? this.phase,
      submittedAt: submittedAt,
      latest: latest ?? this.latest,
    );
  }
}

class RecentAppOperation {
  const RecentAppOperation({
    required this.appId,
    required this.taskId,
    required this.type,
    required this.submittedAt,
    required this.expiresAt,
  });

  factory RecentAppOperation.fromJson(Map<String, dynamic> json) {
    final type = appOperationTypeFromTaskType(
      (json['task_type'] ?? '').toString(),
    );
    if (type == null) {
      throw const FormatException('Unknown app operation task type.');
    }
    return RecentAppOperation(
      appId: (json['app_id'] ?? '').toString(),
      taskId: (json['task_id'] ?? '').toString(),
      type: type,
      submittedAt:
          DateTime.tryParse((json['submitted_at'] ?? '').toString()) ??
          DateTime.fromMillisecondsSinceEpoch(0),
      expiresAt:
          DateTime.tryParse((json['expires_at'] ?? '').toString()) ??
          DateTime.fromMillisecondsSinceEpoch(0),
    );
  }

  final String appId;
  final String taskId;
  final AppOperationType type;
  final DateTime submittedAt;
  final DateTime expiresAt;

  bool get isExpired => DateTime.now().isAfter(expiresAt);

  Map<String, dynamic> toJson() => {
    'app_id': appId,
    'task_id': taskId,
    'task_type': type.policy.taskType,
    'submitted_at': submittedAt.toIso8601String(),
    'expires_at': expiresAt.toIso8601String(),
  };
}

const appOperationPolicies = <AppOperationType, AppOperationPolicy>{
  AppOperationType.updateImage: AppOperationPolicy(
    type: AppOperationType.updateImage,
    taskType: 'update_image',
    label: 'Updating image',
    observesReadiness: true,
  ),
  AppOperationType.updateConfig: AppOperationPolicy(
    type: AppOperationType.updateConfig,
    taskType: 'update_config',
    label: 'Updating config',
    httpSuccessCompletesOperation: true,
  ),
  AppOperationType.updateManifest: AppOperationPolicy(
    type: AppOperationType.updateManifest,
    taskType: 'update_service_app',
    label: 'Updating app',
    observesReadiness: true,
    httpSuccessCompletesOperation: true,
  ),
  AppOperationType.updateListeners: AppOperationPolicy(
    type: AppOperationType.updateListeners,
    taskType: 'update_listeners',
    label: 'Updating listeners',
    httpSuccessCompletesOperation: true,
  ),
  AppOperationType.start: AppOperationPolicy(
    type: AppOperationType.start,
    taskType: 'start_app',
    label: 'Starting app',
    httpSuccessCompletesOperation: true,
  ),
  AppOperationType.stop: AppOperationPolicy(
    type: AppOperationType.stop,
    taskType: 'stop_app',
    label: 'Stopping app',
    httpSuccessCompletesOperation: true,
  ),
  AppOperationType.rollback: AppOperationPolicy(
    type: AppOperationType.rollback,
    taskType: 'rollback_app',
    label: 'Rolling back app',
  ),
  AppOperationType.uninstall: AppOperationPolicy(
    type: AppOperationType.uninstall,
    taskType: 'uninstall_app',
    label: 'Uninstalling app',
    clearsAppDetail: true,
    httpSuccessCompletesOperation: true,
  ),
};

extension AppOperationTypePolicy on AppOperationType {
  AppOperationPolicy get policy => appOperationPolicies[this]!;
}

final Set<String> appOperationTaskTypeSet = appOperationPolicies.values
    .map((policy) => policy.taskType)
    .followedBy(const [_legacyUpdateManifestTaskType])
    .toSet();

AppOperationType? appOperationTypeFromTaskType(String taskType) {
  if (taskType == _legacyUpdateManifestTaskType) {
    return AppOperationType.updateManifest;
  }
  for (final entry in appOperationPolicies.entries) {
    if (entry.value.taskType == taskType) return entry.key;
  }
  return null;
}
