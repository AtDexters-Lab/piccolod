import '../services/api_client.dart';
import '../models/app_models.dart';

class AppService {
  final ApiClient _client;

  AppService(this._client);

  // --- Catalog ---

  Future<List<CatalogItem>> getCatalog() async {
    final data = await _client.get('/api/v1/catalog');
    // Expected structure: { data: { apps: [...] } } or { apps: [...] }
    
    var list = [];
    if (data is Map) {
      if (data['data'] != null && data['data'] is Map && data['data']['apps'] != null) {
        list = data['data']['apps'];
      } else if (data['apps'] != null) {
        list = data['apps'];
      }
    }
    
    return list.map((e) => CatalogItem.fromJson(e)).toList();
  }

  Future<String?> getCatalogTemplate(String name) async {
    final data = await _client.get('/api/v1/catalog/$name/template');
    return data is String ? data : null;
  }

  // --- Installed Apps ---

  Future<List<App>> getApps() async {
    final data = await _client.get('/api/v1/apps');
    // Expected: { data: [...] }
    final List<dynamic> list = data['data'] ?? [];
    return list.map((e) => App.fromJson(e)).toList();
  }

  Future<App> getAppDetail(String name) async {
    final data = await _client.get('/api/v1/apps/$name');
    // Expected: { data: { app: {...}, services: [...] } }
    return App.fromJson(data['data']['app']);
  }

  Future<List<ServiceEndpoint>> getAppServices(String name) async {
    try {
      final data = await _client.get('/api/v1/apps/$name');
      final List<dynamic> list = data['data']['services'] ?? [];
      return list.map((e) => ServiceEndpoint.fromJson(e)).toList();
    } catch (_) {
      return [];
    }
  }

  // --- Lifecycle ---

  Future<void> startApp(String name) async {
    await _client.post('/api/v1/apps/$name/start', body: {});
  }

  Future<void> stopApp(String name) async {
    await _client.post('/api/v1/apps/$name/stop', body: {});
  }

  Future<void> uninstallApp(String name, {bool purge = false}) async {
    // Query params not supported in delete? ApiClient.delete supports body.
    // Need to append query to path.
    await _client.delete('/api/v1/apps/$name?purge=$purge');
  }

  // --- Install / Validate ---

  Future<AppValidationResult> validateManifest(String yamlContent) async {
    try {
      final data = await _client.postRaw(
        '/api/v1/apps/validate',
        body: yamlContent,
        contentType: 'application/x-yaml',
      );
      
      // Expected: { data: { valid: true }, message: "valid" }
      final valid = data['data']?['valid'] ?? false;
      return AppValidationResult(valid: valid);
    } catch (e) {
      if (e is ApiException) {
        // Validation error often comes as 400 with details
        return AppValidationResult(valid: false, error: e.message);
      }
      return AppValidationResult(valid: false, error: e.toString());
    }
  }

  Future<App> installApp(String yamlContent) async {
    final data = await _client.postRaw(
      '/api/v1/apps',
      body: yamlContent,
      contentType: 'application/x-yaml',
    );
    
    // Expected: { data: {App}, message: ... }
    return App.fromJson(data['data']);
  }
}
