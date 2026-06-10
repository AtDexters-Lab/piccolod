import 'dart:async';
import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/core/models/app_status_event.dart';
import 'package:piccolo_os/core/models/listener_health.dart';
import 'package:piccolo_os/core/models/task_progress.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/app_service.dart';
import 'package:piccolo_os/core/services/task_progress_client.dart';
import 'package:piccolo_os/core/utils/task_id.dart';
import 'package:piccolo_os/features/apps/app_launcher.dart';
import 'package:piccolo_os/features/apps/app_operation_lifecycle.dart';
import 'package:piccolo_os/features/apps/installed_config_wizard.dart';
import 'package:piccolo_os/features/apps/manifest_update_wizard.dart';
import 'package:piccolo_os/features/apps/widgets/edit_listeners_dialog.dart';
import 'package:piccolo_os/features/apps/widgets/health_banner.dart';
import 'package:piccolo_os/features/apps/widgets/local_fallback_overlay.dart';
import 'package:piccolo_os/features/apps/workspace_terminal.dart';
import 'package:piccolo_os/shared/widgets/app_icon.dart';
import 'package:piccolo_os/shared/widgets/app_log_download.dart';
import 'package:piccolo_os/shared/widgets/health_badge.dart';
import 'package:piccolo_os/shared/widgets/log_stream_viewer.dart';
import 'package:piccolo_os/shared/widgets/status_banner.dart';
import 'package:piccolo_os/shared/widgets/task_progress_panel.dart';
import 'package:piccolo_os/shared/widgets/uninstall_confirmation_dialog.dart';
import 'package:piccolo_os/shells/desktop/desktop_controller.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:url_launcher/url_launcher.dart';
import 'package:web/web.dart' as web;

class AppDetailView extends StatefulWidget {
  const AppDetailView({
    required this.appId,
    required this.appService,
    required this.desktopController,
    super.key,
    this.initialTab = 0,
    this.iconUrl,
    this.originalIconUrl,
  });
  static const int tabOverview = 0;
  static const int tabNetwork = 1;
  static const int tabConfiguration = 2;
  static const int tabLogs = 3;

  final String appId;
  final AppService appService;
  final DesktopController desktopController;
  final int initialTab;
  final String? iconUrl;
  final String? originalIconUrl;

  @override
  State<AppDetailView> createState() => _AppDetailViewState();
}

enum _AppMenuAction { applyYaml, rollback, uninstall }

class _ReadinessObservation {
  const _ReadinessObservation({
    required this.startedAt,
    required this.deadline,
    required this.updateCompleted,
  });

  final DateTime startedAt;
  final DateTime deadline;
  final bool updateCompleted;

  bool get expired => DateTime.now().isAfter(deadline);
}

class _ReadinessPresentation {
  const _ReadinessPresentation({
    required this.severity,
    required this.title,
    required this.message,
  });

  final BannerSeverity severity;
  final String title;
  final String message;
}

class _OperationDialogHandle {
  BuildContext? _context;
  bool _isOpen = true;
  bool _isClosing = false;
  bool _closeRequested = false;

  void attach(BuildContext context) {
    _context = context;
    if (_closeRequested) {
      scheduleMicrotask(close);
    }
  }

  void close() {
    if (!_isOpen || _isClosing) return;
    final context = _context;
    if (context == null) {
      _closeRequested = true;
      return;
    }
    final navigator = Navigator.of(context);
    _isClosing = true;
    navigator.pop();
  }

  void markClosed() {
    _isOpen = false;
    _isClosing = false;
    _context = null;
  }
}

class _OperationSubmitResult {
  const _OperationSubmitResult({
    required this.accepted,
    this.error,
    this.trackingContinues = false,
    this.blockedByActiveOperation = false,
  });

  final bool accepted;
  final Object? error;
  final bool trackingContinues;
  final bool blockedByActiveOperation;
}

class _PendingListenerSave {
  const _PendingListenerSave({
    required this.appName,
    required this.listeners,
  });

  final String appName;
  final List<AppListener> listeners;
}

class _RecentOperationFailure {
  const _RecentOperationFailure({
    required this.title,
    required this.message,
    this.severity = BannerSeverity.error,
    this.pendingListenerSave,
  });

  final String title;
  final String message;
  final BannerSeverity severity;
  final _PendingListenerSave? pendingListenerSave;
}

class _TaskProgressSubscription extends StatefulWidget {
  const _TaskProgressSubscription({
    required this.taskId,
    required this.onEvent,
  });

  final String taskId;
  final void Function(TaskProgressEvent event) onEvent;

  @override
  State<_TaskProgressSubscription> createState() =>
      _TaskProgressSubscriptionState();
}

class _TaskProgressSubscriptionState extends State<_TaskProgressSubscription> {
  TaskProgressClient? _client;
  StreamSubscription<TaskProgressEvent>? _sub;

  @override
  void initState() {
    super.initState();
    _connect();
  }

  @override
  void didUpdateWidget(_TaskProgressSubscription oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (oldWidget.taskId != widget.taskId) {
      _disconnect();
      _connect();
    }
  }

  @override
  void dispose() {
    _disconnect();
    super.dispose();
  }

  void _connect() {
    final url = TaskProgressClient.buildUrl(widget.taskId);
    _client = TaskProgressClient(url);
    _sub = _client!.events.listen((event) {
      if (!mounted) return;
      widget.onEvent(event);
    });
    _client!.connect();
  }

  void _disconnect() {
    unawaited(_sub?.cancel());
    _sub = null;
    _client?.dispose();
    _client = null;
  }

  @override
  Widget build(BuildContext context) => const SizedBox.shrink();
}

class _AppDetailViewState extends State<AppDetailView>
    with SingleTickerProviderStateMixin {
  late TabController _tabController;

  App? _app;
  List<ServiceEndpoint> _listeners = [];
  List<AppContainerStatus> _containers = [];
  String? _selectedService;
  bool _isLoading = true;
  String? _error;

  bool _snapshotAvailable = false;
  DateTime? _lastDetailRefreshAt;

  TrackedAppOperation? _activeOperation;
  _RecentOperationFailure? _recentOperationFailure;
  _ReadinessObservation? _readiness;
  Timer? _noProgressTimer;
  Timer? _readinessTimer;
  Timer? _activeTaskPollTimer;
  _OperationDialogHandle? _operationDialogHandle;
  final Set<String> _terminallySettledTaskIds = <String>{};
  final Set<String> _pendingSubmitTaskIds = <String>{};
  final Set<String> _pendingSubmitNoProgressTaskIds = <String>{};
  final Set<String> _missingProgressSettledSubmitTaskIds = <String>{};
  final Map<String, _PendingListenerSave> _pendingListenerSaves =
      <String, _PendingListenerSave>{};

  // SSE streams (via unified EventStreamClient)
  StreamSubscription<AppStatusEvent>? _statusSub;
  StreamSubscription<ListenerHealthEvent>? _healthSub;
  ListenerHealth? _primaryHealth;
  String? _primaryListenerName;

  static const _recentSubmissionTtl = Duration(minutes: 35);
  static const _noProgressTimeout = Duration(seconds: 12);
  static const _readinessWindow = Duration(seconds: 75);

  @override
  void initState() {
    super.initState();
    _tabController = TabController(
      length: 4,
      vsync: this,
      initialIndex: widget.initialTab.clamp(0, 3),
    );
    unawaited(_initialLoad());
    _activeTaskPollTimer = Timer.periodic(
      const Duration(seconds: 10),
      (_) => unawaited(_syncActiveOperationFromServer()),
    );
    _connectStatusStream();
    _connectHealthStream();
  }

  void _connectStatusStream() {
    final client = widget.desktopController.eventStreamClient;
    if (client == null) return;

    _statusSub = client.appStatusEvents.listen((event) {
      if (!mounted) return;
      if (event.app != widget.appId) return;
      setState(() {
        _app = _app?.copyWithStatus(
          event.status,
          statusMessage: event.message ?? '',
        );
      });
    });
  }

  void _connectHealthStream() {
    final client = widget.desktopController.eventStreamClient;
    if (client == null) return;

    _healthSub = client.healthEvents.listen((event) {
      if (!mounted) return;
      // Only process events for this specific app
      if (event.app != widget.appId) return;
      // Only update primary health from the primary listener's events
      if (_primaryListenerName != null &&
          event.listener != _primaryListenerName) {
        return;
      }
      setState(() {
        _primaryHealth = event.health;
      });
      if (_readiness != null) {
        unawaited(_loadData(showLoading: false));
      }
    });
  }

  @override
  void dispose() {
    unawaited(_statusSub?.cancel());
    unawaited(_healthSub?.cancel());
    _noProgressTimer?.cancel();
    _readinessTimer?.cancel();
    _activeTaskPollTimer?.cancel();
    _tabController.dispose();
    super.dispose();
  }

  Future<void> _initialLoad() async {
    await _loadData();
    await _syncActiveOperationFromServer();
  }

  Future<Object?> _loadData({bool showLoading = true}) async {
    if (showLoading) {
      setState(() {
        _isLoading = true;
        _error = null;
      });
    }

    try {
      final detail = await widget.appService.getAppDetail(widget.appId);

      if (!mounted) return null;

      setState(() {
        _app = detail.app;
        _listeners = detail.listeners;
        _containers = detail.containers;
        _snapshotAvailable = detail.snapshotAvailable;
        _primaryHealth = detail.app.primaryListenerHealth;
        // Identify the primary listener name for stream filtering
        _primaryListenerName = _findPrimaryListenerName(detail.listeners);
        _selectedService = _pickSelectedService(
          _selectedService,
          detail.containers,
        );
        _lastDetailRefreshAt = DateTime.now();
        _error = null;
        _isLoading = false;
      });
      return null;
    } on Object catch (e) {
      if (mounted) {
        final shouldShowError = showLoading || _app == null;
        setState(() {
          _error = shouldShowError ? e.toString() : null;
          _isLoading = false;
        });
      }
      return e;
    }
  }

  Future<_OperationSubmitResult> _handleActionWithProgress({
    required AppOperationType type,
    required Future<void> Function(String taskId) action,
    void Function(TrackedAppOperation operation)? onSubmitted,
  }) async {
    if (!mounted) return const _OperationSubmitResult(accepted: false);
    if (_activeOperation != null) {
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(
          content: Text(
            '${_activeOperation!.type.policy.label} is already in progress.',
          ),
        ),
      );
      return const _OperationSubmitResult(
        accepted: false,
        blockedByActiveOperation: true,
      );
    }

    final taskId = generateTaskId();
    final submittedOperation = _beginSubmittedOperation(
      taskId: taskId,
      type: type,
    );
    onSubmitted?.call(submittedOperation);
    final dialogHandle = _showOperationDialog(
      taskId: taskId,
      type: type,
    );

    Object? actionError;
    _pendingSubmitTaskIds.add(taskId);
    try {
      await action(taskId);
    } on Object catch (e) {
      actionError = e;
    } finally {
      _pendingSubmitTaskIds.remove(taskId);
      _pendingSubmitNoProgressTaskIds.remove(taskId);
    }

    if (actionError != null) {
      if (_missingProgressSettledSubmitTaskIds.remove(taskId)) {
        return const _OperationSubmitResult(accepted: true);
      }
      final operation = _operationForTask(
        taskId: taskId,
        fallback: submittedOperation,
      );
      final hasProgress =
          operation.taskId == _activeOperation?.taskId && operation.hasProgress;
      final settlement = type.policy.settle(
        _classifySubmitFailure(actionError),
        hasProgress: hasProgress,
      );
      if (settlement.closeProgressDialog) {
        dialogHandle.close();
      }
      final applied = await _applyOperationSettlement(operation, settlement);
      if (!applied) return const _OperationSubmitResult(accepted: true);
      final trackingAvailable =
          settlement.keepTracking && _activeOperation?.taskId == taskId;
      if (trackingAvailable) {
        final active = _activeOperation;
        if (active != null && !active.hasProgress) {
          _scheduleNoProgressTimeout(active);
        }
      }
      if (!mounted) return const _OperationSubmitResult(accepted: false);
      final message = trackingAvailable
          ? 'Request status is unclear; continuing to track ${type.policy.label.toLowerCase()}.'
          : 'Action failed: $actionError';
      ScaffoldMessenger.of(
        context,
      ).showSnackBar(SnackBar(content: Text(message)));
      return _OperationSubmitResult(
        accepted: false,
        error: actionError,
        trackingContinues: trackingAvailable,
      );
    }

    final operation = _operationForTask(
      taskId: taskId,
      fallback: submittedOperation,
    );
    _missingProgressSettledSubmitTaskIds.remove(taskId);
    final settlement = type.policy.settleAfterHttpSuccess(
      isStillTracked: operation.taskId == _activeOperation?.taskId,
    );
    if (settlement != null) {
      final applied = await _applyOperationSettlement(operation, settlement);
      if (applied && type == AppOperationType.updateListeners) {
        _pendingListenerSaves.remove(operation.taskId);
      }
      if (applied && mounted) {
        setState(() => _clearRecentOperationFailureForSuccess(type));
      }
    } else if (operation.taskId == _activeOperation?.taskId &&
        !operation.hasProgress) {
      _scheduleNoProgressTimeout(operation);
    }
    return const _OperationSubmitResult(accepted: true);
  }

  static AppOperationOutcome _classifySubmitFailure(Object error) {
    if (error is ApiException &&
        isDeterministicAppOperationSubmitRejectionStatus(error.statusCode)) {
      return AppOperationOutcome.submitRejected;
    }
    return AppOperationOutcome.submitUnclear;
  }

  Future<void> _syncActiveOperationFromServer() async {
    try {
      final tasks = await widget.appService.getActiveTasks();
      if (!mounted) return;
      final matching =
          tasks.where((task) {
            if (task.instanceId != widget.appId || task.isComplete) {
              return false;
            }
            return appOperationTypeFromTaskType(task.taskType) != null;
          }).toList()..sort((a, b) {
            final at = a.timestamp ?? DateTime.fromMillisecondsSinceEpoch(0);
            final bt = b.timestamp ?? DateTime.fromMillisecondsSinceEpoch(0);
            return bt.compareTo(at);
          });

      if (matching.isNotEmpty) {
        final task = matching.first;
        final type = appOperationTypeFromTaskType(task.taskType);
        if (type == null) return;
        _adoptOperation(
          taskId: task.taskId,
          type: type,
          phase: AppOperationPhase.running,
          submittedAt: task.timestamp ?? DateTime.now(),
          latest: task,
        );
        return;
      }

      final recent = _readRecentSubmission();
      if (recent == null || recent.appId != widget.appId || recent.isExpired) {
        _clearRecentSubmission();
        final active = _activeOperation;
        if (active != null) _settleOperationWithoutActiveTask(active);
        return;
      }
      _adoptOperation(
        taskId: recent.taskId,
        type: recent.type,
        phase: AppOperationPhase.submitting,
        submittedAt: recent.submittedAt,
      );
    } on Object catch (e) {
      debugPrint('Failed to sync active operation: $e');
    }
  }

  TrackedAppOperation _beginSubmittedOperation({
    required String taskId,
    required AppOperationType type,
  }) {
    _cancelReadinessObservation();
    final now = DateTime.now();
    final operation = TrackedAppOperation(
      taskId: taskId,
      type: type,
      phase: AppOperationPhase.submitting,
      submittedAt: now,
    );
    _writeRecentSubmission(
      RecentAppOperation(
        appId: widget.appId,
        taskId: taskId,
        type: type,
        submittedAt: now,
        expiresAt: now.add(_recentSubmissionTtl),
      ),
    );
    _adoptOperation(
      taskId: operation.taskId,
      type: operation.type,
      phase: operation.phase,
      submittedAt: operation.submittedAt,
    );
    return operation;
  }

  void _adoptWizardOperation({
    required String taskId,
    required String taskType,
  }) {
    final type = appOperationTypeFromTaskType(taskType);
    if (type == null) return;
    _beginSubmittedOperation(taskId: taskId, type: type);
  }

  void _adoptOperation({
    required String taskId,
    required AppOperationType type,
    required AppOperationPhase phase,
    required DateTime submittedAt,
    TaskProgressEvent? latest,
  }) {
    if (_terminallySettledTaskIds.contains(taskId)) return;
    final active = _activeOperation;
    if (active != null &&
        active.taskId == taskId &&
        latest == null &&
        !active.hasProgress) {
      return;
    }

    _noProgressTimer?.cancel();
    setState(() {
      if (_recentOperationFailure?.pendingListenerSave == null) {
        _recentOperationFailure = null;
      }
      _activeOperation = TrackedAppOperation(
        taskId: taskId,
        type: type,
        phase: phase,
        submittedAt: submittedAt,
        latest: latest,
      );
    });
    _scheduleNoProgressTimeout(_activeOperation!);
  }

  void _scheduleNoProgressTimeout(TrackedAppOperation operation) {
    if (operation.latest != null && operation.latest!.isComplete) return;
    _noProgressTimer?.cancel();
    _noProgressTimer = Timer(_noProgressTimeout, () {
      if (!mounted) return;
      final active = _activeOperation;
      if (active == null ||
          active.taskId != operation.taskId ||
          active.hasProgress) {
        return;
      }
      _handleNoProgressTimeout(active);
    });
  }

  TrackedAppOperation _operationForTask({
    required String taskId,
    required TrackedAppOperation fallback,
  }) {
    final active = _activeOperation;
    if (active != null && active.taskId == taskId) return active;
    return fallback;
  }

  void _handleOperationEvent(TaskProgressEvent event) {
    final active = _activeOperation;
    if (active == null || active.taskId != event.taskId) return;

    _noProgressTimer?.cancel();
    setState(() {
      _activeOperation = active.copyWith(
        phase: event.error != null && event.error!.isNotEmpty
            ? AppOperationPhase.failed
            : AppOperationPhase.running,
        latest: event,
      );
    });

    if (!event.isComplete) return;
    if (event.error != null && event.error!.isNotEmpty) {
      unawaited(_failOperation(active, event));
      return;
    }
    unawaited(_completeOperation(active));
  }

  Future<void> _failOperation(
    TrackedAppOperation operation,
    TaskProgressEvent event,
  ) async {
    final label = operation.type.policy.label;
    final error = event.error ?? 'Operation failed.';
    final pendingListenerSave =
        operation.type == AppOperationType.updateListeners
        ? _pendingListenerSaves.remove(operation.taskId)
        : null;
    final applied = await _applyOperationSettlement(
      operation,
      operation.type.policy.settle(AppOperationOutcome.progressFailed),
    );
    if (!applied) return;
    if (!mounted) return;
    setState(() {
      if (pendingListenerSave == null &&
          _recentOperationFailure?.pendingListenerSave != null) {
        return;
      }
      _recentOperationFailure = _RecentOperationFailure(
        title: '$label failed',
        message: error,
        pendingListenerSave: pendingListenerSave,
      );
    });
    ScaffoldMessenger.of(
      context,
    ).showSnackBar(SnackBar(content: Text('$label failed: $error')));
  }

  Future<void> _completeOperation(TrackedAppOperation operation) async {
    final applied = await _applyOperationSettlement(
      operation,
      operation.type.policy.settle(AppOperationOutcome.progressSucceeded),
    );
    if (applied && operation.type == AppOperationType.updateListeners) {
      _pendingListenerSaves.remove(operation.taskId);
    }
    if (applied && mounted) {
      setState(() => _clearRecentOperationFailureForSuccess(operation.type));
    }
  }

  void _clearRecentOperationFailureForSuccess(AppOperationType type) {
    final failure = _recentOperationFailure;
    if (failure == null) return;
    if (failure.pendingListenerSave != null &&
        type != AppOperationType.updateListeners) {
      return;
    }
    _recentOperationFailure = null;
  }

  void _handleNoProgressTimeout(TrackedAppOperation operation) {
    _noProgressTimer?.cancel();
    _noProgressTimer = null;
    var settledPendingSubmit = false;
    if (_pendingSubmitTaskIds.contains(operation.taskId)) {
      if (!_pendingSubmitNoProgressTaskIds.add(operation.taskId)) {
        _pendingSubmitTaskIds.remove(operation.taskId);
        _pendingSubmitNoProgressTaskIds.remove(operation.taskId);
        _missingProgressSettledSubmitTaskIds.add(operation.taskId);
        settledPendingSubmit = true;
      } else {
        _closeOperationDialog();
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(
            content: Text(
              'Still waiting for ${operation.type.policy.label.toLowerCase()} to be accepted. Continuing to track status.',
            ),
          ),
        );
        _scheduleNoProgressTimeout(operation);
        return;
      }
    }

    if (settledPendingSubmit) {
      _closeOperationDialog();
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(
          content: Text(
            'Could not confirm ${operation.type.policy.label.toLowerCase()} progress. Refreshed app state.',
          ),
        ),
      );
    }

    final settlement = operation.type.policy.settle(
      AppOperationOutcome.progressMissing,
    );
    final pendingListenerSave =
        operation.type == AppOperationType.updateListeners
        ? _pendingListenerSaves.remove(operation.taskId)
        : null;
    unawaited(
      _applyOperationSettlement(operation, settlement).then((applied) {
        if (!applied || !mounted || pendingListenerSave == null) return;
        setState(() {
          _recentOperationFailure = _RecentOperationFailure(
            title: '${operation.type.policy.label} status unclear',
            message:
                'Could not attach to progress. Your changes are preserved; refresh was attempted before retrying.',
            severity: BannerSeverity.warning,
            pendingListenerSave: pendingListenerSave,
          );
        });
      }),
    );
    if (settlement.detailAction == AppOperationDetailAction.refresh) {
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(
          content: Text(
            'Could not attach to ${operation.type.policy.label} progress. Refreshed app state.',
          ),
        ),
      );
    }
  }

  void _settleOperationWithoutActiveTask(TrackedAppOperation operation) {
    unawaited(
      _applyOperationSettlement(
        operation,
        operation.type.policy.settle(AppOperationOutcome.activeTaskMissing),
      ),
    );
  }

  Future<bool> _applyOperationSettlement(
    TrackedAppOperation? operation,
    AppOperationSettlement settlement,
  ) async {
    final taskId = operation?.taskId;
    if (taskId != null && _terminallySettledTaskIds.contains(taskId)) {
      return false;
    }
    if (settlement.isTerminal && taskId != null) {
      _terminallySettledTaskIds.add(taskId);
    }

    if (settlement.clearRecentSubmission) _clearRecentSubmission();
    if (settlement.notifyAppsChanged) {
      widget.desktopController.notifyAppsChanged();
    }
    if (settlement.closeProgressDialog) {
      _closeOperationDialog();
    }
    if (settlement.clearActiveOperation) {
      _clearActiveOperation(removeRecentSubmission: false);
    }

    if (settlement.detailAction == AppOperationDetailAction.refresh) {
      await _loadData(showLoading: false);
    } else if (settlement.detailAction ==
        AppOperationDetailAction.clearDeletedApp) {
      _clearDeletedAppDetail(operation);
    } else if (settlement.detailAction ==
        AppOperationDetailAction.verifyDeletedApp) {
      await _verifyDeletedAppDetail(operation);
    }

    final readiness = settlement.readiness;
    if (readiness != null && mounted) {
      _startReadinessObservation(updateCompleted: readiness.updateCompleted);
    }
    return true;
  }

  void _clearDeletedAppDetail(TrackedAppOperation? operation) {
    _noProgressTimer?.cancel();
    _noProgressTimer = null;
    _cancelReadinessObservation();
    if (!mounted) return;
    setState(() {
      if (operation == null || _activeOperation?.taskId == operation.taskId) {
        _activeOperation = null;
      }
      _app = null;
      _listeners = [];
      _containers = [];
      _selectedService = null;
      _snapshotAvailable = false;
      _primaryHealth = null;
      _primaryListenerName = null;
      _error = null;
    });
  }

  Future<void> _verifyDeletedAppDetail(TrackedAppOperation? operation) async {
    final error = await _loadData(showLoading: false);
    if (!mounted) return;
    if (error is! ApiException || error.statusCode != 404) {
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(
          content: Text('Uninstall status is unclear. Refreshed app state.'),
        ),
      );
      return;
    }
    _clearDeletedAppDetail(operation);
  }

  void _clearActiveOperation({required bool removeRecentSubmission}) {
    _noProgressTimer?.cancel();
    _noProgressTimer = null;
    if (removeRecentSubmission) _clearRecentSubmission();
    if (!mounted) return;
    setState(() => _activeOperation = null);
  }

  void _startReadinessObservation({required bool updateCompleted}) {
    _readinessTimer?.cancel();
    final now = DateTime.now();
    setState(() {
      _readiness = _ReadinessObservation(
        startedAt: now,
        deadline: now.add(_readinessWindow),
        updateCompleted: updateCompleted,
      );
    });
    unawaited(_loadData(showLoading: false));
    _readinessTimer = Timer(_readinessWindow, () {
      if (mounted) setState(() {});
    });
  }

  void _cancelReadinessObservation() {
    _readinessTimer?.cancel();
    _readinessTimer = null;
    if (_readiness != null && mounted) {
      setState(() => _readiness = null);
    }
  }

  _OperationDialogHandle _showOperationDialog({
    required String taskId,
    required AppOperationType type,
  }) {
    final handle = _OperationDialogHandle();
    _operationDialogHandle = handle;
    unawaited(
      showDialog<void>(
        context: context,
        builder: (dialogContext) {
          handle.attach(dialogContext);
          return AlertDialog(
            title: Text(type.policy.label),
            content: SizedBox(
              width: 520,
              child: TaskProgressPanel(
                taskId: taskId,
                taskType: type.policy.taskType,
                onComplete: (event) {
                  if (event.error != null && event.error!.isNotEmpty) return;
                  handle.close();
                },
              ),
            ),
            actions: [
              TextButton(
                onPressed: handle.close,
                child: const Text('Keep Working'),
              ),
            ],
          );
        },
      ).whenComplete(() {
        handle.markClosed();
        if (identical(_operationDialogHandle, handle)) {
          _operationDialogHandle = null;
        }
      }),
    );
    return handle;
  }

  void _closeOperationDialog() {
    final handle = _operationDialogHandle;
    if (handle == null) return;
    handle.close();
    _operationDialogHandle = null;
  }

  String _recentSubmissionKey() =>
      'piccolo.app-detail.recent-operation.${widget.appId}';

  RecentAppOperation? _readRecentSubmission() {
    try {
      final raw = web.window.localStorage.getItem(_recentSubmissionKey());
      if (raw == null || raw.isEmpty) return null;
      final decoded = jsonDecode(raw);
      if (decoded is! Map) return null;
      return RecentAppOperation.fromJson(Map<String, dynamic>.from(decoded));
    } on Object catch (e) {
      debugPrint('Failed to read recent app operation: $e');
      return null;
    }
  }

  void _writeRecentSubmission(RecentAppOperation submission) {
    try {
      web.window.localStorage.setItem(
        _recentSubmissionKey(),
        jsonEncode(submission.toJson()),
      );
    } on Object catch (e) {
      debugPrint('Failed to persist recent app operation: $e');
    }
  }

  void _clearRecentSubmission() {
    try {
      web.window.localStorage.removeItem(_recentSubmissionKey());
    } on Object catch (_) {}
  }

  Future<void> _handleUpdate() async {
    await _handleActionWithProgress(
      type: AppOperationType.updateImage,
      action: (taskId) =>
          widget.appService.updateApp(widget.appId, taskId: taskId),
    );
  }

  void _showManifestUpdateWizard() {
    if (_app == null) return;
    unawaited(
      showDialog<void>(
        context: context,
        barrierDismissible: false,
        builder: (context) => ManifestUpdateWizard(
          appId: _app!.name,
          appService: widget.appService,
          onTaskStarted: (taskId, taskType) =>
              _adoptWizardOperation(taskId: taskId, taskType: taskType),
          onApplied: () async {
            widget.desktopController.notifyAppsChanged();
            await _loadData(showLoading: false);
          },
        ),
      ),
    );
  }

  void _showInstalledConfigWizard() {
    if (_app == null) return;
    unawaited(
      showDialog<void>(
        context: context,
        barrierDismissible: false,
        builder: (context) => InstalledConfigWizard(
          appId: _app!.name,
          appService: widget.appService,
          onTaskStarted: (taskId, taskType) =>
              _adoptWizardOperation(taskId: taskId, taskType: taskType),
          onApplied: () async {
            widget.desktopController.notifyAppsChanged();
            await _loadData(showLoading: false);
          },
        ),
      ),
    );
  }

  Future<void> _confirmRollback() async {
    final confirmed = await showDialog<bool>(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text('Roll back to previous version?'),
        content: const Text(
          'This will restore the app and its data to the state before the last update. Any changes since the update will be lost.',
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(context).pop(false),
            child: const Text('Cancel'),
          ),
          FilledButton(
            onPressed: () => Navigator.of(context).pop(true),
            style: FilledButton.styleFrom(
              backgroundColor: PiccoloTheme.warning,
            ),
            child: const Text('Roll Back'),
          ),
        ],
      ),
    );

    if (confirmed != true) return;

    await _handleActionWithProgress(
      type: AppOperationType.rollback,
      action: (taskId) =>
          widget.appService.rollbackApp(widget.appId, taskId: taskId),
    );
  }

  Future<void> _confirmUninstall() async {
    final confirmed = await UninstallConfirmationDialog.show(
      context,
      appDisplayTitle: _app?.displayTitle ?? widget.appId,
    );

    if (confirmed != true) return;

    await _handleActionWithProgress(
      type: AppOperationType.uninstall,
      action: (taskId) => widget.appService.uninstallApp(
        widget.appId,
        taskId: taskId,
      ),
    );
  }

  void _showEditListenersDialog() {
    if (_app == null) return;

    final appName = _app!.name;
    // Convert current services to AppListener list
    final initialListeners = _listeners
        .map(AppListener.fromServiceEndpoint)
        .toList();

    unawaited(_editListeners(appName, initialListeners));
  }

  Future<void> _editListeners(
    String appName,
    List<AppListener> initialListeners, {
    String? initialErrorMessage,
  }) async {
    var draft = initialListeners;
    var errorMessage = initialErrorMessage;
    while (mounted && _app != null) {
      if (!mounted) return;
      final dialogContext = context;
      if (!dialogContext.mounted) return;
      final newListeners = await showDialog<List<AppListener>>(
        context: dialogContext,
        builder: (context) => EditListenersDialog(
          initialListeners: draft,
          errorMessage: errorMessage,
        ),
      );
      if (newListeners == null || !mounted || _app == null) return;
      String? pendingTaskId;
      final result = await _handleActionWithProgress(
        type: AppOperationType.updateListeners,
        onSubmitted: (operation) {
          pendingTaskId = operation.taskId;
          _pendingListenerSaves[operation.taskId] = _PendingListenerSave(
            appName: appName,
            listeners: List<AppListener>.from(newListeners),
          );
        },
        action: (taskId) => widget.appService.updateAppListeners(
          appName,
          newListeners,
          taskId: taskId,
        ),
      );
      if (result.accepted || result.trackingContinues) {
        return;
      }
      if (result.blockedByActiveOperation) {
        _replacePendingListenerRecoveryDraft(appName, newListeners);
        return;
      }
      if (pendingTaskId != null) {
        _pendingListenerSaves.remove(pendingTaskId);
      }
      draft = newListeners;
      errorMessage =
          'Could not save listeners. Your changes are preserved; review and try again.';
    }
  }

  void _replacePendingListenerRecoveryDraft(
    String appName,
    List<AppListener> listeners,
  ) {
    if (!mounted) return;
    final existing = _recentOperationFailure;
    final hasPendingRecovery = existing?.pendingListenerSave != null;
    setState(() {
      _recentOperationFailure = _RecentOperationFailure(
        title: hasPendingRecovery
            ? existing!.title
            : 'Updating listeners paused',
        message: hasPendingRecovery
            ? existing!.message
            : 'Another operation is in progress. Your changes are preserved; review them after it finishes.',
        severity: hasPendingRecovery
            ? existing!.severity
            : BannerSeverity.warning,
        pendingListenerSave: _PendingListenerSave(
          appName: appName,
          listeners: List<AppListener>.from(listeners),
        ),
      );
    });
  }

  Future<void> _reviewFailedListenerSave(
    _PendingListenerSave pending,
  ) async {
    if (!mounted || _app == null) return;
    _closeOperationDialog();
    await _editListeners(
      pending.appName,
      pending.listeners,
      initialErrorMessage:
          'Could not save listeners. Your changes are preserved; review and try again.',
    );
  }

  Future<void> _dismissOperationFailureBanner(
    _RecentOperationFailure failure,
  ) async {
    if (failure.pendingListenerSave != null) {
      final discard = await showDialog<bool>(
        context: context,
        builder: (context) => AlertDialog(
          title: const Text('Discard listener draft?'),
          content: const Text(
            'This will remove the preserved listener changes from this window.',
          ),
          actions: [
            TextButton(
              onPressed: () => Navigator.of(context).pop(false),
              child: const Text('Cancel'),
            ),
            FilledButton(
              style: FilledButton.styleFrom(
                backgroundColor: PiccoloTheme.critical,
              ),
              onPressed: () => Navigator.of(context).pop(true),
              child: const Text('Discard Draft'),
            ),
          ],
        ),
      );
      if (discard != true) return;
    }
    if (!mounted || !identical(_recentOperationFailure, failure)) return;
    setState(() => _recentOperationFailure = null);
  }

  void _openTerminal() {
    if (_app == null) return;

    final windowId = 'terminal-${_app!.name}';
    widget.desktopController.openApp(
      windowId,
      '${_app!.displayTitle} Terminal',
      PiccoloIcons.terminal,
      WorkspaceTerminal(
        appId: _app!.name,
        serviceName: _selectedService,
        onSessionEnd: () => widget.desktopController.closeWindow(windowId),
      ),
    );
  }

  @override
  Widget build(BuildContext context) {
    // Removed Scaffold/AppBar to fit into WindowFrame naturally
    return _buildBody();
  }

  Widget _buildBody() {
    if (_isLoading) return const Center(child: CircularProgressIndicator());
    if (_error != null) return Center(child: Text('Error: $_error'));
    if (_app == null) return const Center(child: Text('App uninstalled.'));

    return Column(
      children: [
        // Header
        _buildHeader(),

        if (_activeOperation != null) _buildOperationBanner(_activeOperation!),

        if (_recentOperationFailure != null)
          _buildOperationFailureBanner(_recentOperationFailure!),

        if (_readiness != null) _buildReadinessBanner(_readiness!),

        // Starting status banner
        if (_app!.isStarting) _buildStartingBanner(),

        // Error status banner
        if (_app!.isError) _buildErrorBanner(),

        // Health banner
        if (_primaryHealth != null && !_primaryHealth!.isOk)
          AppDetailHealthBanner(
            health: _primaryHealth!,
            lanFallbackUrl: _getLanFallbackUrl(),
            appService: widget.appService,
            desktopController: widget.desktopController,
          ),

        // Tabs
        TabBar(
          controller: _tabController,
          labelColor: PiccoloTheme.cobalt600,
          unselectedLabelColor: PiccoloTheme.inkMuted,
          indicatorColor: PiccoloTheme.cobalt600,
          tabs: const [
            Tab(text: 'Overview'),
            Tab(text: 'Network'),
            Tab(text: 'Configuration'),
            Tab(text: 'Logs'),
          ],
        ),

        // Content
        Expanded(
          child: ColoredBox(
            color: PiccoloTheme.mist,
            child: TabBarView(
              controller: _tabController,
              children: [
                _buildOverviewTab(),
                _buildNetworkTab(),
                _buildConfigTab(),
                _buildLogsTab(),
              ],
            ),
          ),
        ),
      ],
    );
  }

  Widget _buildHeader() {
    var statusColor = PiccoloTheme.inkMuted;
    if (_app!.isRunning) statusColor = PiccoloTheme.success;
    if (_app!.isStarting) statusColor = PiccoloTheme.warning;
    if (_app!.isError) statusColor = PiccoloTheme.critical;

    return Container(
      color: PiccoloTheme.porcelain,
      padding: const EdgeInsets.all(Spacing.lg),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          Row(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Container(
                width: 80,
                height: 80,
                decoration: BoxDecoration(
                  color: PiccoloTheme.mist,
                  borderRadius: BorderRadius.circular(Radii.lg),
                ),
                child: Center(
                  child: AppIcon(
                    proxyUrl: widget.iconUrl,
                    originalIconUrl: widget.originalIconUrl,
                    size: 64,
                    borderRadius: 12,
                    fallbackText: _app!.displayTitle.isNotEmpty
                        ? _app!.displayTitle[0]
                        : '?',
                    fallbackBackgroundColor: Colors.transparent,
                  ),
                ),
              ),
              const SizedBox(width: Spacing.lg),
              Expanded(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Text(
                      _app!.displayTitle,
                      maxLines: 2,
                      overflow: TextOverflow.ellipsis,
                      style: PiccoloTheme.textTheme.displayLarge?.copyWith(
                        fontSize: 24,
                      ),
                    ),
                    const SizedBox(height: Spacing.sm),
                    Wrap(
                      spacing: Spacing.base,
                      runSpacing: Spacing.sm,
                      crossAxisAlignment: WrapCrossAlignment.center,
                      children: [
                        _buildStatusPill(statusColor),
                        _buildImageLabel(),
                        if (_containers.length > 1) _buildServiceSelector(),
                      ],
                    ),
                  ],
                ),
              ),
            ],
          ),
          const SizedBox(height: Spacing.lg),
          _buildActionToolbar(),
        ],
      ),
    );
  }

  Widget _buildStatusPill(Color statusColor) {
    return Container(
      padding: const EdgeInsets.symmetric(
        horizontal: Spacing.sm,
        vertical: Spacing.xs,
      ),
      decoration: BoxDecoration(
        color: statusColor.withValues(alpha: 0.1),
        borderRadius: BorderRadius.circular(Radii.md),
      ),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          Container(
            width: 8,
            height: 8,
            decoration: BoxDecoration(
              color: statusColor,
              shape: BoxShape.circle,
            ),
          ),
          const SizedBox(width: 6),
          Text(
            _app!.status.toUpperCase(),
            style: TextStyle(
              color: statusColor,
              fontSize: 12,
              fontWeight: FontWeight.bold,
            ),
          ),
        ],
      ),
    );
  }

  Widget _buildImageLabel() {
    return ConstrainedBox(
      constraints: const BoxConstraints(maxWidth: 520),
      child: Text(
        'Image: ${_app!.image}',
        maxLines: 2,
        overflow: TextOverflow.ellipsis,
        style: PiccoloTheme.textTheme.labelSmall,
      ),
    );
  }

  Widget _buildActionToolbar() {
    final paused = _mutatingActionsPaused;
    final pauseReason = paused
        ? '${_activeOperation!.type.policy.label} is in progress'
        : null;

    Widget pausedTooltip(Widget child) {
      if (!paused || pauseReason == null) return child;
      return Tooltip(message: pauseReason, child: child);
    }

    return Wrap(
      spacing: Spacing.md,
      runSpacing: Spacing.sm,
      crossAxisAlignment: WrapCrossAlignment.center,
      children: [
        if (_app!.isRunning)
          FilledButton.icon(
            onPressed: _openTerminal,
            icon: const Icon(PiccoloIcons.terminal),
            label: const Text('Terminal'),
            style: FilledButton.styleFrom(
              backgroundColor: PiccoloTheme.cobalt600,
            ),
          ),
        if (!_app!.isWorkspace)
          pausedTooltip(
            OutlinedButton.icon(
              onPressed: paused ? null : _showInstalledConfigWizard,
              icon: const Icon(PiccoloIcons.settings),
              label: const Text('Edit Config'),
            ),
          ),
        if (!_app!.isWorkspace && _app!.isRunning)
          pausedTooltip(
            FilledButton.icon(
              onPressed: paused ? null : _handleUpdate,
              icon: const Icon(PiccoloIcons.refresh),
              label: const Text('Update Image'),
              style: FilledButton.styleFrom(
                backgroundColor: PiccoloTheme.cobalt600,
              ),
            ),
          ),
        pausedTooltip(
          _app!.isRunning
              ? FilledButton.icon(
                  onPressed: paused
                      ? null
                      : () => _handleActionWithProgress(
                          type: AppOperationType.stop,
                          action: (taskId) => widget.appService.stopApp(
                            _app!.name,
                            taskId: taskId,
                          ),
                        ),
                  icon: const Icon(PiccoloIcons.stop),
                  label: const Text('Stop'),
                  style: FilledButton.styleFrom(
                    backgroundColor: PiccoloTheme.inkMuted,
                  ),
                )
              : FilledButton.icon(
                  onPressed: paused
                      ? null
                      : () => _handleActionWithProgress(
                          type: AppOperationType.start,
                          action: (taskId) => widget.appService.startApp(
                            _app!.name,
                            taskId: taskId,
                          ),
                        ),
                  icon: const Icon(PiccoloIcons.play),
                  label: const Text('Start'),
                  style: FilledButton.styleFrom(
                    backgroundColor: PiccoloTheme.success,
                  ),
                ),
        ),
        PopupMenuButton<_AppMenuAction>(
          tooltip: paused ? pauseReason : 'More actions',
          enabled: !paused,
          onSelected: _handleOverflowAction,
          itemBuilder: (context) => [
            if (!_app!.isWorkspace &&
                _app!.isRunning &&
                _app!.catalogSource.isEmpty)
              const PopupMenuItem(
                value: _AppMenuAction.applyYaml,
                child: ListTile(
                  leading: Icon(PiccoloIcons.fileText),
                  title: Text('Apply YAML'),
                ),
              ),
            if (_snapshotAvailable && !_app!.isWorkspace)
              const PopupMenuItem(
                value: _AppMenuAction.rollback,
                child: ListTile(
                  leading: Icon(
                    PiccoloIcons.restart,
                    color: PiccoloTheme.warning,
                  ),
                  title: Text('Roll Back'),
                ),
              ),
            const PopupMenuDivider(),
            const PopupMenuItem(
              value: _AppMenuAction.uninstall,
              child: ListTile(
                leading: Icon(
                  PiccoloIcons.delete,
                  color: PiccoloTheme.critical,
                ),
                title: Text(
                  'Uninstall',
                  style: TextStyle(color: PiccoloTheme.critical),
                ),
              ),
            ),
          ],
          child: DecoratedBox(
            decoration: BoxDecoration(
              border: Border.all(
                color: paused ? PiccoloTheme.hairline : PiccoloTheme.inkMuted,
              ),
              borderRadius: BorderRadius.circular(Radii.md),
            ),
            child: const Padding(
              padding: EdgeInsets.symmetric(
                horizontal: Spacing.md,
                vertical: Spacing.sm,
              ),
              child: Row(
                mainAxisSize: MainAxisSize.min,
                children: [
                  Icon(PiccoloIcons.moreVert, size: 18),
                  SizedBox(width: Spacing.xs),
                  Text('More'),
                ],
              ),
            ),
          ),
        ),
      ],
    );
  }

  void _handleOverflowAction(_AppMenuAction action) {
    switch (action) {
      case _AppMenuAction.applyYaml:
        _showManifestUpdateWizard();
      case _AppMenuAction.rollback:
        unawaited(_confirmRollback());
      case _AppMenuAction.uninstall:
        unawaited(_confirmUninstall());
    }
  }

  Widget _buildOperationBanner(TrackedAppOperation operation) {
    final latest = operation.latest;
    final progress = latest?.progress ?? -1;
    final label = operation.type.policy.label;
    final message = latest?.message.isNotEmpty ?? false
        ? latest!.message
        : operation.phase == AppOperationPhase.submitting
        ? 'Submitting request'
        : 'Working';
    final isError =
        operation.phase == AppOperationPhase.failed ||
        (latest?.error != null && latest!.error!.isNotEmpty);

    return Container(
      padding: const EdgeInsets.all(Spacing.md),
      decoration: BoxDecoration(
        color: isError
            ? PiccoloTheme.critical.withValues(alpha: 0.08)
            : PiccoloTheme.cobalt600.withValues(alpha: 0.08),
        border: const Border(
          top: BorderSide(color: PiccoloTheme.hairline),
          bottom: BorderSide(color: PiccoloTheme.hairline),
        ),
      ),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          _TaskProgressSubscription(
            taskId: operation.taskId,
            onEvent: _handleOperationEvent,
          ),
          Icon(
            isError ? PiccoloIcons.error : PiccoloIcons.hourglass,
            color: isError ? PiccoloTheme.critical : PiccoloTheme.cobalt600,
            size: 20,
          ),
          const SizedBox(width: Spacing.sm),
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                Text(
                  label,
                  style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
                    fontWeight: FontWeight.w700,
                    color: isError
                        ? PiccoloTheme.critical
                        : PiccoloTheme.cobalt600,
                  ),
                ),
                const SizedBox(height: Spacing.xs),
                Text(
                  isError && latest?.error != null && latest!.error!.isNotEmpty
                      ? latest.error!
                      : '$message. Mutating actions are paused.',
                  style: PiccoloTheme.textTheme.bodySmall,
                ),
                const SizedBox(height: Spacing.sm),
                LinearProgressIndicator(
                  value: progress >= 0 ? progress / 100.0 : null,
                  minHeight: 6,
                  color: isError
                      ? PiccoloTheme.critical
                      : PiccoloTheme.cobalt600,
                  backgroundColor: PiccoloTheme.mist,
                ),
              ],
            ),
          ),
          const SizedBox(width: Spacing.md),
          TextButton(
            onPressed: () => _showOperationDialog(
              taskId: operation.taskId,
              type: operation.type,
            ),
            child: const Text('Details'),
          ),
        ],
      ),
    );
  }

  Widget _buildOperationFailureBanner(_RecentOperationFailure failure) {
    final pending = failure.pendingListenerSave;
    final canReviewPending = pending != null && _activeOperation == null;
    final accent = switch (failure.severity) {
      BannerSeverity.info => PiccoloTheme.info,
      BannerSeverity.warning => PiccoloTheme.warning,
      BannerSeverity.error => PiccoloTheme.critical,
    };
    final icon = switch (failure.severity) {
      BannerSeverity.info => PiccoloIcons.info,
      BannerSeverity.warning => PiccoloIcons.warning,
      BannerSeverity.error => PiccoloIcons.error,
    };
    return Container(
      padding: const EdgeInsets.all(Spacing.md),
      decoration: BoxDecoration(
        color: accent.withValues(alpha: 0.08),
        border: const Border(
          top: BorderSide(color: PiccoloTheme.hairline),
          bottom: BorderSide(color: PiccoloTheme.hairline),
        ),
      ),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Icon(icon, color: accent, size: 20),
          const SizedBox(width: Spacing.sm),
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                Text(
                  failure.title,
                  style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
                    fontWeight: FontWeight.w700,
                    color: accent,
                  ),
                ),
                const SizedBox(height: Spacing.xs),
                Text(
                  failure.message,
                  style: PiccoloTheme.textTheme.bodySmall?.copyWith(
                    color: PiccoloTheme.inkMuted,
                  ),
                ),
              ],
            ),
          ),
          if (pending != null) ...[
            const SizedBox(width: Spacing.sm),
            Tooltip(
              message: canReviewPending
                  ? 'Review preserved listener changes'
                  : 'Wait for the current operation to finish',
              child: TextButton(
                onPressed: canReviewPending
                    ? () => unawaited(_reviewFailedListenerSave(pending))
                    : null,
                child: const Text('Review Changes'),
              ),
            ),
          ],
          const SizedBox(width: Spacing.sm),
          IconButton(
            tooltip: pending != null ? 'Discard Draft' : 'Dismiss',
            onPressed: () => unawaited(
              _dismissOperationFailureBanner(failure),
            ),
            icon: Icon(
              pending != null ? PiccoloIcons.delete : PiccoloIcons.close,
            ),
          ),
        ],
      ),
    );
  }

  Widget _buildReadinessBanner(_ReadinessObservation observation) {
    final presentation = _readinessPresentation(observation);
    return StatusBanner(
      severity: presentation.severity,
      icon: presentation.severity == BannerSeverity.info
          ? PiccoloIcons.info
          : presentation.severity == BannerSeverity.warning
          ? PiccoloIcons.warning
          : PiccoloIcons.error,
      title: presentation.title,
      message: presentation.message,
      action: Wrap(
        spacing: Spacing.sm,
        children: [
          TextButton.icon(
            onPressed: () => _tabController.animateTo(AppDetailView.tabLogs),
            icon: const Icon(PiccoloIcons.article, size: 16),
            label: const Text('View Logs'),
          ),
          TextButton.icon(
            onPressed: () => _tabController.animateTo(AppDetailView.tabNetwork),
            icon: const Icon(PiccoloIcons.router, size: 16),
            label: const Text('Network'),
          ),
        ],
      ),
    );
  }

  _ReadinessPresentation _readinessPresentation(
    _ReadinessObservation observation,
  ) {
    final unknownPrefix = observation.updateCompleted
        ? ''
        : 'Update status unknown; ';
    if (_app == null) {
      return _ReadinessPresentation(
        severity: BannerSeverity.warning,
        title: '${unknownPrefix}app state unavailable',
        message: 'Refresh app detail or check logs for the current state.',
      );
    }
    if (_app!.isError) {
      return _ReadinessPresentation(
        severity: BannerSeverity.error,
        title: '${unknownPrefix}app needs attention',
        message: _app!.statusMessage.isNotEmpty
            ? _app!.statusMessage
            : 'The app is reporting an error after the operation.',
      );
    }

    final primary = _primaryListener();
    if (primary == null) {
      return _ReadinessPresentation(
        severity: BannerSeverity.info,
        title: observation.updateCompleted
            ? 'Updated'
            : 'Update status unknown',
        message:
            'No primary browser listener is configured. Use Network and Logs to verify this app.',
      );
    }

    final primaryHealth = _healthFor(primary);
    final fresh = _healthIsFresh(primaryHealth, observation);
    final secondaryWarnings = _secondaryListenerWarnings(primary.name);

    if (!fresh || primaryHealth == null || primaryHealth.status == 'unknown') {
      if (observation.expired) {
        return _ReadinessPresentation(
          severity: BannerSeverity.warning,
          title: '${unknownPrefix}listener still checking',
          message:
              'No fresh listener health sample arrived within the readiness window.',
        );
      }
      return _ReadinessPresentation(
        severity: BannerSeverity.info,
        title: '${unknownPrefix}checking app access',
        message: 'Waiting for fresh listener health after the operation.',
      );
    }

    if (primaryHealth.isError) {
      return _ReadinessPresentation(
        severity: BannerSeverity.error,
        title: '${unknownPrefix}app access needs attention',
        message: primaryHealth.reason.isNotEmpty
            ? primaryHealth.reason
            : 'The primary listener is not reachable.',
      );
    }

    if (primaryHealth.isRecovering) {
      if (observation.expired) {
        return _ReadinessPresentation(
          severity: BannerSeverity.warning,
          title: '${unknownPrefix}listener still recovering',
          message: primaryHealth.reason.isNotEmpty
              ? primaryHealth.reason
              : 'Automatic recovery is still in progress.',
        );
      }
      return _ReadinessPresentation(
        severity: BannerSeverity.info,
        title: '${unknownPrefix}checking app access',
        message: primaryHealth.reason.isNotEmpty
            ? primaryHealth.reason
            : 'The listener is recovering.',
      );
    }

    if (primaryHealth.isDegraded || secondaryWarnings.isNotEmpty) {
      final warningText = secondaryWarnings.isEmpty
          ? primaryHealth.reason
          : 'Secondary listener warnings: ${secondaryWarnings.join(', ')}';
      return _ReadinessPresentation(
        severity: BannerSeverity.warning,
        title: '${unknownPrefix}app ready with warnings',
        message: warningText.isNotEmpty
            ? warningText
            : 'The app is usable, but listener health has warnings.',
      );
    }

    return _ReadinessPresentation(
      severity: BannerSeverity.info,
      title: '${unknownPrefix}app ready',
      message: 'The primary listener is healthy.',
    );
  }

  ServiceEndpoint? _primaryListener() {
    final name = _primaryListenerName;
    if (name == null) return null;
    for (final listener in _listeners) {
      if (listener.name == name) return listener;
    }
    return null;
  }

  ListenerHealth? _healthFor(ServiceEndpoint listener) {
    if (listener.name == _primaryListenerName) {
      return _primaryHealth ?? listener.health ?? _app?.primaryListenerHealth;
    }
    return listener.health;
  }

  bool _healthIsFresh(
    ListenerHealth? health,
    _ReadinessObservation observation,
  ) {
    if (health == null) return false;
    final lastChecked = DateTime.tryParse(health.lastChecked ?? '');
    if (lastChecked != null) {
      return !lastChecked.isBefore(observation.startedAt);
    }
    final lastRefresh = _lastDetailRefreshAt;
    return lastRefresh != null && !lastRefresh.isBefore(observation.startedAt);
  }

  List<String> _secondaryListenerWarnings(String primaryName) {
    final warnings = <String>[];
    for (final listener in _listeners) {
      if (listener.name == primaryName) continue;
      final health = listener.health;
      if (health == null || health.isOk) continue;
      warnings.add(listener.name);
    }
    return warnings;
  }

  bool get _mutatingActionsPaused => _activeOperation != null;

  static String? _findPrimaryListenerName(List<ServiceEndpoint> listeners) {
    // Prefer the endpoint marked as primary
    for (final l in listeners) {
      if (l.primary) return l.name;
    }
    // RFC: first HTTP or WebSocket listener; null for raw-only apps
    for (final l in listeners) {
      if (l.protocol == 'http' || l.protocol == 'ws') return l.name;
    }
    return null;
  }

  String _getLanFallbackUrl() {
    // Prefer the primary listener's fallback URL
    final primary = _primaryListenerName;
    if (primary != null) {
      for (final l in _listeners) {
        if (l.name == primary) {
          return l.lanFallbackUrl ?? l.lanHostUrl ?? l.localUrl ?? '';
        }
      }
    }
    // Fallback: first listener with a URL
    for (final l in _listeners) {
      if (l.lanFallbackUrl != null) return l.lanFallbackUrl!;
    }
    for (final l in _listeners) {
      if (l.lanHostUrl != null) return l.lanHostUrl!;
    }
    for (final l in _listeners) {
      if (l.localUrl != null) return l.localUrl!;
    }
    return '';
  }

  Widget _buildOverviewTab() {
    return ListView(
      padding: const EdgeInsets.all(Spacing.lg),
      children: [
        _buildSectionTitle('Storage Volumes'),
        if (_app!.volumes.isEmpty)
          const Text('No persistent volumes configured.')
        else
          ..._app!.volumes.map(
            (v) => Card(
              child: ListTile(
                leading: const Icon(PiccoloIcons.storage),
                title: Text(v.containerPath),
                subtitle: Text('Host: ${v.hostPath}'),
                trailing: Text(v.sizeLimit),
              ),
            ),
          ),

        const SizedBox(height: Spacing.lg),
        _buildSectionTitle(
          _containers.length > 1 && (_selectedService?.isNotEmpty ?? false)
              ? 'Environment Variables (${_selectedService!})'
              : 'Environment Variables',
        ),
        if (_app!.environmentForService(_selectedService).isEmpty)
          const Text('No environment variables.')
        else
          Container(
            padding: const EdgeInsets.all(Spacing.base),
            decoration: BoxDecoration(
              color: PiccoloTheme.porcelain,
              borderRadius: BorderRadius.circular(Radii.sm),
              border: Border.all(color: PiccoloTheme.hairline),
            ),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: _app!
                  .environmentForService(_selectedService)
                  .entries
                  .map(
                    (e) => Padding(
                      padding: const EdgeInsets.symmetric(vertical: Spacing.xs),
                      child: Row(
                        crossAxisAlignment: CrossAxisAlignment.start,
                        children: [
                          SizedBox(
                            width: 120,
                            child: SelectableText(
                              e.key,
                              style: const TextStyle(
                                fontWeight: FontWeight.bold,
                                fontFamily: 'JetBrainsMono',
                                fontSize: 12,
                              ),
                            ),
                          ),
                          Expanded(
                            child: SelectableText(
                              e.value,
                              style: const TextStyle(
                                fontFamily: 'JetBrainsMono',
                                fontSize: 12,
                              ),
                            ),
                          ),
                        ],
                      ),
                    ),
                  )
                  .toList(),
            ),
          ),
      ],
    );
  }

  Widget _buildNetworkTab() {
    final content = _listeners.isEmpty
        ? const Center(child: Text('No network services exposed.'))
        : ListView(
            padding: const EdgeInsets.all(Spacing.lg),
            children: _listeners.map((svc) {
              final access = classifyAccess(svc.auth);
              return Card(
                child: Padding(
                  padding: const EdgeInsets.all(Spacing.base),
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      Row(
                        children: [
                          const Icon(
                            PiccoloIcons.router,
                            color: PiccoloTheme.cobalt600,
                          ),
                          const SizedBox(width: Spacing.md),
                          Text(
                            svc.name,
                            style: const TextStyle(
                              fontWeight: FontWeight.bold,
                              fontSize: 16,
                            ),
                          ),
                          if (svc.health != null && !svc.health!.isOk) ...[
                            const SizedBox(width: Spacing.sm),
                            HealthBadge(health: svc.health),
                          ],
                          const Spacer(),
                          Container(
                            padding: const EdgeInsets.symmetric(
                              horizontal: Spacing.sm,
                              vertical: 2,
                            ),
                            decoration: BoxDecoration(
                              color: PiccoloTheme.mist,
                              borderRadius: BorderRadius.circular(Radii.xxs),
                              border: Border.all(color: PiccoloTheme.hairline),
                            ),
                            child: Text(
                              svc.protocol.toUpperCase(),
                              style: const TextStyle(fontSize: 12),
                            ),
                          ),
                          if (access != 'private') ...[
                            const SizedBox(width: Spacing.sm),
                            _buildAccessBadge(access),
                          ],
                        ],
                      ),
                      const Divider(height: Spacing.lg),
                      _buildNetworkRow('Internal Port', '${svc.guestPort}'),
                      if (svc.lanHostUrl != null)
                        Builder(
                          builder: (_) {
                            final host = Uri.base.host.toLowerCase();
                            // When accessed via IP, prefer localUrl (port-based) since
                            // mDNS hostname won't resolve for this user.
                            final useLocal =
                                AppLauncher.isIpAddress(host) ||
                                AppLauncher.isLoopback(host);
                            final url = useLocal
                                ? (svc.localUrl ?? svc.lanHostUrl!)
                                : svc.lanHostUrl!;
                            return _buildNetworkLinkRow(
                              'LAN Access',
                              url,
                              onTap: () => launchUrl(Uri.parse(url)),
                              icon: PiccoloIcons.openExternal,
                              tooltip: 'Opens in new tab',
                            );
                          },
                        ),
                      if (svc.localUrl != null)
                        _buildNetworkLinkRow(
                          svc.lanHostUrl != null
                              ? 'LAN Fallback'
                              : 'LAN Access',
                          '${svc.localUrl} (Port ${svc.publicPort}${svc.portClaim != null ? ", claimed" : ""})',
                          onTap: () => AppLauncher.openAppWindow(
                            controller: widget.desktopController,
                            appService: widget.appService,
                            app: _app!,
                            service: svc,
                          ),
                          icon: PiccoloIcons.webAsset,
                          tooltip: 'Opens in app window',
                        ),
                      if (svc.remoteUrls.isNotEmpty)
                        ...svc.remoteUrls.map((url) {
                          // Only the context-aware URL (matching current portal) can be
                          // embedded in an iframe — other portals have different cookies/OIDC.
                          final isCurrentPortal = url == svc.remoteUrl;
                          return _buildNetworkLinkRow(
                            'Remote Access',
                            url,
                            onTap: isCurrentPortal
                                ? () => AppLauncher.healthGatedOpen(
                                    context: context,
                                    controller: widget.desktopController,
                                    appService: widget.appService,
                                    app: _app!,
                                    service: svc,
                                    overrideUrl: url,
                                    healthOverride:
                                        svc.name == _primaryListenerName
                                        ? _primaryHealth
                                        : null,
                                  )
                                : () => _healthGatedLaunchUrl(url, svc),
                            icon: isCurrentPortal
                                ? PiccoloIcons.webAsset
                                : PiccoloIcons.openExternal,
                            tooltip: isCurrentPortal
                                ? 'Opens in app window'
                                : 'Opens in browser',
                          );
                        })
                      else if (svc.remoteUrl != null)
                        _buildNetworkLinkRow(
                          'Remote Access',
                          svc.remoteUrl!,
                          onTap: () => AppLauncher.healthGatedOpen(
                            context: context,
                            controller: widget.desktopController,
                            appService: widget.appService,
                            app: _app!,
                            service: svc,
                            overrideUrl: svc.remoteUrl,
                            healthOverride: svc.name == _primaryListenerName
                                ? _primaryHealth
                                : null,
                          ),
                          icon: PiccoloIcons.webAsset,
                          tooltip: 'Opens in app window',
                        ),
                    ],
                  ),
                ),
              );
            }).toList(),
          );

    return Column(
      children: [
        if (_app!.isWorkspace)
          Padding(
            padding: const EdgeInsets.fromLTRB(
              Spacing.lg,
              Spacing.lg,
              Spacing.lg,
              0,
            ),
            child: Row(
              mainAxisAlignment: MainAxisAlignment.end,
              children: [
                OutlinedButton.icon(
                  onPressed: _mutatingActionsPaused
                      ? null
                      : _showEditListenersDialog,
                  icon: const Icon(PiccoloIcons.edit, size: 16),
                  label: const Text('Edit Listeners'),
                ),
              ],
            ),
          ),
        Expanded(child: content),
      ],
    );
  }

  Widget _buildConfigTab() {
    // Ideally this would show the original YAML, but the API doesn't return it yet.
    // We can just show a JSON dump of the App object for now.
    return Padding(
      padding: const EdgeInsets.all(Spacing.lg),
      child: Container(
        padding: const EdgeInsets.all(Spacing.base),
        decoration: BoxDecoration(
          color: PiccoloTheme.porcelain,
          borderRadius: BorderRadius.circular(Radii.sm),
          border: Border.all(color: PiccoloTheme.hairline),
        ),
        child: SelectableText(
          'App ID: ${_app!.id}\n'
          'Type: ${_app!.type}\n'
          'Container ID: ${_app!.containerId}\n',
          style: const TextStyle(fontFamily: 'JetBrainsMono'),
        ),
      ),
    );
  }

  Widget _buildLogsTab() {
    return Padding(
      padding: const EdgeInsets.all(Spacing.lg),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          // Combined-across-services historic download. Does NOT
          // take _selectedService — it always downloads all services.
          AppLogDownload(appName: _app!.name),
          const SizedBox(height: Spacing.lg),
          Expanded(
            child: LogStreamViewer(
              appName: _app!.name,
              serviceName: _selectedService,
            ),
          ),
        ],
      ),
    );
  }

  String? _pickSelectedService(
    String? current,
    List<AppContainerStatus> containers,
  ) {
    if (containers.isEmpty) return null;
    if (current != null && containers.any((c) => c.service == current)) {
      return current;
    }
    return containers.first.service;
  }

  Widget _buildServiceSelector() {
    if (_containers.length <= 1) return const SizedBox.shrink();

    var selected = _selectedService;
    if (selected == null || !_containers.any((c) => c.service == selected)) {
      selected = _containers.first.service;
    }

    return Container(
      padding: const EdgeInsets.symmetric(
        horizontal: Spacing.md,
        vertical: Spacing.xs,
      ),
      decoration: BoxDecoration(
        color: PiccoloTheme.mist,
        borderRadius: BorderRadius.circular(Radii.sm),
        border: Border.all(color: PiccoloTheme.hairline),
      ),
      child: DropdownButtonHideUnderline(
        child: DropdownButton<String>(
          value: selected,
          isDense: true,
          items: _containers
              .map(
                (c) => DropdownMenuItem<String>(
                  value: c.service,
                  child: Text(c.running ? c.service : '${c.service} (stopped)'),
                ),
              )
              .toList(),
          onChanged: (value) => setState(() => _selectedService = value),
        ),
      ),
    );
  }

  Widget _buildStartingBanner() {
    return StatusBanner(
      severity: BannerSeverity.warning,
      icon: PiccoloIcons.hourglass,
      title: 'App is starting...',
      message: _app!.statusMessage.isNotEmpty
          ? _app!.statusMessage
          : 'The app is initializing. Check logs if startup takes too long.',
      action: TextButton.icon(
        onPressed: () => _tabController.animateTo(AppDetailView.tabLogs),
        icon: const Icon(PiccoloIcons.article, size: 16),
        label: const Text('View Logs'),
      ),
    );
  }

  Widget _buildErrorBanner() {
    return StatusBanner(
      severity: BannerSeverity.error,
      title: 'App failed to start',
      message: _app!.statusMessage.isNotEmpty
          ? _app!.statusMessage
          : 'The app failed to start. Check logs or try restarting.',
      action: TextButton.icon(
        onPressed: () => _tabController.animateTo(AppDetailView.tabLogs),
        icon: const Icon(PiccoloIcons.article, size: 16),
        label: const Text('View Logs'),
      ),
    );
  }

  Widget _buildSectionTitle(String title) {
    return Padding(
      padding: const EdgeInsets.only(bottom: Spacing.md),
      child: Text(
        title,
        style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
          fontWeight: FontWeight.bold,
        ),
      ),
    );
  }

  Widget _buildAccessBadge(String access) {
    final isPublic = access == 'public';
    return Container(
      padding: const EdgeInsets.symmetric(
        horizontal: Spacing.sm,
        vertical: 2,
      ),
      decoration: BoxDecoration(
        color: isPublic
            ? PiccoloTheme.warning.withValues(alpha: 0.12)
            : PiccoloTheme.mist,
        borderRadius: BorderRadius.circular(Radii.xxs),
        border: Border.all(
          color: isPublic
              ? PiccoloTheme.warning.withValues(alpha: 0.4)
              : PiccoloTheme.hairline,
        ),
      ),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          Icon(
            isPublic ? PiccoloIcons.router : PiccoloIcons.shield,
            size: 12,
            color: isPublic ? PiccoloTheme.warning : PiccoloTheme.inkMuted,
          ),
          const SizedBox(width: 4),
          Text(
            access.toUpperCase(),
            style: TextStyle(
              fontSize: 12,
              color: isPublic ? PiccoloTheme.warning : PiccoloTheme.inkMuted,
            ),
          ),
        ],
      ),
    );
  }

  Widget _buildNetworkRow(String label, String value) {
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: Spacing.xs),
      child: Row(
        children: [
          SizedBox(
            width: 120,
            child: Text(
              label,
              style: const TextStyle(color: PiccoloTheme.inkMuted),
            ),
          ),
          Expanded(
            child: SelectableText(
              value,
              style: const TextStyle(fontFamily: 'JetBrainsMono'),
            ),
          ),
        ],
      ),
    );
  }

  /// Health-gates a remote URL then opens it in a new browser tab.
  /// Used for alternate portal URLs that can't be iframe-embedded (cross-site cookies)
  /// but should still be gated to avoid dumping users into TLS errors.
  void _healthGatedLaunchUrl(String url, ServiceEndpoint svc) {
    final health =
        (svc.name == _primaryListenerName ? _primaryHealth : null) ??
        svc.health ??
        _app!.primaryListenerHealth;
    if (health != null && !health.isOk && !health.isDegraded) {
      final fallbackUrl =
          svc.lanFallbackUrl ?? svc.lanHostUrl ?? svc.localUrl ?? '';
      unawaited(
        showDialog<void>(
          context: context,
          builder: (_) => LocalFallbackOverlay(
            health: health,
            appName: _app!.displayTitle,
            lanFallbackUrl: fallbackUrl,
            appService: widget.appService,
            desktopController: widget.desktopController,
          ),
        ),
      );
      return;
    }
    unawaited(launchUrl(Uri.parse(url)));
  }

  Widget _buildNetworkLinkRow(
    String label,
    String value, {
    required VoidCallback onTap,
    required IconData icon,
    String? tooltip,
  }) {
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: Spacing.xs),
      child: Row(
        children: [
          SizedBox(
            width: 120,
            child: Text(
              label,
              style: const TextStyle(color: PiccoloTheme.inkMuted),
            ),
          ),
          Expanded(
            child: InkWell(
              onTap: onTap,
              borderRadius: BorderRadius.circular(Radii.xxs),
              child: Padding(
                padding: const EdgeInsets.symmetric(vertical: 2),
                child: Row(
                  children: [
                    Flexible(
                      child: Text(
                        value,
                        style: const TextStyle(
                          fontFamily: 'JetBrainsMono',
                          color: PiccoloTheme.cobalt600,
                          decoration: TextDecoration.underline,
                        ),
                      ),
                    ),
                    const SizedBox(width: 6),
                    Tooltip(
                      message: tooltip ?? '',
                      child: Icon(
                        icon,
                        size: 14,
                        color: PiccoloTheme.cobalt600,
                      ),
                    ),
                  ],
                ),
              ),
            ),
          ),
        ],
      ),
    );
  }
}
