// Models for LAN device discovery.

import '../utils/string_utils.dart';

class DiscoveredPeer {
  final String hostname;
  final String machineId;
  final String? ipv4;
  final String? ipv6;
  final String? model;
  final String? version;
  final bool online;

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
      hostname: json['hostname'] ?? '',
      machineId: json['machine_id'] ?? '',
      ipv4: json['ipv4'],
      ipv6: json['ipv6'],
      model: json['model'],
      version: json['version'],
      online: json['online'] ?? true,
    );
  }

  String get url => 'http://$hostname';

  String get httpsUrl => 'https://$hostname';

  String get displayName => formatHostnameDisplayName(hostname);
}

class NetworkPeersResponse {
  final NetworkSelf? self;
  final List<DiscoveredPeer> peers;

  NetworkPeersResponse({this.self, required this.peers});

  factory NetworkPeersResponse.fromJson(Map<String, dynamic> json) {
    return NetworkPeersResponse(
      self: json['self'] != null ? NetworkSelf.fromJson(json['self']) : null,
      peers: (json['peers'] as List<dynamic>?)
              ?.map((e) => DiscoveredPeer.fromJson(e as Map<String, dynamic>))
              .toList() ??
          [],
    );
  }
}

class NetworkSelf {
  final String hostname;
  final String specificHostname;
  final String machineId;
  final String? model;
  final String? version;
  final bool isGatewayLeader;

  NetworkSelf({
    required this.hostname,
    required this.specificHostname,
    required this.machineId,
    this.model,
    this.version,
    required this.isGatewayLeader,
  });

  factory NetworkSelf.fromJson(Map<String, dynamic> json) {
    return NetworkSelf(
      hostname: json['hostname'] ?? '',
      specificHostname: json['specific_hostname'] ?? json['hostname'] ?? '',
      machineId: json['machine_id'] ?? '',
      model: json['model'],
      version: json['version'],
      isGatewayLeader: json['is_gateway_leader'] ?? false,
    );
  }

  String get httpsUrl => 'https://${specificHostname.isNotEmpty ? specificHostname : hostname}';

  /// Display name using the specific hostname format.
  String get displayName => formatHostnameDisplayName(specificHostname);
}
