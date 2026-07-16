import 'package:piccolo_os/core/models/wifi_models.dart';
import 'package:piccolo_os/core/services/api_client.dart';

class WifiService {
  WifiService(this._client);
  final ApiClient _client;

  Future<NetworkStatus> getStatus() async {
    final data = await _client.get('/api/v1/network/status');
    if (data is Map<String, dynamic>) {
      return NetworkStatus.fromJson(data);
    }
    return NetworkStatus(
      available: false,
      activeUplink: 'none',
      connectivity: 'unknown',
      interfaces: const [],
      apActive: false,
      hasSavedNetwork: false,
    );
  }

  Future<List<WifiNetwork>> scan() async {
    final data = await _client.post('/api/v1/wifi/scan');
    if (data is Map<String, dynamic>) {
      final networks = data['networks'] as List<dynamic>?;
      return networks
              ?.whereType<Map<dynamic, dynamic>>()
              .map((e) => WifiNetwork.fromJson(Map<String, dynamic>.from(e)))
              .toList() ??
          [];
    }
    return [];
  }

  Future<WifiConnectResult> connect(String ssid, String passphrase) async {
    final data = await _client.post(
      '/api/v1/wifi/connect',
      body: {
        'ssid': ssid,
        'passphrase': passphrase,
      },
    );
    if (data is Map<String, dynamic>) {
      return WifiConnectResult.fromJson(data);
    }
    return WifiConnectResult(success: false);
  }

  Future<void> disconnect() async {
    await _client.post('/api/v1/wifi/disconnect');
  }

  Future<WifiAPStatus> getAPStatus() async {
    final data = await _client.get('/api/v1/wifi/ap');
    if (data is Map<String, dynamic>) {
      return WifiAPStatus.fromJson(data);
    }
    return WifiAPStatus(active: false, suppressed: false, clients: 0);
  }

  Future<void> suppressAP(bool suppress) async {
    await _client.post(
      '/api/v1/wifi/ap/suppress',
      body: {
        'suppress': suppress,
      },
    );
  }
}
