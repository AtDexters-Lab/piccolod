class OSUpdate {

  OSUpdate({
    required this.currentVersion,
    required this.availableVersion,
    required this.pending,
    required this.requiresReboot,
    required this.lastChecked,
    this.rpmUpdatesAvailable = 0,
  });

  factory OSUpdate.fromJson(Map<String, dynamic> json) {
    var rpm = 0;
    final meta = json['meta'];
    if (meta != null && meta is Map) {
      rpm = (meta['rpm_updates_available'] as int?) ?? 0;
    }

    return OSUpdate(
      currentVersion: (json['current_version'] as String?) ?? '',
      availableVersion: (json['available_version'] as String?) ?? '',
      pending: (json['pending'] as bool?) ?? false,
      requiresReboot: (json['requires_reboot'] as bool?) ?? false,
      lastChecked: DateTime.parse((json['last_checked'] as String?) ?? DateTime.now().toIso8601String()),
      rpmUpdatesAvailable: rpm,
    );
  }
  final String currentVersion;
  final String availableVersion;
  final bool pending;
  final bool requiresReboot;
  final DateTime lastChecked;
  final int rpmUpdatesAvailable;
}
