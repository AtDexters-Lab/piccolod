class Session {

  Session({
    required this.authenticated,
    required this.user,
    required this.volumesLocked, required this.passwordStale, required this.recoveryStale,
    this.expiresAt,
    this.hasPasskey = false,
    this.passkeyCount = 0,
    this.mustRegisterPasskey = false,
  });

  factory Session.fromJson(Map<String, dynamic> json) {
    return Session(
      authenticated: (json['authenticated'] as bool?) ?? false,
      user: (json['user'] as String?) ?? '',
      expiresAt: json['expires_at'] != null ? DateTime.parse(json['expires_at'] as String) : null,
      volumesLocked: (json['volumes_locked'] as bool?) ?? false,
      passwordStale: (json['password_stale'] as bool?) ?? false,
      recoveryStale: (json['recovery_stale'] as bool?) ?? false,
      hasPasskey: (json['has_passkey'] as bool?) ?? false,
      passkeyCount: (json['passkey_count'] as int?) ?? 0,
      mustRegisterPasskey: (json['must_register_passkey'] as bool?) ?? false,
    );
  }
  final bool authenticated;
  final String user;
  final DateTime? expiresAt;
  final bool volumesLocked;
  final bool passwordStale;
  final bool recoveryStale;
  final bool hasPasskey;
  final int passkeyCount;
  final bool mustRegisterPasskey;
}
