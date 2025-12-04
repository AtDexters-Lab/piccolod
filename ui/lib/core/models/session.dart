class Session {
  final bool authenticated;
  final String user;
  final DateTime? expiresAt;
  final bool volumesLocked;
  final bool passwordStale;
  final bool recoveryStale;

  Session({
    required this.authenticated,
    required this.user,
    this.expiresAt,
    required this.volumesLocked,
    required this.passwordStale,
    required this.recoveryStale,
  });

  factory Session.fromJson(Map<String, dynamic> json) {
    return Session(
      authenticated: json['authenticated'] ?? false,
      user: json['user'] ?? '',
      expiresAt: json['expires_at'] != null ? DateTime.parse(json['expires_at']) : null,
      volumesLocked: json['volumes_locked'] ?? false,
      passwordStale: json['password_stale'] ?? false,
      recoveryStale: json['recovery_stale'] ?? false,
    );
  }
}
