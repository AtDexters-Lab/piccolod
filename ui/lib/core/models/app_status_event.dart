/// Event emitted when an app's status changes.
class AppStatusEvent {
  final String app;
  final String status;
  final String? prevStatus;
  final String? message;
  final String? timestamp;

  const AppStatusEvent({
    required this.app,
    required this.status,
    this.prevStatus,
    this.message,
    this.timestamp,
  });

  factory AppStatusEvent.fromJson(Map<String, dynamic> json) {
    return AppStatusEvent(
      app: (json['app'] ?? '').toString(),
      status: (json['status'] ?? '').toString(),
      prevStatus: json['prev_status']?.toString(),
      message: json['message']?.toString(),
      timestamp: json['timestamp']?.toString(),
    );
  }

  /// Status values
  static const statusInstalled = 'installed';
  static const statusUninstalled = 'uninstalled';
  static const statusRunning = 'running';
  static const statusStopped = 'stopped';
  static const statusError = 'error';
  static const statusStarting = 'starting';

  bool get isInstalled => status == statusInstalled;
  bool get isUninstalled => status == statusUninstalled;
  bool get isRunning => status == statusRunning;
  bool get isStopped => status == statusStopped;
  bool get isError => status == statusError;
  bool get isStarting => status == statusStarting;
}
