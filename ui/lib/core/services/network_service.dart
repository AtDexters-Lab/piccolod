import '../services/api_client.dart';
import '../models/network_models.dart';

class NetworkService {
  final ApiClient _client;

  NetworkService(this._client);

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
