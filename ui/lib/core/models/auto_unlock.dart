class AutoRebootSettings {
  const AutoRebootSettings({
    required this.enabled,
    required this.windowStartHour,
    required this.windowEndHour,
  });

  factory AutoRebootSettings.fromJson(Map<String, dynamic> json) {
    return AutoRebootSettings(
      enabled: (json['enabled'] as bool?) ?? false,
      windowStartHour: (json['window_start_hour'] as int?) ?? 3,
      windowEndHour: (json['window_end_hour'] as int?) ?? 5,
    );
  }

  final bool enabled;
  final int windowStartHour;
  final int windowEndHour;
}

class AutoUnlockState {
  const AutoUnlockState({
    required this.enabled,
    required this.autoReboot,
    required this.hasOutstandingBlob,
    this.lastDepositAt,
    this.lastPickupAt,
    this.lastFailureAt,
    this.lastFailureReason = '',
  });

  factory AutoUnlockState.fromJson(Map<String, dynamic> json) {
    DateTime? parseTs(dynamic v) {
      if (v is String && v.isNotEmpty) {
        try {
          return DateTime.parse(v);
        } on Object catch (_) {
          return null;
        }
      }
      return null;
    }

    return AutoUnlockState(
      enabled: (json['enabled'] as bool?) ?? false,
      autoReboot: AutoRebootSettings.fromJson(
        (json['auto_reboot'] as Map<String, dynamic>?) ?? <String, dynamic>{},
      ),
      hasOutstandingBlob: (json['has_outstanding_blob'] as bool?) ?? false,
      lastDepositAt: parseTs(json['last_deposit_at']),
      lastPickupAt: parseTs(json['last_pickup_at']),
      lastFailureAt: parseTs(json['last_failure_at']),
      lastFailureReason: (json['last_failure_reason'] as String?) ?? '',
    );
  }

  final bool enabled;
  final AutoRebootSettings autoReboot;
  final bool hasOutstandingBlob;
  final DateTime? lastDepositAt;
  final DateTime? lastPickupAt;
  final DateTime? lastFailureAt;
  final String lastFailureReason;

  /// True when the most recent activity is a failure (no successful pickup
  /// since the last failure). Drives the Settings → Security badge.
  bool get isInFailureState {
    if (lastFailureAt == null) return false;
    if (lastPickupAt == null) return true;
    return lastFailureAt!.isAfter(lastPickupAt!);
  }
}

/// Failure-token → user-friendly string for the Settings → Security
/// auto-unlock detail (post-auth — locked-screen UI does NOT use these).
String autoUnlockFailureReasonLabel(String token) {
  switch (token) {
    case 'service_unreachable':
      return "Couldn't reach piccolospace";
    case 'service_not_ready':
      return 'Connection service not ready';
    case 'auth_failed':
      return 'Authentication with piccolospace failed';
    case 'escrow_not_found':
      return 'Stored unlock data was missing on piccolospace';
    case 'blob_corrupt':
      return 'Stored unlock data could not be used';
    case 'blob_write_failed':
      return 'Could not save unlock data on the device';
    case 'no_blob':
      return 'No unlock data was prepared (last shutdown was unclean)';
    case 'deposit_failed':
      return 'Could not save unlock data with piccolospace';
    case 'manual_unlock_first':
      return 'Password unlock happened first';
    default:
      return token.isEmpty ? 'Unknown' : token;
  }
}
