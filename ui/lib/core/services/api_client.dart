import 'dart:convert';
import 'package:flutter/foundation.dart';
import 'package:http/http.dart' as http;
import 'http_client_factory.dart'; // Import the factory
import '../config/core_config.dart';

class ApiClient {
  // Singleton instance
  static final ApiClient _instance = ApiClient._internal();
  factory ApiClient() => _instance;

  late final http.Client _client;
  final String _baseUrl;
  String? _csrfToken;

  ApiClient._internal() : _baseUrl = CoreConfig.apiBaseUrl {
    _client = createHttpClient(); // Use the factory
  }

  /// Helper to construct the full URI.
  Uri _buildUri(String path, [Map<String, dynamic>? queryParameters]) {
    String urlString;
    if (_baseUrl.isEmpty) {
      urlString = path;
    } else {
      final cleanBase = _baseUrl.endsWith('/')
          ? _baseUrl.substring(0, _baseUrl.length - 1)
          : _baseUrl;
      final cleanPath = path.startsWith('/') ? path : '/$path';
      urlString = '$cleanBase$cleanPath';
    }

    final uri = Uri.parse(urlString);
    if (queryParameters != null) {
      return uri.replace(queryParameters: queryParameters);
    }
    return uri;
  }

  /// Fetches and stores the CSRF token.
  /// Should be called after authentication/unlocking.
  Future<void> fetchCsrfToken() async {
    try {
      final response = await get('/api/v1/auth/csrf');
      if (response is Map && response.containsKey('token')) {
        _csrfToken = response['token'];
      }
    } catch (e) {
      debugPrint("Failed to fetch CSRF token: $e");
      // Don't rethrow, just proceed. Some endpoints might not need it.
    }
  }

  Future<dynamic> get(
    String path, {
    Map<String, dynamic>? queryParameters,
  }) async {
    final uri = _buildUri(path, queryParameters);
    final response = await _client.get(uri, headers: _getHeaders());
    return _handleResponse(response);
  }

  Future<dynamic> post(String path, {Object? body}) async {
    // Automatically ensure we have a CSRF token before mutating state.
    // This prevents 401/403 errors when developers forget to call fetchCsrfToken().
    if (_csrfToken == null) {
      await fetchCsrfToken();
    }

    final uri = _buildUri(path);
    final response = await _client.post(
      uri,
      headers: _getHeaders(contentType: 'application/json'),
      body: body != null ? jsonEncode(body) : null,
    );
    return _handleResponse(response);
  }

  Future<dynamic> delete(String path, {Object? body}) async {
    if (_csrfToken == null) {
      await fetchCsrfToken();
    }

    final uri = _buildUri(path);
    final response = await _client.delete(
      uri,
      headers: _getHeaders(contentType: 'application/json'),
      body: body != null ? jsonEncode(body) : null,
    );
    return _handleResponse(response);
  }

  Map<String, String> _getHeaders({String? contentType}) {
    final headers = <String, String>{};
    if (contentType != null) {
      headers['Content-Type'] = contentType;
    }
    if (_csrfToken != null) {
      headers['X-CSRF-Token'] = _csrfToken!;
    }
    // Note: "withCredentials" is a property of the request, not a header.
    // package:http BrowserClient usually handles cookies if the browser sends them.
    return headers;
  }

  dynamic _handleResponse(http.Response response) {
    if (response.statusCode >= 200 && response.statusCode < 300) {
      if (response.body.isEmpty) return null;
      try {
        return jsonDecode(response.body);
      } catch (_) {
        return response.body;
      }
    } else {
      throw ApiException(response.statusCode, response.body);
    }
  }
}

class ApiException implements Exception {
  final int statusCode;
  final String message;

  ApiException(this.statusCode, this.message);

  @override
  String toString() => 'ApiException($statusCode): $message';
}
