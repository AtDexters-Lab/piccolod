// Models for LAN device discovery.

import 'package:piccolo_os/core/utils/string_utils.dart';

class DiscoveredPeer {

  DiscoveredPeer({
    required this.hostname,
    required this.machineId,
    this.ipv4,
    this.ipv6,
    this.model,
    this.version,
    this.online = true,
  });

  factory DiscoveredPeer.fromJson(Map<String, dynamic> json) {
    return DiscoveredPeer(
      hostname: (json['hostname'] as String?) ?? '',
      machineId: (json['machine_id'] as String?) ?? '',
      ipv4: json['ipv4'] as String?,
      ipv6: json['ipv6'] as String?,
      model: json['model'] as String?,
      version: json['version'] as String?,
      online: (json['online'] as bool?) ?? true,
    );
  }
  final String hostname;
  final String machineId;
  final String? ipv4;
  final String? ipv6;
  final String? model;
  final String? version;
  final bool online;

  String get url => 'http://$hostname';

  String get httpsUrl => 'https://$hostname';

  String get displayName => formatHostnameDisplayName(hostname);
}

class NetworkPeersResponse {

  NetworkPeersResponse({required this.peers, this.self});

  factory NetworkPeersResponse.fromJson(Map<String, dynamic> json) {
    return NetworkPeersResponse(
      self: json['self'] != null ? NetworkSelf.fromJson(json['self'] as Map<String, dynamic>) : null,
      peers: (json['peers'] as List<dynamic>?)
              ?.whereType<Map<dynamic, dynamic>>()
              .map((e) => DiscoveredPeer.fromJson(Map<String, dynamic>.from(e)))
              .toList() ??
          [],
    );
  }
  final NetworkSelf? self;
  final List<DiscoveredPeer> peers;
}

class NetworkSelf {

  NetworkSelf({
    required this.hostname,
    required this.specificHostname,
    required this.machineId,
    required this.isGatewayLeader, this.model,
    this.version,
  });

  factory NetworkSelf.fromJson(Map<String, dynamic> json) {
    return NetworkSelf(
      hostname: (json['hostname'] as String?) ?? '',
      specificHostname: (json['specific_hostname'] as String?) ?? (json['hostname'] as String?) ?? '',
      machineId: (json['machine_id'] as String?) ?? '',
      model: json['model'] as String?,
      version: json['version'] as String?,
      isGatewayLeader: (json['is_gateway_leader'] as bool?) ?? false,
    );
  }
  final String hostname;
  final String specificHostname;
  final String machineId;
  final String? model;
  final String? version;
  final bool isGatewayLeader;

  String get httpsUrl => 'https://${specificHostname.isNotEmpty ? specificHostname : hostname}';

  /// Display name using the specific hostname format.
  String get displayName => formatHostnameDisplayName(specificHostname);
}
