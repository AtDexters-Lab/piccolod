/// Models for the WiFi management API (/api/v1/wifi/*).

class WifiStatus {
  final bool available;
  final String state;
  final String activeUplink;
  final String? ssid;
  final int? signalDbm;
  final String? signalTier;
  final int? frequencyMhz;
  final String? band;
  final String? ipAddress;
  final bool hasSavedNetwork;
  final String? savedSsid;

  WifiStatus({
    required this.available,
    required this.state,
    required this.activeUplink,
    this.ssid,
    this.signalDbm,
    this.signalTier,
    this.frequencyMhz,
    this.band,
    this.ipAddress,
    required this.hasSavedNetwork,
    this.savedSsid,
  });

  factory WifiStatus.fromJson(Map<String, dynamic> json) {
    return WifiStatus(
      available: json['wifi_available'] as bool? ?? false,
      state: json['state'] as String? ?? 'disconnected',
      activeUplink: json['active_uplink'] as String? ?? 'none',
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

  bool get isWifiConnected => state == 'wifi_connected';
  bool get isEthernet => state == 'ethernet';
  bool get isReconnecting => state == 'reconnecting';
  bool get isDisconnected => state == 'disconnected';
  bool get isAPMode => state == 'ap_mode';
}

class WifiNetwork {
  final String ssid;
  final String security;
  final int signalDbm;
  final String signalTier;
  final int frequencyMhz;
  final String band;

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
}

class WifiConnectResult {
  final bool success;
  final String? error;
  final String state;

  WifiConnectResult({
    required this.success,
    this.error,
    required this.state,
  });

  factory WifiConnectResult.fromJson(Map<String, dynamic> json) {
    return WifiConnectResult(
      success: json['success'] as bool? ?? false,
      error: json['error'] as String?,
      state: json['state'] as String? ?? 'disconnected',
    );
  }
}

class WifiAPStatus {
  final bool active;
  final String? ssid;
  final bool suppressed;
  final int clients;

  WifiAPStatus({
    required this.active,
    this.ssid,
    required this.suppressed,
    required this.clients,
  });

  factory WifiAPStatus.fromJson(Map<String, dynamic> json) {
    return WifiAPStatus(
      active: json['active'] as bool? ?? false,
      ssid: json['ssid'] as String?,
      suppressed: json['suppressed'] as bool? ?? false,
      clients: json['clients'] as int? ?? 0,
    );
  }
}
