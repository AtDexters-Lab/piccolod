import 'package:piccolo_os/core/models/identity_models.dart';
import 'package:piccolo_os/core/services/api_client.dart';

class IdentityService {
  final ApiClient _api = ApiClient();

  Future<IdentityStatus> getStatus() async {
    final response =
        await _api.get('/api/v1/identity') as Map<String, dynamic>;
    return IdentityStatus.fromJson(response);
  }

  Future<Map<String, dynamic>> enroll() async {
    final response =
        await _api.post('/api/v1/identity/enroll') as Map<String, dynamic>;
    return response;
  }

  Future<void> enable() async {
    await _api.post('/api/v1/identity/enable');
  }

  Future<void> disable() async {
    await _api.post('/api/v1/identity/disable');
  }

  Future<void> setHostname(String hostname) async {
    await _api.post('/api/v1/identity/hostname', body: {
      'hostname': hostname,
    });
  }

  Future<void> setNamekUrl(String url) async {
    await _api.post('/api/v1/identity/namek-url', body: {
      'url': url,
    });
  }
}
