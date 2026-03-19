import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/core/models/task_progress.dart';
import 'package:piccolo_os/core/services/api_client.dart';

class AppService {

  AppService(this._client);
  final ApiClient _client;

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
      if (data.containsKey('data') && data['data'] is Map<dynamic, dynamic>) {
        json = Map<String, dynamic>.from(data['data'] as Map<dynamic, dynamic>);
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

    var list = <dynamic>[];
    if (data is Map<dynamic, dynamic>) {
      final dataWrapper = data['data'];
      if (dataWrapper is Map<dynamic, dynamic> && dataWrapper['categories'] is List<dynamic>) {
        list = dataWrapper['categories'] as List<dynamic>;
      } else if (data['categories'] is List<dynamic>) {
        list = data['categories'] as List<dynamic>;
      }
    }
    return list.whereType<String>().toList();
  }

  Future<String?> getCatalogTemplate(String name) async {
    final data = await _client.get('/api/v1/catalog/$name/template');
    return data is String ? data : null;
  }

  // --- Active Tasks ---

  Future<List<TaskProgressEvent>> getActiveTasks() async {
    final data = await _client.get('/api/v1/tasks/active');
    if (data is! Map<String, dynamic>) return [];
    final list = (data['data'] as List?) ?? [];
    return list
        .whereType<Map<String, dynamic>>()
        .map(TaskProgressEvent.fromJson)
        .toList();
  }

  // --- Installed Apps ---

  Future<List<App>> getApps() async {
    final data = await _client.get('/api/v1/apps');
    // Expected: { data: [...] }
    final rawData = (data as Map<String, dynamic>)['data'];
    final list = rawData is List ? rawData : <dynamic>[];
    return list
        .whereType<Map<dynamic, dynamic>>()
        .map((e) => App.fromJson(Map<String, dynamic>.from(e)))
        .toList();
  }

  Future<AppDetail> getAppDetail(String name) async {
    final data = await _client.get('/api/v1/apps/$name');
    // Expected: { data: { app: {...}, listeners: [...], containers: [...] } }

    final payload = (data is Map<dynamic, dynamic>) ? data['data'] : null;
    final json = payload is Map<dynamic, dynamic>
        ? Map<String, dynamic>.from(payload)
        : <String, dynamic>{};

    final appJson = json['app'];
    final app = App.fromJson(
      appJson is Map<dynamic, dynamic> ? Map<String, dynamic>.from(appJson) : <String, dynamic>{},
    );

    final rawListeners = json['listeners'] ?? json['services'];
    final listeners = (rawListeners is List<dynamic> ? rawListeners : const <dynamic>[])
        .whereType<Map<dynamic, dynamic>>()
        .map((e) => ServiceEndpoint.fromJson(Map<String, dynamic>.from(e)))
        .toList();

    final rawContainers = json['containers'];
    final containers = (rawContainers is List<dynamic> ? rawContainers : const <dynamic>[])
        .whereType<Map<dynamic, dynamic>>()
        .map((e) => AppContainerStatus.fromJson(Map<String, dynamic>.from(e)))
        .toList();

    final snapshotAvailable = json['snapshot_available'] == true;
    return AppDetail(app: app, listeners: listeners, containers: containers, snapshotAvailable: snapshotAvailable);
  }

  Future<List<ServiceEndpoint>> getAppServices(String name) async {
    try {
      final detail = await getAppDetail(name);
      return detail.listeners;
    } on Exception catch (_) {
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

  Future<void> updateApp(String name, {String? tag, String? taskId}) async {
    final body = <String, dynamic>{};
    if (tag != null) body['tag'] = tag;
    try {
      await _client
          .post('/api/v1/apps/$name/update', body: body, headers: _taskHeaders(taskId))
          .timeout(const Duration(seconds: 10));
    } on TimeoutException {
      debugPrint('updateApp: POST timed out (expected for image pulls)');
    }
  }

  Future<void> rollbackApp(String name, {String? taskId}) async {
    try {
      await _client
          .post('/api/v1/apps/$name/rollback', body: {}, headers: _taskHeaders(taskId))
          .timeout(const Duration(seconds: 10));
    } on TimeoutException {
      debugPrint('rollbackApp: POST timed out (expected for slow container stops)');
    }
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
      final dataMap = (data as Map<String, dynamic>)['data'];
      final valid = (dataMap is Map<dynamic, dynamic> ? (dataMap['valid'] as bool?) : null) ?? false;
      return AppValidationResult(valid: valid);
    } on ApiException catch (e) {
      // Validation error often comes as 400 with details
      return AppValidationResult(valid: false, error: e.message);
    } on Exception catch (e) {
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
    return App.fromJson(
      Map<String, dynamic>.from((data as Map<String, dynamic>)['data'] as Map),
    );
  }

  Future<Map<String, dynamic>> getCatalogConfigure(String name) async {
    final data = await _client.get('/api/v1/catalog/$name/configure');
    // Expected: { data: { inputName: { type:..., default:..., ... } } }
    final rawData = (data as Map<String, dynamic>)['data'];
    return rawData is Map<dynamic, dynamic> ? Map<String, dynamic>.from(rawData) : <String, dynamic>{};
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
    return App.fromJson(
      Map<String, dynamic>.from((data as Map<String, dynamic>)['data'] as Map),
    );
  }

  /// Fire-and-forget install: sends the install POST with a short timeout.
  /// Validation errors (4xx) return within the timeout and propagate.
  /// For long installs, the timeout fires silently — the backend continues
  /// on its 30-min background context, and the client tracks progress via WebSocket.
  Future<void> initiateInstall(
    String yamlContent,
    Map<String, dynamic> inputs, {
    String? taskId,
    String? catalogSource,
  }) async {
    final body = <String, dynamic>{
      'app_definition': yamlContent,
      'inputs': inputs,
    };
    if (catalogSource != null && catalogSource.isNotEmpty) {
      body['catalog_source'] = catalogSource;
    }
    try {
      await _client
          .post('/api/v1/apps', body: body, headers: _taskHeaders(taskId))
          .timeout(const Duration(seconds: 10));
    } on TimeoutException {
      // Expected for non-cached installs. Backend continues in background.
      debugPrint('initiateInstall: POST timed out (expected for long installs)');
    }
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
      if (data.containsKey('data') && data['data'] is Map<dynamic, dynamic>) {
        response = Map<String, dynamic>.from(data['data'] as Map<dynamic, dynamic>);
      } else {
        response = data;
      }
    } else {
      return [];
    }

    final images = (response['images'] as List<dynamic>?) ?? <dynamic>[];
    return images
        .whereType<Map<dynamic, dynamic>>()
        .map((e) => ImageSearchResult.fromJson(Map<String, dynamic>.from(e)))
        .toList();
  }
}
