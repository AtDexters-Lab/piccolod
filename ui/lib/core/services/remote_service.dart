import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/core/models/remote_models.dart';
import 'package:piccolo_os/core/services/api_client.dart';

class RemoteService {
  final ApiClient _api = ApiClient();

  Future<List<ServiceEndpoint>> getServices() async {
    final response = await _api.get('/api/v1/services') as Map<String, dynamic>;
    final list = (response['services'] as List<dynamic>?) ?? <dynamic>[];
    return list
        .whereType<Map<dynamic, dynamic>>()
        .map((e) => ServiceEndpoint.fromJson(Map<String, dynamic>.from(e)))
        .toList();
  }

  Future<RemoteStatus> getStatus() async {
    final response = await _api.get('/api/v1/remote/status') as Map<String, dynamic>;
    return RemoteStatus.fromJson(response);
  }

  Future<void> configure(Map<String, dynamic> config) async {
    await _api.post('/api/v1/remote/configure', body: config);
  }

  Future<void> disable() async {
    await _api.post('/api/v1/remote/disable');
  }

  Future<String> rotateCredentials() async {
    final response = await _api.post('/api/v1/remote/rotate') as Map<String, dynamic>;
    return (response['device_secret'] as String?) ?? '';
  }

  Future<List<RemoteAlias>> getAliases() async {
    final response = await _api.get('/api/v1/remote/aliases') as Map<String, dynamic>;
    final list = (response['aliases'] as List<dynamic>?) ?? <dynamic>[];
    return list
        .whereType<Map<dynamic, dynamic>>()
        .map((e) => RemoteAlias.fromJson(Map<String, dynamic>.from(e)))
        .toList();
  }

  Future<RemoteAlias> addAlias(String hostname, String listener) async {
    final response = await _api.post('/api/v1/remote/aliases', body: {
      'hostname': hostname,
      'listener': listener,
    }) as Map<String, dynamic>;
    return RemoteAlias.fromJson(response);
  }

  Future<void> removeAlias(String id) async {
    await _api.delete('/api/v1/remote/aliases/$id');
  }

  Future<List<RemoteCertificate>> getCertificates() async {
    final response = await _api.get('/api/v1/remote/certificates') as Map<String, dynamic>;
    final list = (response['certificates'] as List<dynamic>?) ?? <dynamic>[];
    return list
        .whereType<Map<dynamic, dynamic>>()
        .map((e) => RemoteCertificate.fromJson(Map<String, dynamic>.from(e)))
        .toList();
  }

  Future<void> renewCertificate(String id) async {
    await _api.post('/api/v1/remote/certificates/$id/renew');
  }

  Future<List<RemoteEvent>> getEvents() async {
    final response = await _api.get('/api/v1/remote/events') as Map<String, dynamic>;
    final list = (response['events'] as List<dynamic>?) ?? <dynamic>[];
    return list
        .whereType<Map<dynamic, dynamic>>()
        .map((e) => RemoteEvent.fromJson(Map<String, dynamic>.from(e)))
        .toList();
  }

  Future<RemoteGuideInfo> getNexusGuide() async {
    final response = await _api.get('/api/v1/remote/nexus-guide') as Map<String, dynamic>;
    return RemoteGuideInfo.fromJson(response);
  }

  Future<void> verifyNexusGuide(Map<String, dynamic> data) async {
    await _api.post('/api/v1/remote/nexus-guide/verify', body: data);
  }
}
