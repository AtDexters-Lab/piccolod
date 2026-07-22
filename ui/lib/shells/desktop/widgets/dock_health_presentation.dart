enum DockHealthLevel { healthy, error, degraded, recovering }

class DockHealthPresentation {
  const DockHealthPresentation({required this.label, required this.message});

  final String label;
  final String message;
}

DockHealthPresentation resolveDockHealthPresentation({
  required bool connected,
  required bool snapshotsPending,
  required DockHealthLevel aggregateLevel,
  bool taskCritical = false,
  bool taskWarning = false,
  bool taskMonitorUnavailable = false,
  bool automaticRecoveryBackoff = false,
  bool unknownAppObservation = false,
}) {
  if (!connected) {
    return const DockHealthPresentation(
      label: 'Offline',
      message: 'Connection lost - Reconnecting...',
    );
  }
  if (snapshotsPending) {
    return const DockHealthPresentation(
      label: 'Checking',
      message: 'Connected - Waiting for health data...',
    );
  }

  final label = switch (aggregateLevel) {
    DockHealthLevel.error => 'Error',
    DockHealthLevel.degraded => 'Degraded',
    DockHealthLevel.recovering => 'Recovering',
    DockHealthLevel.healthy => 'Healthy',
  };

  if (taskCritical) {
    return DockHealthPresentation(
      label: label,
      message: 'Piccolo is recovering.',
    );
  }
  if (taskMonitorUnavailable) {
    return DockHealthPresentation(
      label: label,
      message:
          'Piccolo cannot confirm system health right now. It will keep trying.',
    );
  }
  if (taskWarning) {
    return DockHealthPresentation(
      label: label,
      message:
          'Piccolo is under heavy load. Some actions may be temporarily unavailable.',
    );
  }
  if (automaticRecoveryBackoff) {
    return DockHealthPresentation(
      label: label,
      message: 'Piccolo is recovering. It will retry automatically.',
    );
  }
  if (unknownAppObservation) {
    return DockHealthPresentation(
      label: label,
      message:
          'Piccolo cannot confirm some app statuses right now. Last known status is shown.',
    );
  }

  final message = switch (aggregateLevel) {
    DockHealthLevel.error => 'System Error - Check app details',
    DockHealthLevel.degraded => 'System Degraded - Action may be required',
    DockHealthLevel.recovering =>
      'System Recovering - Auto-healing in progress',
    DockHealthLevel.healthy => 'System Healthy',
  };
  return DockHealthPresentation(label: label, message: message);
}
