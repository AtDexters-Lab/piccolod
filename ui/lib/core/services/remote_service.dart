import 'api_client.dart';
import '../models/remote_models.dart';
import '../models/service_endpoint.dart';

class RemoteService {
  final ApiClient _api = ApiClient();

  Future<List<ServiceEndpoint>> getServices() async {
    final response = await _api.get('/api/v1/services');
    final List<dynamic> list = response['services'] ?? [];
    return list.map((e) => ServiceEndpoint.fromJson(e)).toList();
  }

  Future<RemoteStatus> getStatus() async {
    final response = await _api.get('/api/v1/remote/status');
    return RemoteStatus.fromJson(response);
  }

  Future<void> configure(Map<String, dynamic> config) async {
    await _api.post('/api/v1/remote/configure', body: config);
  }

  Future<void> disable() async {
    await _api.post('/api/v1/remote/disable');
  }

  Future<String> rotateCredentials() async {
    final response = await _api.post('/api/v1/remote/rotate');
    return response['device_secret'] ?? '';
  }

  Future<List<RemotePreflightCheck>> runPreflight([Map<String, dynamic>? config]) async {
    final response = await _api.post('/api/v1/remote/preflight', body: config);
    final List<dynamic> checks = response['checks'] ?? [];
    return checks.map((e) => RemotePreflightCheck.fromJson(e)).toList();
  }

  Future<List<RemoteAlias>> getAliases() async {
    final response = await _api.get('/api/v1/remote/aliases');
    final List<dynamic> list = response['aliases'] ?? [];
    return list.map((e) => RemoteAlias.fromJson(e)).toList();
  }

  Future<RemoteAlias> addAlias(String hostname, String listener) async {
    final response = await _api.post('/api/v1/remote/aliases', body: {
      'hostname': hostname,
      'listener': listener,
    });
    return RemoteAlias.fromJson(response);
  }

  Future<void> removeAlias(String id) async {
    await _api.delete('/api/v1/remote/aliases/$id');
  }

  Future<List<RemoteCertificate>> getCertificates() async {
    final response = await _api.get('/api/v1/remote/certificates');
    final List<dynamic> list = response['certificates'] ?? [];
    return list.map((e) => RemoteCertificate.fromJson(e)).toList();
  }

  Future<void> renewCertificate(String id) async {
    await _api.post('/api/v1/remote/certificates/$id/renew');
  }

  Future<List<RemoteEvent>> getEvents() async {
    final response = await _api.get('/api/v1/remote/events');
    final List<dynamic> list = response['events'] ?? [];
    return list.map((e) => RemoteEvent.fromJson(e)).toList();
  }

  Future<List<RemoteDNSProvider>> getDNSProviders() async {
    final response = await _api.get('/api/v1/remote/dns/providers');
    final List<dynamic> list = response['providers'] ?? [];
    return list.map((e) => RemoteDNSProvider.fromJson(e)).toList();
  }

  Future<RemoteGuideInfo> getNexusGuide() async {
    final response = await _api.get('/api/v1/remote/nexus-guide');
    return RemoteGuideInfo.fromJson(response);
  }

  Future<void> verifyNexusGuide(Map<String, dynamic> data) async {
    await _api.post('/api/v1/remote/nexus-guide/verify', body: data);
  }
}
