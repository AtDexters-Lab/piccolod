class RemoteStatus {
  final bool enabled;
  final String state;
  final String solver;
  final String? endpoint;
  final String? tld;
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

  RemoteStatus({
    required this.enabled,
    required this.state,
    required this.solver,
    this.endpoint,
    this.tld,
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
      enabled: json['enabled'] ?? false,
      state: json['state'] ?? 'unknown',
      solver: json['solver'] ?? 'unknown',
      endpoint: json['endpoint'],
      tld: json['tld'],
      portalHostname: json['portal_hostname'],
      latencyMs: json['latency_ms'],
      lastHandshake: json['last_handshake'] != null ? DateTime.parse(json['last_handshake']) : null,
      nextRenewal: json['next_renewal'] != null ? DateTime.parse(json['next_renewal']) : null,
      issuer: json['issuer'],
      expiresAt: json['expires_at'] != null ? DateTime.parse(json['expires_at']) : null,
      warnings: (json['warnings'] as List<dynamic>?)?.map((e) => e.toString()).toList() ?? [],
      guideVerifiedAt: json['guide_verified_at'] != null ? DateTime.parse(json['guide_verified_at']) : null,
      listeners: (json['listeners'] as List<dynamic>?)?.map((e) => RemoteListener.fromJson(e)).toList() ?? [],
      aliases: (json['aliases'] as List<dynamic>?)?.map((e) => RemoteAlias.fromJson(e)).toList() ?? [],
      certificates: (json['certificates'] as List<dynamic>?)?.map((e) => RemoteCertificate.fromJson(e)).toList() ?? [],
    );
  }
}

class RemoteListener {
  final String name;
  final String remoteHost;

  RemoteListener({required this.name, required this.remoteHost});

  factory RemoteListener.fromJson(Map<String, dynamic> json) {
    return RemoteListener(
      name: json['name'] ?? '',
      remoteHost: json['remote_host'] ?? '',
    );
  }
}

class RemoteAlias {
  final String id;
  final String hostname;
  final String listener;
  final String status;
  final DateTime? lastChecked;
  final String? message;

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
      id: json['id'] ?? '',
      hostname: json['hostname'] ?? '',
      listener: json['listener'] ?? '',
      status: json['status'] ?? 'unknown',
      lastChecked: json['last_checked'] != null ? DateTime.parse(json['last_checked']) : null,
      message: json['message'],
    );
  }
}

class RemoteCertificate {
  final String id;
  final List<String> domains;
  final String? solver;
  final DateTime? issuedAt;
  final DateTime? expiresAt;
  final DateTime? nextRenewal;
  final String? status;
  final String? failureReason;

  RemoteCertificate({
    required this.id,
    required this.domains,
    this.solver,
    this.issuedAt,
    this.expiresAt,
    this.nextRenewal,
    this.status,
    this.failureReason,
  });

  factory RemoteCertificate.fromJson(Map<String, dynamic> json) {
    return RemoteCertificate(
      id: json['id'] ?? '',
      domains: (json['domains'] as List<dynamic>?)?.map((e) => e.toString()).toList() ?? [],
      solver: json['solver'],
      issuedAt: json['issued_at'] != null ? DateTime.parse(json['issued_at']) : null,
      expiresAt: json['expires_at'] != null ? DateTime.parse(json['expires_at']) : null,
      nextRenewal: json['next_renewal'] != null ? DateTime.parse(json['next_renewal']) : null,
      status: json['status'],
      failureReason: json['failure_reason'],
    );
  }
}

class RemotePreflightCheck {
  final String name;
  final String status; // pass, warn, fail
  final String? detail;
  final String? nextStep;

  RemotePreflightCheck({
    required this.name,
    required this.status,
    this.detail,
    this.nextStep,
  });

  factory RemotePreflightCheck.fromJson(Map<String, dynamic> json) {
    return RemotePreflightCheck(
      name: json['name'] ?? '',
      status: json['status'] ?? 'unknown',
      detail: json['detail'],
      nextStep: json['next_step'],
    );
  }
}

class RemoteEvent {
  final DateTime ts;
  final String level;
  final String source;
  final String message;
  final String? nextStep;

  RemoteEvent({
    required this.ts,
    required this.level,
    required this.source,
    required this.message,
    this.nextStep,
  });

  factory RemoteEvent.fromJson(Map<String, dynamic> json) {
    return RemoteEvent(
      ts: DateTime.parse(json['ts']),
      level: json['level'] ?? 'info',
      source: json['source'] ?? 'unknown',
      message: json['message'] ?? '',
      nextStep: json['next_step'],
    );
  }
}

class RemoteDNSProvider {
  final String id;
  final String name;
  final String? docsUrl;
  final List<RemoteDNSProviderField> fields;

  RemoteDNSProvider({
    required this.id,
    required this.name,
    this.docsUrl,
    required this.fields,
  });

  factory RemoteDNSProvider.fromJson(Map<String, dynamic> json) {
    return RemoteDNSProvider(
      id: json['id'] ?? '',
      name: json['name'] ?? '',
      docsUrl: json['docs_url'],
      fields: (json['fields'] as List<dynamic>?)?.map((e) => RemoteDNSProviderField.fromJson(e)).toList() ?? [],
    );
  }
}

class RemoteDNSProviderField {
  final String id;
  final String label;
  final bool secret;
  final String? placeholder;
  final String? description;

  RemoteDNSProviderField({
    required this.id,
    required this.label,
    required this.secret,
    this.placeholder,
    this.description,
  });

  factory RemoteDNSProviderField.fromJson(Map<String, dynamic> json) {
    return RemoteDNSProviderField(
      id: json['id'] ?? '',
      label: json['label'] ?? '',
      secret: json['secret'] ?? false,
      placeholder: json['placeholder'],
      description: json['description'],
    );
  }
}

class RemoteGuideInfo {
  final String command;
  final List<String> requirements;
  final List<String> notes;
  final String docsUrl;
  final DateTime? verifiedAt;

  RemoteGuideInfo({
    required this.command,
    required this.requirements,
    required this.notes,
    required this.docsUrl,
    this.verifiedAt,
  });

  factory RemoteGuideInfo.fromJson(Map<String, dynamic> json) {
    return RemoteGuideInfo(
      command: json['command'] ?? '',
      requirements: (json['requirements'] as List<dynamic>?)?.map((e) => e.toString()).toList() ?? [],
      notes: (json['notes'] as List<dynamic>?)?.map((e) => e.toString()).toList() ?? [],
      docsUrl: json['docs_url'] ?? '',
      verifiedAt: json['verified_at'] != null ? DateTime.parse(json['verified_at']) : null,
    );
  }
}
