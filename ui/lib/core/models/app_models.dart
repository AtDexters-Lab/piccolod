import 'listener_health.dart';

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
  final Map<String, dynamic> definition;
  final ListenerHealth? primaryListenerHealth;

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
    this.definition = const {},
    this.primaryListenerHealth,
  });

  factory App.fromJson(Map<String, dynamic> json) {
    final instanceId = (json['instance_id'] ?? json['name'] ?? json['id'] ?? '')
        .toString();
    final displayName = (json['display_name'] ?? '').toString();

    // Definition is now nested - extract it (with fallback for backward compatibility)
    final rawDefinition = json['definition'];
    final def = rawDefinition is Map
        ? Map<String, dynamic>.from(rawDefinition)
        : <String, dynamic>{};

    // App name comes from definition.name, with fallbacks
    final appName = (def['name'] ?? json['app_name'] ?? json['name'] ?? '')
        .toString();

    // Image, type, environment come from definition (fallback to primary service)
    final services = def['services'] is Map ? Map.from(def['services']) : null;
    String primaryService = (def['primary_service'] ?? '').toString();
    if (primaryService.isEmpty) {
      primaryService = 'main';
    }
    Map? primarySvc;
    if (services != null) {
      final svc = services[primaryService];
      if (svc is Map) {
        primarySvc = svc;
      } else if (services.length == 1) {
        final only = services.values.first;
        if (only is Map) {
          primarySvc = only;
        }
      }
    }

    String image = (def['image'] ?? json['image'] ?? '').toString();
    if (image.isEmpty && primarySvc != null) {
      image = (primarySvc['image'] ?? '').toString();
    }
    final type = (def['type'] ?? json['type'] ?? 'user').toString();
    Map<String, String> environment = Map<String, String>.from(
      def['environment'] ?? json['environment'] ?? {},
    );
    if (environment.isEmpty && primarySvc != null) {
      final rawEnv = primarySvc['environment'];
      if (rawEnv is Map) {
        environment = rawEnv.map((k, v) => MapEntry(k.toString(), v.toString()));
      }
    }

    // Mode comes from x-piccolo extensions in definition
    String mode = '';
    final extensions = def['x-piccolo'] as Map<String, dynamic>?;
    if (extensions != null) {
      mode = (extensions['mode'] ?? '').toString();
    } else {
      // Fallback for backward compatibility
      mode = (json['mode'] ?? '').toString();
    }

    // Volumes come from definition.storage.volumes
    List<AppVolume> volumes = [];
    final storage = def['storage'] as Map<String, dynamic>?;
    if (storage != null) {
      final volumeList = storage['volumes'] as List<dynamic>?;
      if (volumeList != null) {
        volumes = volumeList.map((e) => AppVolume.fromJson(e)).toList();
      }
    } else if (json['volumes'] != null) {
      // Fallback for backward compatibility
      volumes = (json['volumes'] as List<dynamic>)
          .map((e) => AppVolume.fromJson(e))
          .toList();
    }

    final rawHealth = json['primary_listener_health'];
    final primaryHealth = rawHealth is Map
        ? ListenerHealth.fromJson(Map<String, dynamic>.from(rawHealth))
        : null;

    return App(
      id: instanceId,
      name: instanceId,
      appName: appName,
      displayName: displayName,
      image: image,
      type: type,
      mode: mode,
      status: json['status'] ?? 'unknown',
      volumes: volumes,
      environment: environment,
      containerId: json['container_id'],
      definition: def,
      primaryListenerHealth: primaryHealth,
    );
  }

  bool get isRunning => status.toLowerCase() == 'running';
  bool get isStopped =>
      status.toLowerCase() == 'stopped' || status.toLowerCase() == 'created';
  bool get isError => status.toLowerCase() == 'error';
  bool get isWorkspace => mode.toLowerCase() == 'workspace';

  Map<String, String> environmentForService(String? serviceName) {
    final svc = serviceName?.trim();
    if (svc == null || svc.isEmpty) return environment;

    final services = definition['services'];
    if (services is! Map) return environment;

    final rawSvc = services[svc];
    if (rawSvc is! Map) return environment;

    final rawEnv = rawSvc['environment'];
    if (rawEnv is! Map) return environment;

    return rawEnv.map((k, v) => MapEntry(k.toString(), v.toString()));
  }

  String get displayTitle {
    if (displayName.isNotEmpty) return displayName;
    if (appName.isNotEmpty) return appName;
    return name;
  }
}

class AppDetail {
  final App app;
  final List<ServiceEndpoint> listeners;
  final List<AppContainerStatus> containers;

  const AppDetail({
    required this.app,
    this.listeners = const [],
    this.containers = const [],
  });
}

class AppContainerStatus {
  final String service;
  final String containerId;
  final bool running;

  const AppContainerStatus({
    required this.service,
    required this.containerId,
    required this.running,
  });

  factory AppContainerStatus.fromJson(Map<String, dynamic> json) {
    return AppContainerStatus(
      service: (json['service'] ?? '').toString(),
      containerId: (json['container_id'] ?? '').toString(),
      running: json['running'] == true,
    );
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
  final String? lanHostUrl;
  final String? lanFallbackUrl;
  final String? lanPortUrl;
  final bool primary;
  final ListenerHealth? health;
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
    this.lanHostUrl,
    this.lanFallbackUrl,
    this.lanPortUrl,
    this.primary = false,
    this.health,
    this.middleware = const [],
  });

  factory ServiceEndpoint.fromJson(Map<String, dynamic> json) {
    final rawHealth = json['health'];
    final endpointHealth = rawHealth is Map
        ? ListenerHealth.fromJson(Map<String, dynamic>.from(rawHealth))
        : null;

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
      lanHostUrl: json['lan_host_url'],
      lanFallbackUrl: json['lan_fallback_url'],
      lanPortUrl: json['lan_port_url'],
      primary: json['primary'] == true,
      health: endpointHealth,
      middleware: json['middleware'] ?? [],
    );
  }

  // Helper to get the Remote URL (if enabled)
  String? get remoteUrl {
    if (remoteHost == null || remoteHost!.isEmpty) return null;
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

/// Represents a container image search result from Docker Hub or other registries.
class ImageSearchResult {
  final String name;
  final String description;
  final int stars;
  final bool official;
  final String index;

  ImageSearchResult({
    required this.name,
    required this.description,
    required this.stars,
    required this.official,
    required this.index,
  });

  factory ImageSearchResult.fromJson(Map<String, dynamic> json) {
    return ImageSearchResult(
      name: json['name'] ?? '',
      description: json['description'] ?? '',
      stars: json['stars'] ?? 0,
      official: json['official'] ?? false,
      index: json['index'] ?? 'docker.io',
    );
  }

  /// Returns the full image name including registry index.
  String get fullName {
    if (index.isEmpty || index == 'docker.io') {
      return name;
    }
    return '$index/$name';
  }
}
