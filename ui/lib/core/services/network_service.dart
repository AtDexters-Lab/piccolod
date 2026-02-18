import 'package:piccolo_os/core/models/network_models.dart';
import 'package:piccolo_os/core/services/api_client.dart';

class NetworkService {

  NetworkService(this._client);
  final ApiClient _client;

  /// Fetches discovered Piccolo peers on the LAN.
  /// Returns empty list if accessed remotely (via Nexus proxy).
  Future<NetworkPeersResponse> getPeers() async {
    final data = await _client.get('/api/v1/network/peers');

    Map<String, dynamic> json;
    if (data is Map<String, dynamic>) {
      json = data;
    } else {
      return NetworkPeersResponse(peers: []);
    }

    return NetworkPeersResponse.fromJson(json);
  }
}
