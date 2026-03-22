enum PortalSource { namek, selfHosted }

class Portal {

  Portal({
    required this.source,
    required this.hostname,
    required this.state,
    this.endpoints = const [],
  });
  final PortalSource source;
  final String hostname;
  final String state;
  final List<String> endpoints;
}

class RemoteStatus {

  RemoteStatus({
    required this.enabled,
    required this.state,
    required this.solver,
    this.endpoint,
    this.portalHostname,
    this.latencyMs,
    this.lastHandshake,
    this.nextRenewal,
    this.issuer,
    this.expiresAt,
    this.warnings = const [],
    this.guideVerifiedAt,
    this.listeners = const [],
    this.aliases = const [],
    this.certificates = const [],
  });

  factory RemoteStatus.fromJson(Map<String, dynamic> json) {
    return RemoteStatus(
      enabled: (json['enabled'] as bool?) ?? false,
      state: (json['state'] as String?) ?? 'unknown',
      solver: (json['solver'] as String?) ?? 'unknown',
      endpoint: json['endpoint'] as String?,
      portalHostname: json['portal_hostname'] as String?,
      latencyMs: json['latency_ms'] as int?,
      lastHandshake: json['last_handshake'] != null ? DateTime.parse(json['last_handshake'] as String) : null,
      nextRenewal: json['next_renewal'] != null ? DateTime.parse(json['next_renewal'] as String) : null,
      issuer: json['issuer'] as String?,
      expiresAt: json['expires_at'] != null ? DateTime.parse(json['expires_at'] as String) : null,
      warnings: (json['warnings'] as List<dynamic>?)?.map((e) => e.toString()).toList() ?? [],
      guideVerifiedAt: json['guide_verified_at'] != null ? DateTime.parse(json['guide_verified_at'] as String) : null,
      listeners: (json['listeners'] as List<dynamic>?)?.whereType<Map<dynamic, dynamic>>().map((e) => RemoteListener.fromJson(Map<String, dynamic>.from(e))).toList() ?? [],
      aliases: (json['aliases'] as List<dynamic>?)?.whereType<Map<dynamic, dynamic>>().map((e) => RemoteAlias.fromJson(Map<String, dynamic>.from(e))).toList() ?? [],
      certificates: (json['certificates'] as List<dynamic>?)?.whereType<Map<dynamic, dynamic>>().map((e) => RemoteCertificate.fromJson(Map<String, dynamic>.from(e))).toList() ?? [],
    );
  }
  final bool enabled;
  final String state;
  final String solver;
  final String? endpoint;
  final String? portalHostname;
  final int? latencyMs;
  final DateTime? lastHandshake;
  final DateTime? nextRenewal;
  final String? issuer;
  final DateTime? expiresAt;
  final List<String> warnings;
  final DateTime? guideVerifiedAt;
  final List<RemoteListener> listeners;
  final List<RemoteAlias> aliases;
  final List<RemoteCertificate> certificates;
}

class RemoteListener {

  RemoteListener({required this.name, required this.remoteHost});

  factory RemoteListener.fromJson(Map<String, dynamic> json) {
    return RemoteListener(
      name: (json['name'] as String?) ?? '',
      remoteHost: (json['remote_host'] as String?) ?? '',
    );
  }
  final String name;
  final String remoteHost;
}

class RemoteAlias {

  RemoteAlias({
    required this.id,
    required this.hostname,
    required this.listener,
    required this.status,
    this.lastChecked,
    this.message,
  });

  factory RemoteAlias.fromJson(Map<String, dynamic> json) {
    return RemoteAlias(
      id: (json['id'] as String?) ?? '',
      hostname: (json['hostname'] as String?) ?? '',
      listener: (json['listener'] as String?) ?? '',
      status: (json['status'] as String?) ?? 'unknown',
      lastChecked: json['last_checked'] != null ? DateTime.parse(json['last_checked'] as String) : null,
      message: json['message'] as String?,
    );
  }
  final String id;
  final String hostname;
  final String listener;
  final String status;
  final DateTime? lastChecked;
  final String? message;
}

class RemoteCertificate {

  RemoteCertificate({
    required this.id,
    required this.domains,
    this.solver,
    this.issuedAt,
    this.expiresAt,
    this.nextRenewal,
    this.retryAt,
    this.status,
    this.failureReason,
  });

  factory RemoteCertificate.fromJson(Map<String, dynamic> json) {
    return RemoteCertificate(
      id: (json['id'] as String?) ?? '',
      domains: (json['domains'] as List<dynamic>?)?.map((e) => e.toString()).toList() ?? [],
      solver: json['solver'] as String?,
      issuedAt: json['issued_at'] != null ? DateTime.parse(json['issued_at'] as String) : null,
      expiresAt: json['expires_at'] != null ? DateTime.parse(json['expires_at'] as String) : null,
      nextRenewal: json['next_renewal'] != null ? DateTime.parse(json['next_renewal'] as String) : null,
      retryAt: json['retry_at'] != null ? DateTime.parse(json['retry_at'] as String) : null,
      status: json['status'] as String?,
      failureReason: json['failure_reason'] as String?,
    );
  }
  final String id;
  final List<String> domains;
  final String? solver;
  final DateTime? issuedAt;
  final DateTime? expiresAt;
  final DateTime? nextRenewal;
  final DateTime? retryAt;
  final String? status;
  final String? failureReason;
}

class RemotePreflightCheck {

  RemotePreflightCheck({
    required this.name,
    required this.status,
    this.detail,
    this.nextStep,
  });

  factory RemotePreflightCheck.fromJson(Map<String, dynamic> json) {
    return RemotePreflightCheck(
      name: (json['name'] as String?) ?? '',
      status: (json['status'] as String?) ?? 'unknown',
      detail: json['detail'] as String?,
      nextStep: json['next_step'] as String?,
    );
  }
  final String name;
  final String status; // pass, warn, fail
  final String? detail;
  final String? nextStep;
}

class RemoteEvent {

  RemoteEvent({
    required this.ts,
    required this.level,
    required this.source,
    required this.message,
    this.nextStep,
  });

  factory RemoteEvent.fromJson(Map<String, dynamic> json) {
    return RemoteEvent(
      ts: DateTime.parse(json['ts'] as String),
      level: (json['level'] as String?) ?? 'info',
      source: (json['source'] as String?) ?? 'unknown',
      message: (json['message'] as String?) ?? '',
      nextStep: json['next_step'] as String?,
    );
  }
  final DateTime ts;
  final String level;
  final String source;
  final String message;
  final String? nextStep;
}

class RemoteGuideInfo {

  RemoteGuideInfo({
    required this.command,
    required this.requirements,
    required this.notes,
    required this.docsUrl,
    this.verifiedAt,
  });

  factory RemoteGuideInfo.fromJson(Map<String, dynamic> json) {
    return RemoteGuideInfo(
      command: (json['command'] as String?) ?? '',
      requirements: (json['requirements'] as List<dynamic>?)?.map((e) => e.toString()).toList() ?? [],
      notes: (json['notes'] as List<dynamic>?)?.map((e) => e.toString()).toList() ?? [],
      docsUrl: (json['docs_url'] as String?) ?? '',
      verifiedAt: json['verified_at'] != null ? DateTime.parse(json['verified_at'] as String) : null,
    );
  }
  final String command;
  final List<String> requirements;
  final List<String> notes;
  final String docsUrl;
  final DateTime? verifiedAt;
}
