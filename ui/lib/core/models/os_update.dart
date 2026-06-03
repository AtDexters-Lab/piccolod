class OSUpdate {
  OSUpdate({
    required this.currentVersion,
    required this.availableVersion,
    required this.pending,
    required this.requiresReboot,
    required this.lastChecked,
    this.stale = false,
    this.refreshing = false,
    this.degraded = false,
    this.cacheEmpty = false,
    this.rpmUpdatesAvailable = 0,
  });

  factory OSUpdate.fromJson(Map<String, dynamic> json) {
    var rpm = 0;
    var stale = false;
    var refreshing = false;
    var degraded = false;
    var cacheEmpty = false;
    final meta = json['meta'];
    if (meta != null && meta is Map) {
      rpm = (meta['rpm_updates_available'] as int?) ?? 0;
      stale = meta['stale'] == true;
      refreshing = meta['refreshing'] == true;
      degraded = meta['degraded'] == true;
      cacheEmpty = meta['cache_empty'] == true;
    }

    return OSUpdate(
      currentVersion: (json['current_version'] as String?) ?? '',
      availableVersion: (json['available_version'] as String?) ?? '',
      pending: (json['pending'] as bool?) ?? false,
      requiresReboot: (json['requires_reboot'] as bool?) ?? false,
      lastChecked: DateTime.parse(
        (json['last_checked'] as String?) ?? DateTime.now().toIso8601String(),
      ),
      stale: stale,
      refreshing: refreshing,
      degraded: degraded,
      cacheEmpty: cacheEmpty,
      rpmUpdatesAvailable: rpm,
    );
  }

  final String currentVersion;
  final String availableVersion;
  final bool pending;
  final bool requiresReboot;
  final DateTime lastChecked;
  final bool stale;
  final bool refreshing;
  final bool degraded;
  final bool cacheEmpty;
  final int rpmUpdatesAvailable;

  bool get isUncertain => stale || refreshing || degraded || cacheEmpty;
}
