class SystemTimezone {
  const SystemTimezone({required this.timezone, required this.isSet});

  factory SystemTimezone.fromJson(Map<String, dynamic> json) {
    return SystemTimezone(
      timezone: (json['timezone'] as String?) ?? 'Etc/UTC',
      isSet: (json['is_set'] as bool?) ?? false,
    );
  }

  final String timezone;

  /// True when the timezone has been configured away from the UTC default.
  /// Drives the auto-unlock scheduler's pre-fire safety gate on the backend
  /// and the "Configure timezone" hint on the Settings → System page.
  final bool isSet;
}

/// Curated IANA-zone list for the Settings dropdown. Hand-picked to keep the
/// UX simple — the backend accepts any valid IANA zone (validated via
/// time.LoadLocation), so power users can still curl /system/timezone with
/// any zone they want.
const List<String> commonTimezones = [
  'Etc/UTC',
  'America/Anchorage',
  'America/Chicago',
  'America/Denver',
  'America/Halifax',
  'America/Los_Angeles',
  'America/Mexico_City',
  'America/New_York',
  'America/Phoenix',
  'America/Sao_Paulo',
  'America/Toronto',
  'America/Vancouver',
  'Asia/Bangkok',
  'Asia/Dubai',
  'Asia/Hong_Kong',
  'Asia/Jakarta',
  'Asia/Karachi',
  'Asia/Kolkata',
  'Asia/Manila',
  'Asia/Seoul',
  'Asia/Shanghai',
  'Asia/Singapore',
  'Asia/Taipei',
  'Asia/Tokyo',
  'Australia/Adelaide',
  'Australia/Brisbane',
  'Australia/Melbourne',
  'Australia/Perth',
  'Australia/Sydney',
  'Europe/Amsterdam',
  'Europe/Berlin',
  'Europe/Brussels',
  'Europe/Bucharest',
  'Europe/Dublin',
  'Europe/Helsinki',
  'Europe/Istanbul',
  'Europe/Lisbon',
  'Europe/London',
  'Europe/Madrid',
  'Europe/Moscow',
  'Europe/Oslo',
  'Europe/Paris',
  'Europe/Prague',
  'Europe/Rome',
  'Europe/Stockholm',
  'Europe/Vienna',
  'Europe/Warsaw',
  'Europe/Zurich',
  'Pacific/Auckland',
  'Pacific/Honolulu',
];
