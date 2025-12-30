class TaskProgressEvent {
  final String taskId;
  final String taskType;
  final String? instanceId;
  final String phase;
  final int progress;
  final String message;
  final bool isComplete;
  final String? error;
  final DateTime? timestamp;
  final Map<String, dynamic>? metadata;

  const TaskProgressEvent({
    required this.taskId,
    required this.taskType,
    required this.phase,
    required this.progress,
    required this.message,
    required this.isComplete,
    this.instanceId,
    this.error,
    this.timestamp,
    this.metadata,
  });

  factory TaskProgressEvent.fromJson(Map<String, dynamic> json) {
    return TaskProgressEvent(
      taskId: json['task_id'] ?? '',
      taskType: json['task_type'] ?? '',
      instanceId: json['instance_id'],
      phase: json['phase'] ?? '',
      progress: json['progress'] is int ? json['progress'] : -1,
      message: json['message'] ?? '',
      isComplete: json['is_complete'] == true,
      error: json['error'],
      timestamp: json['timestamp'] != null
          ? DateTime.tryParse(json['timestamp'])
          : null,
      metadata: json['metadata'] is Map
          ? Map<String, dynamic>.from(json['metadata'])
          : null,
    );
  }
}

