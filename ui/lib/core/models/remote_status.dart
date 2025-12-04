class RemoteStatus {
  final bool enabled;
  final String state;
  final String? endpoint;
  final String? tld;
  final String? portalHostname;

  RemoteStatus({
    required this.enabled,
    required this.state,
    this.endpoint,
    this.tld,
    this.portalHostname,
  });

  factory RemoteStatus.fromJson(Map<String, dynamic> json) {
    return RemoteStatus(
      enabled: json['enabled'] ?? false,
      state: json['state'] ?? 'unknown',
      endpoint: json['endpoint'],
      tld: json['tld'],
      portalHostname: json['portal_hostname'],
    );
  }
}
