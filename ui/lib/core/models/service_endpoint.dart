class ServiceEndpoint {
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

  ServiceEndpoint({
    required this.app,
    required this.name,
    required this.guestPort,
    required this.hostPort,
    required this.publicPort,
    this.remotePorts = const [],
    this.remoteHost,
    required this.flow,
    required this.protocol,
    this.localUrl,
  });

  factory ServiceEndpoint.fromJson(Map<String, dynamic> json) {
    return ServiceEndpoint(
      app: json['app'] ?? '',
      name: json['name'] ?? '',
      guestPort: json['guest_port'] ?? 0,
      hostPort: json['host_port'] ?? 0,
      publicPort: json['public_port'] ?? 0,
      remotePorts: (json['remote_ports'] as List<dynamic>?)?.map((e) => e as int).toList() ?? [],
      remoteHost: json['remote_host'],
      flow: json['flow'] ?? '',
      protocol: json['protocol'] ?? '',
      localUrl: json['local_url'],
    );
  }
}
