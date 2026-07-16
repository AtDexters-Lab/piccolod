/// Models for the WiFi management API (/api/v1/wifi/*).
library;

import 'package:flutter/widgets.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class NetworkInterfaceStatus {
  NetworkInterfaceStatus({
    required this.kind,
    required this.iface,
    required this.role,
    required this.linkUp,
    required this.hasIp,
    required this.ipv4,
    required this.ipv6,
  });

  factory NetworkInterfaceStatus.fromJson(Map<String, dynamic> json) {
    return NetworkInterfaceStatus(
      kind: json['kind'] as String? ?? 'unknown',
      iface: json['iface'] as String? ?? '',
      role: json['role'] as String? ?? 'unknown',
      linkUp: json['link_up'] as bool? ?? false,
      hasIp: json['has_ip'] as bool? ?? false,
      ipv4: (json['ipv4'] as List<dynamic>? ?? const <dynamic>[])
          .whereType<String>()
          .toList(),
      ipv6: (json['ipv6'] as List<dynamic>? ?? const <dynamic>[])
          .whereType<String>()
          .toList(),
    );
  }

  final String kind;
  final String iface;
  final String role;
  final bool linkUp;
  final bool hasIp;
  final List<String> ipv4;
  final List<String> ipv6;
}

class NetworkStatus {
  NetworkStatus({
    required this.available,
    required this.activeUplink,
    required this.connectivity,
    required this.interfaces,
    required this.apActive,
    required this.hasSavedNetwork,
    this.activeUplinkIface,
    this.ssid,
    this.signalDbm,
    this.signalTier,
    this.frequencyMhz,
    this.band,
    this.ipAddress,
    this.savedSsid,
  });

  factory NetworkStatus.fromJson(Map<String, dynamic> json) {
    final rawInterfaces =
        json['interfaces'] as List<dynamic>? ?? const <dynamic>[];
    return NetworkStatus(
      available: json['wifi_available'] as bool? ?? false,
      activeUplink: json['active_uplink'] as String? ?? 'none',
      activeUplinkIface: json['active_uplink_iface'] as String?,
      connectivity: json['connectivity'] as String? ?? 'unknown',
      interfaces: rawInterfaces
          .whereType<Map<dynamic, dynamic>>()
          .map(
            (item) => NetworkInterfaceStatus.fromJson(
              Map<String, dynamic>.from(item),
            ),
          )
          .toList(),
      apActive: json['ap_active'] as bool? ?? false,
      ssid: json['ssid'] as String?,
      signalDbm: json['signal_dbm'] as int?,
      signalTier: json['signal_tier'] as String?,
      frequencyMhz: json['frequency_mhz'] as int?,
      band: json['band'] as String?,
      ipAddress: json['ip_address'] as String?,
      hasSavedNetwork: json['has_saved_network'] as bool? ?? false,
      savedSsid: json['saved_ssid'] as String?,
    );
  }
  final bool available;
  final String activeUplink;
  final String? activeUplinkIface;
  final String connectivity;
  final List<NetworkInterfaceStatus> interfaces;
  final bool apActive;
  final String? ssid;
  final int? signalDbm;
  final String? signalTier;
  final int? frequencyMhz;
  final String? band;
  final String? ipAddress;
  final bool hasSavedNetwork;
  final String? savedSsid;

  bool get hasUsableConnectivity =>
      connectivity == 'full' || connectivity == 'limited';
  bool get isWifiConnected =>
      !apActive && activeUplink == 'wifi' && hasUsableConnectivity;
  bool get isEthernet =>
      !apActive && activeUplink == 'ethernet' && hasUsableConnectivity;
  bool get isLimitedConnectivity =>
      !apActive && connectivity == 'limited' && activeUplink != 'none';
  bool get isPortal => !apActive && connectivity == 'portal';
  bool get isUnknown => !apActive && connectivity == 'unknown';
  bool get isReconnecting =>
      !apActive &&
      connectivity == 'none' &&
      activeUplink == 'none' &&
      available &&
      hasSavedNetwork;
  bool get isDisconnected =>
      !apActive && connectivity == 'none' && !isReconnecting;
  bool get isAPMode => apActive;
}

class WifiNetwork {
  WifiNetwork({
    required this.ssid,
    required this.security,
    required this.signalDbm,
    required this.signalTier,
    required this.frequencyMhz,
    required this.band,
  });

  factory WifiNetwork.fromJson(Map<String, dynamic> json) {
    return WifiNetwork(
      ssid: json['ssid'] as String? ?? '',
      security: json['security'] as String? ?? 'unknown',
      signalDbm: json['signal_dbm'] as int? ?? -90,
      signalTier: json['signal_tier'] as String? ?? 'poor',
      frequencyMhz: json['frequency_mhz'] as int? ?? 0,
      band: json['band'] as String? ?? '',
    );
  }
  final String ssid;
  final String security;
  final int signalDbm;
  final String signalTier;
  final int frequencyMhz;
  final String band;

  bool get isOpen {
    final sec = security.toLowerCase();
    return sec == 'open' || sec == 'none' || sec == '';
  }

  IconData get signalIcon => switch (signalTier) {
    'good' => PiccoloIcons.wifi,
    'fair' => PiccoloIcons.wifiMedium,
    'weak' => PiccoloIcons.wifiLow,
    _ => PiccoloIcons.wifiNone,
  };

  Color get signalColor => switch (signalTier) {
    'good' || 'fair' => PiccoloTheme.success,
    'weak' => PiccoloTheme.warning,
    _ => PiccoloTheme.critical,
  };
}

class WifiConnectResult {
  WifiConnectResult({
    required this.success,
    this.error,
  });

  factory WifiConnectResult.fromJson(Map<String, dynamic> json) {
    return WifiConnectResult(
      success: json['success'] as bool? ?? false,
      error: json['error'] as String?,
    );
  }
  final bool success;
  final String? error;
}

class WifiAPStatus {
  WifiAPStatus({
    required this.active,
    required this.suppressed,
    required this.clients,
    this.ssid,
  });

  factory WifiAPStatus.fromJson(Map<String, dynamic> json) {
    return WifiAPStatus(
      active: json['active'] as bool? ?? false,
      ssid: json['ssid'] as String?,
      suppressed: json['suppressed'] as bool? ?? false,
      clients: json['clients'] as int? ?? 0,
    );
  }
  final bool active;
  final String? ssid;
  final bool suppressed;
  final int clients;
}
