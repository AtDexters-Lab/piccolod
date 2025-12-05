class OSUpdate {
  final String currentVersion;
  final String availableVersion;
  final bool pending;
  final bool requiresReboot;
  final DateTime lastChecked;
  final int rpmUpdatesAvailable;

  OSUpdate({
    required this.currentVersion,
    required this.availableVersion,
    required this.pending,
    required this.requiresReboot,
    required this.lastChecked,
    this.rpmUpdatesAvailable = 0,
  });

  factory OSUpdate.fromJson(Map<String, dynamic> json) {
    int rpm = 0;
    if (json['meta'] != null && json['meta'] is Map) {
      rpm = json['meta']['rpm_updates_available'] ?? 0;
    }

    return OSUpdate(
      currentVersion: json['current_version'] ?? '',
      availableVersion: json['available_version'] ?? '',
      pending: json['pending'] ?? false,
      requiresReboot: json['requires_reboot'] ?? false,
      lastChecked: DateTime.parse(json['last_checked'] ?? DateTime.now().toIso8601String()),
      rpmUpdatesAvailable: rpm,
    );
  }
}
