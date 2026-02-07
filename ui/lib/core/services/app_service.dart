import '../services/api_client.dart';
import '../models/app_models.dart';

class AppService {
  final ApiClient _client;

  AppService(this._client);

  Map<String, String>? _taskHeaders(String? taskId) {
    if (taskId == null || taskId.isEmpty) return null;
    return {'X-Piccolo-Task-ID': taskId};
  }

  // --- Catalog ---

  Future<CatalogResponse> getCatalog({
    int page = 1,
    int pageSize = 20,
    String? query,
    String? category,
  }) async {
    final queryParams = <String, String>{
      'page': page.toString(),
      'page_size': pageSize.toString(),
    };
    if (query != null && query.isNotEmpty) {
      queryParams['q'] = query;
    }
    if (category != null && category.isNotEmpty) {
      queryParams['category'] = category;
    }

    // ApiClient.get usually appends query params if provided, or we construct the URL
    // Assuming ApiClient.get takes a path. We'll construct the query string.
    final uri = Uri(path: '/api/v1/catalog', queryParameters: queryParams);

    final data = await _client.get(uri.toString());
    // Expected structure: { apps: [...], page: 1, ... }

    // Handle potential wrapper { data: { ... } } if existing ApiClient wraps it?
    // Based on previous code, it seemed to handle raw response or data wrapper.
    // The new backend returns JSON matching CatalogResponse directly.

    Map<String, dynamic> json;
    if (data is Map<String, dynamic>) {
      // Check if wrapped in "data"
      if (data.containsKey('data') && data['data'] is Map) {
        json = data['data'];
      } else {
        json = data;
      }
    } else {
      // Fallback/Error
      return CatalogResponse(
        apps: [],
        page: 1,
        pageSize: 20,
        total: 0,
        totalPages: 0,
      );
    }

    return CatalogResponse.fromJson(json);
  }

  Future<List<String>> getCategories() async {
    final data = await _client.get('/api/v1/catalog/categories');
    // Expected: { categories: ["CMS", "Database"] } or { data: { categories: [...] } }

    List<dynamic> list = [];
    if (data is Map) {
      if (data['data'] != null && data['data']['categories'] != null) {
        list = data['data']['categories'];
      } else if (data['categories'] != null) {
        list = data['categories'];
      }
    }
    return list.cast<String>();
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

  Future<AppDetail> getAppDetail(String name) async {
    final data = await _client.get('/api/v1/apps/$name');
    // Expected: { data: { app: {...}, listeners: [...], containers: [...] } }

    final payload = (data is Map) ? data['data'] : null;
    final json = payload is Map
        ? Map<String, dynamic>.from(payload)
        : <String, dynamic>{};

    final appJson = json['app'];
    final app = App.fromJson(
      appJson is Map ? Map<String, dynamic>.from(appJson) : <String, dynamic>{},
    );

    final rawListeners = json['listeners'] ?? json['services'];
    final listeners = (rawListeners is List ? rawListeners : const [])
        .whereType<Map>()
        .map((e) => ServiceEndpoint.fromJson(Map<String, dynamic>.from(e)))
        .toList();

    final rawContainers = json['containers'];
    final containers = (rawContainers is List ? rawContainers : const [])
        .whereType<Map>()
        .map((e) => AppContainerStatus.fromJson(Map<String, dynamic>.from(e)))
        .toList();

    return AppDetail(app: app, listeners: listeners, containers: containers);
  }

  Future<List<ServiceEndpoint>> getAppServices(String name) async {
    try {
      final detail = await getAppDetail(name);
      return detail.listeners;
    } catch (_) {
      return [];
    }
  }

  // --- Lifecycle ---

  Future<void> startApp(String name, {String? taskId}) async {
    await _client.post(
      '/api/v1/apps/$name/start',
      body: {},
      headers: _taskHeaders(taskId),
    );
  }

  Future<void> stopApp(String name, {String? taskId}) async {
    await _client.post(
      '/api/v1/apps/$name/stop',
      body: {},
      headers: _taskHeaders(taskId),
    );
  }

  Future<void> updateAppListeners(
    String name,
    List<AppListener> listeners, {
    String? taskId,
  }) async {
    await _client.patch(
      '/api/v1/apps/$name/listeners',
      body: {'listeners': listeners.map((e) => e.toJson()).toList()},
      headers: _taskHeaders(taskId),
    );
  }

  Future<void> uninstallApp(
    String name, {
    String? taskId,
  }) async {
    await _client.delete(
      '/api/v1/apps/$name',
      headers: _taskHeaders(taskId),
    );
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

  Future<App> installApp(String yamlContent, {String? taskId}) async {
    final data = await _client.postRaw(
      '/api/v1/apps',
      body: yamlContent,
      contentType: 'application/x-yaml',
      headers: _taskHeaders(taskId),
    );

    // Expected: { data: {App}, message: ... }
    return App.fromJson(data['data']);
  }

  Future<Map<String, dynamic>> getCatalogConfigure(String name) async {
    final data = await _client.get('/api/v1/catalog/$name/configure');
    // Expected: { data: { inputName: { type:..., default:..., ... } } }
    return Map<String, dynamic>.from(data['data'] ?? {});
  }

  Future<App> installAppWithInputs(
    String yamlContent,
    Map<String, dynamic> inputs, {
    String? taskId,
    String? catalogSource, // Tracks which catalog item this app was installed from
  }) async {
    final body = <String, dynamic>{
      'app_definition': yamlContent,
      'inputs': inputs,
    };
    if (catalogSource != null && catalogSource.isNotEmpty) {
      body['catalog_source'] = catalogSource;
    }

    final data = await _client.post(
      '/api/v1/apps',
      body: body,
      headers: _taskHeaders(taskId),
    );
    return App.fromJson(data['data']);
  }

  // --- Certificate Management ---

  Future<void> renewCertificate(String certId) async {
    await _client.post(
      '/api/v1/remote/certificates/$certId/renew',
      body: {},
    );
  }

  // --- Image Search (for Workspace creation) ---

  /// Searches for container images in Docker Hub and other registries.
  Future<List<ImageSearchResult>> searchImages(
    String query, {
    int limit = 25,
  }) async {
    final uri = Uri(
      path: '/api/v1/images/search',
      queryParameters: {'q': query, 'limit': limit.toString()},
    );

    final data = await _client.get(uri.toString());

    // Expected: { data: { images: [...], query: "..." } }
    Map<String, dynamic> response;
    if (data is Map<String, dynamic>) {
      if (data.containsKey('data') && data['data'] is Map) {
        response = data['data'];
      } else {
        response = data;
      }
    } else {
      return [];
    }

    final List<dynamic> images = response['images'] ?? [];
    return images.map((e) => ImageSearchResult.fromJson(e)).toList();
  }
}
