
class App {
  final String id;
  final String name;
  final String appName;
  final String displayName;
  final String image;
  final String type;
  final String mode;
  final String status;
  final List<AppVolume> volumes;
  final Map<String, String> environment;
  final String? containerId;

  App({
    required this.id,
    required this.name,
    this.appName = '',
    this.displayName = '',
    required this.image,
    required this.type,
    this.mode = '',
    required this.status,
    this.volumes = const [],
    this.environment = const {},
    this.containerId,
  });

  factory App.fromJson(Map<String, dynamic> json) {
    final instanceId = (json['instance_id'] ?? json['name'] ?? json['id'] ?? '')
        .toString();
    final appName = (json['app_name'] ?? json['name'] ?? '').toString();
    final displayName = (json['display_name'] ?? '').toString();

    return App(
      id: instanceId,
      name: instanceId,
      appName: appName,
      displayName: displayName,
      image: json['image'] ?? '',
      type: json['type'] ?? 'user',
      mode: json['mode'] ?? '',
      status: json['status'] ?? 'unknown',
      volumes:
          (json['volumes'] as List<dynamic>?)
              ?.map((e) => AppVolume.fromJson(e))
              .toList() ??
          [],
      environment: Map<String, String>.from(json['environment'] ?? {}),
      containerId: json['container_id'],
    );
  }

  bool get isRunning => status.toLowerCase() == 'running';
  bool get isStopped =>
      status.toLowerCase() == 'stopped' || status.toLowerCase() == 'created';
  bool get isError => status.toLowerCase() == 'error';
  bool get isWorkspace => mode.toLowerCase() == 'workspace';

  String get displayTitle {
    if (displayName.isNotEmpty) return displayName;
    if (appName.isNotEmpty) return appName;
    return name;
  }
}

class AppVolume {
  final String containerPath;
  final String hostPath;
  final String sizeLimit;

  AppVolume({
    required this.containerPath,
    required this.hostPath,
    required this.sizeLimit,
  });

  factory AppVolume.fromJson(Map<String, dynamic> json) {
    return AppVolume(
      containerPath: json['container'] ?? '',
      hostPath: json['host'] ?? '',
      sizeLimit: json['size_limit'] ?? '',
    );
  }
}

class AppListener {
  final String name;
  final int guestPort;
  final String flow;
  final String protocol;
  final List<int> remotePorts;
  final List<dynamic> middleware;

  AppListener({
    required this.name,
    required this.guestPort,
    this.flow = 'tcp',
    this.protocol = 'raw',
    this.remotePorts = const [],
    this.middleware = const [],
  });

  factory AppListener.fromServiceEndpoint(ServiceEndpoint ep) {
    return AppListener(
      name: ep.name,
      guestPort: ep.guestPort,
      flow: ep.flow,
      protocol: ep.protocol,
      remotePorts: ep.remotePorts,
      middleware: ep.middleware,
    );
  }

  Map<String, dynamic> toJson() {
    return {
      'name': name,
      'guest_port': guestPort,
      'flow': flow,
      'protocol': protocol,
      'remote_ports': remotePorts,
      'protocol_middleware': middleware,
    };
  }
}

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
  final List<dynamic> middleware;

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
    this.middleware = const [],
  });

  factory ServiceEndpoint.fromJson(Map<String, dynamic> json) {
    return ServiceEndpoint(
      app: json['app'] ?? '',
      name: json['name'] ?? '',
      guestPort: json['guest_port'] ?? 0,
      hostPort: json['host_port'] ?? 0,
      publicPort: json['public_port'] ?? 0,
      remotePorts: (json['remote_ports'] as List<dynamic>?)?.cast<int>() ?? [],
      remoteHost: json['remote_host'],
      flow: json['flow'] ?? 'tcp',
      protocol: json['protocol'] ?? 'raw',
      localUrl: json['local_url'],
      middleware: json['middleware'] ?? [],
    );
  }

  // Helper to get the Remote URL (if enabled)
  String? get remoteUrl {
    if (remoteHost == null || remoteHost!.isEmpty) return null;
    // Nexus endpoints are secured by default.
    // If remotePorts contains 443, we imply https without port.
    // If it contains 80, http.
    // Simplification: Nexus is HTTPS-first.
    return 'https://$remoteHost';
  }
}

class CatalogItem {
  final String name;
  final String description;
  final String? icon;
  final String version;
  final String category;
  final String? compatibility;
  final String? maintainer;
  final List<String> tags;
  final String? sourceUrl;
  final String? template; // Optional inline YAML snippet

  CatalogItem({
    required this.name,
    required this.description,
    this.icon,
    this.version = '',
    this.category = 'Uncategorized',
    this.compatibility,
    this.maintainer,
    this.tags = const [],
    this.sourceUrl,
    this.template,
  });

  factory CatalogItem.fromJson(Map<String, dynamic> json) {
    return CatalogItem(
      name: json['name'] ?? '',
      description: json['description'] ?? '',
      icon: json['icon'],
      version: json['version'] ?? '',
      category: json['category'] ?? 'Uncategorized',
      compatibility: json['compatibility'],
      maintainer: json['maintainer'],
      tags: (json['tags'] as List<dynamic>?)?.cast<String>() ?? [],
      sourceUrl: json['source_url'],
      template: json['template'],
    );
  }
}

class CatalogResponse {
  final List<CatalogItem> apps;
  final int page;
  final int pageSize;
  final int total;
  final int totalPages;

  CatalogResponse({
    required this.apps,
    required this.page,
    required this.pageSize,
    required this.total,
    required this.totalPages,
  });

  factory CatalogResponse.fromJson(Map<String, dynamic> json) {
    return CatalogResponse(
      apps:
          (json['apps'] as List<dynamic>?)
              ?.map((e) => CatalogItem.fromJson(e))
              .toList() ??
          [],
      page: json['page'] ?? 1,
      pageSize: json['page_size'] ?? 20,
      total: json['total'] ?? 0,
      totalPages: json['total_pages'] ?? 0,
    );
  }
}

class AppValidationResult {
  final bool valid;
  final String? error;

  AppValidationResult({required this.valid, this.error});

  factory AppValidationResult.fromJson(Map<String, dynamic> json) {
    // Some validation endpoints might return a 400 with 'error' field in body
    // or 200 with { "valid": true/false }
    return AppValidationResult(
      valid: json['valid'] ?? false,
      error:
          json['error'], // If backend returns error detail in 200 OK structure
    );
  }
}
