class ResourcePressure {
  const ResourcePressure({
    required this.resource,
    required this.severity,
    required this.reasonCode,
    required this.actionTaken,
    required this.appInstanceId,
    required this.message,
    this.taskCurrent,
    this.taskLimit,
  });

  factory ResourcePressure.fromJson(Map<String, dynamic> json) {
    return ResourcePressure(
      resource: json['resource'] as String? ?? '',
      severity: json['severity'] as String? ?? '',
      reasonCode: json['reason_code'] as String? ?? '',
      actionTaken: json['action_taken'] as String? ?? '',
      appInstanceId: json['app_instance_id'] as String? ?? '',
      message: json['message'] as String? ?? '',
      taskCurrent: (json['task_current'] as num?)?.toInt(),
      taskLimit: (json['task_limit'] as num?)?.toInt(),
    );
  }

  final String resource;
  final String severity;
  final String reasonCode;
  final String actionTaken;
  final String appInstanceId;
  final String message;
  final int? taskCurrent;
  final int? taskLimit;

  bool get isTaskPressure => resource == 'tasks';
  bool get isRuntimeObservation => resource == 'runtime';
  bool get isWarning => isTaskPressure && severity == 'warn';
  bool get isCritical => isTaskPressure && severity == 'urgent';
  bool get isNormal => isTaskPressure && severity == 'ok';
  bool get isMonitorUnavailable =>
      isTaskPressure && reasonCode == 'monitor_unavailable';
  bool get isRuntimeUnknown =>
      isRuntimeObservation &&
      severity == 'warn' &&
      appInstanceId.isNotEmpty &&
      !isRecoverySuppressed;
  bool get isRecoverySuppressed =>
      isRuntimeObservation &&
      severity == 'warn' &&
      reasonCode == 'automatic_recovery_suppressed';
  bool get isRuntimeRecovered =>
      isRuntimeObservation && severity == 'ok' && appInstanceId.isNotEmpty;
}
