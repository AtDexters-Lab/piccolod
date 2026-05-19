import 'package:piccolo_os/core/models/app_status_event.dart';
import 'package:piccolo_os/core/models/listener_health.dart';

class App {

  App({
    required this.id,
    required this.name,
    required this.image,
    required this.type,
    required this.status, this.mode = '',
    this.statusMessage = '',
    this.volumes = const [],
    this.environment = const {},
    this.containerId,
    this.definition = const {},
    this.primaryListenerHealth,
    this.catalogSource = '',
  });

  factory App.fromJson(Map<String, dynamic> json) {
    final instanceId = (json['instance_id'] ?? json['name'] ?? json['id'] ?? '')
        .toString();

    // Definition is now nested - extract it (with fallback for backward compatibility)
    final rawDefinition = json['definition'];
    final def = rawDefinition is Map
        ? Map<String, dynamic>.from(rawDefinition)
        : <String, dynamic>{};

    // Image, type, environment come from definition (fallback to primary service)
    final rawServices = def['services'];
    final services = rawServices is Map
        ? Map<String, dynamic>.from(rawServices)
        : null;
    var primaryService = (def['primary_service'] ?? '').toString();
    if (primaryService.isEmpty) {
      primaryService = 'main';
    }
    Map<String, dynamic>? primarySvc;
    if (services != null) {
      final svc = services[primaryService];
      if (svc is Map) {
        primarySvc = Map<String, dynamic>.from(svc);
      } else if (services.length == 1) {
        final only = services.values.first;
        if (only is Map) {
          primarySvc = Map<String, dynamic>.from(only);
        }
      }
    }

    var image = (def['image'] ?? json['image'] ?? '').toString();
    if (image.isEmpty && primarySvc != null) {
      image = (primarySvc['image'] ?? '').toString();
    }
    final type = (def['type'] ?? json['type'] ?? 'user').toString();
    final rawEnvSource = def['environment'] ?? json['environment'];
    var environment = rawEnvSource is Map
        ? rawEnvSource.map((k, v) => MapEntry(k.toString(), v.toString()))
        : <String, String>{};
    if (environment.isEmpty && primarySvc != null) {
      final rawEnv = primarySvc['environment'];
      if (rawEnv is Map) {
        environment = rawEnv.map((k, v) => MapEntry(k.toString(), v.toString()));
      }
    }

    // Mode comes from x-piccolo extensions in definition
    var mode = '';
    final rawExtensions = def['x-piccolo'];
    final extensions = rawExtensions is Map
        ? Map<String, dynamic>.from(rawExtensions)
        : null;
    if (extensions != null) {
      mode = (extensions['mode'] ?? '').toString();
    } else {
      // Fallback for backward compatibility
      mode = (json['mode'] ?? '').toString();
    }

    // Volumes come from definition.storage.volumes
    var volumes = <AppVolume>[];
    final rawStorage = def['storage'];
    final storage = rawStorage is Map
        ? Map<String, dynamic>.from(rawStorage)
        : null;
    if (storage != null) {
      final rawVolumeList = storage['volumes'];
      final volumeList = rawVolumeList is List<dynamic> ? rawVolumeList : null;
      if (volumeList != null) {
        volumes = volumeList
            .whereType<Map<dynamic, dynamic>>()
            .map((e) => AppVolume.fromJson(Map<String, dynamic>.from(e)))
            .toList();
      }
    } else if (json['volumes'] is List<dynamic>) {
      // Fallback for backward compatibility
      volumes = (json['volumes'] as List<dynamic>)
          .whereType<Map<dynamic, dynamic>>()
          .map((e) => AppVolume.fromJson(Map<String, dynamic>.from(e)))
          .toList();
    }

    final rawHealth = json['primary_listener_health'];
    final primaryHealth = rawHealth is Map
        ? ListenerHealth.fromJson(Map<String, dynamic>.from(rawHealth))
        : null;

    // Catalog source tracks which catalog item this app was installed from
    final catalogSource = (json['catalog_source'] ?? '').toString();

    return App(
      id: instanceId,
      name: instanceId,
      image: image,
      type: type,
      mode: mode,
      status: (json['status'] as String?) ?? 'unknown',
      statusMessage: (json['status_message'] ?? '').toString(),
      volumes: volumes,
      environment: environment,
      containerId: json['container_id'] as String?,
      definition: def,
      primaryListenerHealth: primaryHealth,
      catalogSource: catalogSource,
    );
  }
  final String id;
  final String name;
  final String image;
  final String type;
  final String mode;
  final String status;
  final List<AppVolume> volumes;
  final Map<String, String> environment;
  final String? containerId;
  final Map<String, dynamic> definition;
  final ListenerHealth? primaryListenerHealth;
  final String catalogSource; // Tracks which catalog item this app was installed from
  final String statusMessage; // Transient status context (e.g., "Re-pulling base image")

  bool get isRunning => status.toLowerCase() == AppStatusEvent.statusRunning;
  bool get isStopped =>
      status.toLowerCase() == AppStatusEvent.statusStopped ||
      status.toLowerCase() == 'created';
  bool get isError => status.toLowerCase() == AppStatusEvent.statusError;
  bool get isStarting => status.toLowerCase() == AppStatusEvent.statusStarting;
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

  /// RFC 20260130: App identity is the instanceID (name), which comes from
  /// the primary listener name (service mode) or workspace_name (workspace mode).
  String get displayTitle => name;

  /// Returns a copy of this App with a new status value and optional message.
  App copyWithStatus(String newStatus, {String? statusMessage}) {
    return App(
      id: id,
      name: name,
      image: image,
      type: type,
      mode: mode,
      status: newStatus,
      statusMessage: statusMessage ?? '',
      volumes: volumes,
      environment: environment,
      containerId: containerId,
      definition: definition,
      primaryListenerHealth: primaryListenerHealth,
      catalogSource: catalogSource,
    );
  }
}

class AppDetail {

  const AppDetail({
    required this.app,
    this.listeners = const [],
    this.containers = const [],
    this.snapshotAvailable = false,
  });
  final App app;
  final List<ServiceEndpoint> listeners;
  final List<AppContainerStatus> containers;
  final bool snapshotAvailable;
}

class AppContainerStatus {

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
  final String service;
  final String containerId;
  final bool running;
}

class AppVolume {

  AppVolume({
    required this.containerPath,
    required this.hostPath,
    required this.sizeLimit,
  });

  factory AppVolume.fromJson(Map<String, dynamic> json) {
    return AppVolume(
      containerPath: (json['container'] as String?) ?? '',
      hostPath: (json['host'] as String?) ?? '',
      sizeLimit: (json['size_limit'] as String?) ?? '',
    );
  }
  final String containerPath;
  final String hostPath;
  final String sizeLimit;
}

class AppListener {

  AppListener({
    required this.name,
    required this.guestPort,
    this.flow = 'tcp',
    this.protocol = 'raw',
    this.remotePorts = const [],
    this.middleware = const [],
    this.auth,
    this.portClaim,
  });

  factory AppListener.fromServiceEndpoint(ServiceEndpoint ep) {
    return AppListener(
      name: ep.name,
      guestPort: ep.guestPort,
      flow: ep.flow,
      protocol: ep.protocol,
      remotePorts: ep.remotePorts,
      middleware: ep.middleware,
      auth: ep.auth,
      portClaim: ep.portClaim,
    );
  }
  final String name;
  final int guestPort;
  final String flow;
  final String protocol;
  final List<int> remotePorts;
  final List<dynamic> middleware;
  final Map<String, dynamic>? auth;
  final int? portClaim;

  Map<String, dynamic> toJson() {
    return {
      'name': name,
      'guest_port': guestPort,
      'flow': flow,
      'protocol': protocol,
      'remote_ports': remotePorts,
      'protocol_middleware': middleware,
      if (auth != null) 'auth': auth,
      if (portClaim != null) 'port_claim': portClaim,
    };
  }
}

/// Classifies the access level of a listener based on its auth configuration.
String classifyAccess(Map<String, dynamic>? auth) {
  if (auth == null) return 'private';
  final rules = auth['rules'];
  if (rules is! List || rules.isEmpty) return 'private';
  if (rules.length == 1) {
    final rule = rules[0];
    if (rule is Map &&
        rule['path'] == '/' &&
        rule['type'] == 'prefix') {
      if (rule['strategy'] == 'public') return 'public';
      if (rule['strategy'] == 'protected') return 'private';
    }
  }
  return 'custom';
}

/// Returns the canonical auth payload for a fully-public listener.
Map<String, dynamic> publicAuthPayload() => {
  'rules': [
    {'path': '/', 'type': 'prefix', 'strategy': 'public'},
  ],
};

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
    this.lanHostUrl,
    this.lanFallbackUrl,
    this.lanPortUrl,
    this.primary = false,
    this.health,
    this.middleware = const [],
    this.auth,
    this.portClaim,
    this.remoteHosts = const [],
    this.derivedHostLabel,
  });

  factory ServiceEndpoint.fromJson(Map<String, dynamic> json) {
    final rawHealth = json['health'];
    final endpointHealth = rawHealth is Map
        ? ListenerHealth.fromJson(Map<String, dynamic>.from(rawHealth))
        : null;

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
      flow: (json['flow'] as String?) ?? 'tcp',
      protocol: (json['protocol'] as String?) ?? 'raw',
      localUrl: json['local_url'] as String?,
      lanHostUrl: json['lan_host_url'] as String?,
      lanFallbackUrl: json['lan_fallback_url'] as String?,
      lanPortUrl: json['lan_port_url'] as String?,
      primary: json['primary'] == true,
      health: endpointHealth,
      middleware: (json['middleware'] as List<dynamic>?) ?? [],
      auth: json['auth'] is Map
          ? Map<String, dynamic>.from(json['auth'] as Map)
          : null,
      portClaim: json['port_claim'] as int?,
      remoteHosts: json['remote_hosts'] is List
          ? (json['remote_hosts'] as List).whereType<String>().toList()
          : [],
      derivedHostLabel: json['derived_host_label'] as String?,
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
  final String? lanHostUrl;
  final String? lanFallbackUrl;
  final String? lanPortUrl;
  final bool primary;
  final ListenerHealth? health;
  final List<dynamic> middleware;
  final Map<String, dynamic>? auth;
  final int? portClaim;
  final List<String> remoteHosts;
  final String? derivedHostLabel;

  // Helper to get the Remote URL (if enabled)
  String? get remoteUrl {
    if (remoteHost == null || remoteHost!.isEmpty) return null;
    return 'https://$remoteHost';
  }

  // All remote URLs across all active portals (derived from remoteHosts, computed once)
  late final List<String> remoteUrls = remoteHosts.map((h) => 'https://$h').toList();
}

class CatalogItem {

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
      name: (json['name'] as String?) ?? '',
      description: (json['description'] as String?) ?? '',
      icon: json['icon'] as String?,
      version: (json['version'] as String?) ?? '',
      category: (json['category'] as String?) ?? 'Uncategorized',
      compatibility: json['compatibility'] as String?,
      maintainer: json['maintainer'] as String?,
      tags: (json['tags'] as List<dynamic>?)?.whereType<String>().toList() ?? [],
      sourceUrl: json['source_url'] as String?,
      template: json['template'] as String?,
    );
  }
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
}

class CatalogResponse {

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
              ?.whereType<Map<dynamic, dynamic>>()
              .map((e) => CatalogItem.fromJson(Map<String, dynamic>.from(e)))
              .toList() ??
          [],
      page: (json['page'] as int?) ?? 1,
      pageSize: (json['page_size'] as int?) ?? 20,
      total: (json['total'] as int?) ?? 0,
      totalPages: (json['total_pages'] as int?) ?? 0,
    );
  }
  final List<CatalogItem> apps;
  final int page;
  final int pageSize;
  final int total;
  final int totalPages;
}

/// Represents a container image search result from Docker Hub or other registries.
class ImageSearchResult {

  ImageSearchResult({
    required this.name,
    required this.description,
    required this.stars,
    required this.official,
    required this.index,
  });

  factory ImageSearchResult.fromJson(Map<String, dynamic> json) {
    return ImageSearchResult(
      name: (json['name'] as String?) ?? '',
      description: (json['description'] as String?) ?? '',
      stars: (json['stars'] as int?) ?? 0,
      official: (json['official'] as bool?) ?? false,
      index: (json['index'] as String?) ?? 'docker.io',
    );
  }
  final String name;
  final String description;
  final int stars;
  final bool official;
  final String index;

  /// Returns the full image name including registry index.
  String get fullName {
    if (index.isEmpty || index == 'docker.io') {
      return name;
    }
    return '$index/$name';
  }
}
