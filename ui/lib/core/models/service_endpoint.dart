class ServiceEndpoint {

  ServiceEndpoint({
    required this.app,
    required this.name,
    required this.guestPort,
    required this.hostPort,
    required this.publicPort,
    required this.flow, required this.protocol, this.remotePorts = const [],
    this.remoteHost,
    this.localUrl,
    this.derivedHostLabel,
    this.portClaim,
  });

  factory ServiceEndpoint.fromJson(Map<String, dynamic> json) {
    return ServiceEndpoint(
      app: (json['app'] as String?) ?? '',
      name: (json['name'] as String?) ?? '',
      guestPort: (json['guest_port'] as int?) ?? 0,
      hostPort: (json['host_port'] as int?) ?? 0,
      publicPort: (json['public_port'] as int?) ?? 0,
      remotePorts: json['remote_ports'] is List
          ? (json['remote_ports'] as List).whereType<int>().toList()
          : [],
      remoteHost: json['remote_host'] as String?,
      flow: (json['flow'] as String?) ?? '',
      protocol: (json['protocol'] as String?) ?? '',
      localUrl: json['local_url'] as String?,
      derivedHostLabel: json['derived_host_label'] as String?,
      portClaim: json['port_claim'] as int?,
    );
  }
  final String app;
  final String name;
  final int guestPort;
  final int hostPort;
  final int publicPort;
  final List<int> remotePorts;
  final String? remoteHost;
  final String flow;
  final String protocol;
  final String? localUrl;
  final String? derivedHostLabel;
  final int? portClaim;
}
