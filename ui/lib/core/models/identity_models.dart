class IdentityStatus {

  IdentityStatus({
    required this.enabled,
    required this.available,
    required this.enrolled,
    required this.suspended,
    this.deviceId,
    this.hostname,
    this.baseDomain,
    this.customHostname,
    this.identityClass,
    this.nexusEndpoints = const [],
    this.namekUrl,
  });

  factory IdentityStatus.fromJson(Map<String, dynamic> json) {
    return IdentityStatus(
      enabled: (json['enabled'] as bool?) ?? false,
      available: (json['available'] as bool?) ?? false,
      enrolled: (json['enrolled'] as bool?) ?? false,
      suspended: (json['suspended'] as bool?) ?? false,
      deviceId: json['device_id'] as String?,
      hostname: json['hostname'] as String?,
      baseDomain: json['base_domain'] as String?,
      customHostname: json['custom_hostname'] as String?,
      identityClass: json['identity_class'] as String?,
      nexusEndpoints: (json['nexus_endpoints'] as List<dynamic>?)
              ?.map((e) => e.toString())
              .toList() ??
          [],
      namekUrl: json['namek_url'] as String?,
    );
  }
  final bool enabled;
  final bool available;
  final bool enrolled;
  final bool suspended;
  final String? deviceId;
  final String? hostname;
  final String? baseDomain;
  final String? customHostname;
  final String? identityClass;
  final List<String> nexusEndpoints;
  final String? namekUrl;

  /// The resolved FQDN: customHostname.baseDomain if set, else hostname.
  String? get resolvedHostname {
    if (customHostname != null &&
        customHostname!.isNotEmpty &&
        baseDomain != null &&
        baseDomain!.isNotEmpty) {
      return '$customHostname.$baseDomain';
    }
    return hostname;
  }

  /// Computed state matching backend's Status().State logic.
  String get state {
    if (suspended) return 'suspended';
    if (!enabled) return 'disabled';
    if (!enrolled) return 'not_enrolled';
    return 'active';
  }
}
