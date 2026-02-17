import 'dart:async';
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

  /// Set by the shell to show a re-auth overlay when a 401 is received.
  VoidCallback? onAuthRequired;

  /// Completer that deduplicates concurrent 401 re-auth flows.
  Completer<bool>? _reauthCompleter;

  /// Paths excluded from the re-auth interceptor (auth lifecycle endpoints).
  static const _reauthExcludedPaths = {
    '/api/v1/auth/login',
    '/api/v1/auth/logout',
    '/api/v1/auth/initialized',
    '/api/v1/auth/session',
    '/api/v1/auth/csrf',
    '/api/v1/crypto/setup',
    '/api/v1/crypto/unlock',
  };

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

  /// Called by the re-auth overlay after login succeeds or is cancelled.
  /// Guarded against double-completion (e.g. timeout racing with user action).
  void completeReauth(bool success) {
    if (_reauthCompleter != null && !_reauthCompleter!.isCompleted) {
      _reauthCompleter!.complete(success);
    }
    _reauthCompleter = null;
  }

  /// Wraps an HTTP request with 401 re-auth interception.
  ///
  /// On 401 (for non-excluded paths):
  /// 1. Triggers the re-auth overlay via [onAuthRequired].
  /// 2. Awaits the completer (with 5-min timeout).
  /// 3. On success, retries the request once.
  /// 4. On failure/timeout, throws the original 401 ApiException.
  Future<dynamic> _withReauth(
    String path,
    Future<http.Response> Function() request,
  ) async {
    final response = await request();

    if (response.statusCode == 401 &&
        !_reauthExcludedPaths.contains(path) &&
        onAuthRequired != null) {
      // Deduplicate: if a re-auth flow is already in progress, await that one.
      if (_reauthCompleter == null) {
        _reauthCompleter = Completer<bool>();
        onAuthRequired!();
      }

      // Capture in local before await — completeReauth() nulls the field.
      final completer = _reauthCompleter!;
      final success = await completer.future.timeout(
        const Duration(minutes: 5),
        onTimeout: () {
          completeReauth(false);
          return false;
        },
      );

      if (success) {
        // Retry once — if the retry also 401s, throw immediately (no re-enter).
        final retryResponse = await request();
        return _handleResponse(retryResponse);
      }

      // Re-auth failed or cancelled — throw original 401.
      throw ApiException(response.statusCode, response.body);
    }

    return _handleResponse(response);
  }

  Future<dynamic> get(
    String path, {
    Map<String, dynamic>? queryParameters,
    Map<String, String>? headers,
  }) async {
    final uri = _buildUri(path, queryParameters);
    return _withReauth(path, () async {
      final mergedHeaders = _getHeaders();
      if (headers != null) mergedHeaders.addAll(headers);
      return _client.get(uri, headers: mergedHeaders);
    });
  }

  Future<dynamic> post(String path, {Object? body, Map<String, String>? headers}) async {
    // Automatically ensure we have a CSRF token before mutating state.
    // This prevents 401/403 errors when developers forget to call fetchCsrfToken().
    if (_csrfToken == null) {
      await fetchCsrfToken();
    }

    final uri = _buildUri(path);
    return _withReauth(path, () async {
      final mergedHeaders = _getHeaders(contentType: 'application/json');
      if (headers != null) mergedHeaders.addAll(headers);
      return _client.post(
        uri,
        headers: mergedHeaders,
        body: body != null ? jsonEncode(body) : null,
      );
    });
  }

  Future<dynamic> postRaw(
    String path, {
    required String body,
    required String contentType,
    Map<String, String>? headers,
  }) async {
    if (_csrfToken == null) {
      await fetchCsrfToken();
    }

    final uri = _buildUri(path);
    return _withReauth(path, () async {
      final mergedHeaders = _getHeaders(contentType: contentType);
      if (headers != null) mergedHeaders.addAll(headers);
      return _client.post(
        uri,
        headers: mergedHeaders,
        body: body,
      );
    });
  }

  Future<dynamic> put(String path, {Object? body, Map<String, String>? headers}) async {
    if (_csrfToken == null) {
      await fetchCsrfToken();
    }

    final uri = _buildUri(path);
    return _withReauth(path, () async {
      final mergedHeaders = _getHeaders(contentType: 'application/json');
      if (headers != null) mergedHeaders.addAll(headers);
      return _client.put(
        uri,
        headers: mergedHeaders,
        body: body != null ? jsonEncode(body) : null,
      );
    });
  }

  Future<dynamic> patch(String path, {Object? body, Map<String, String>? headers}) async {
    if (_csrfToken == null) {
      await fetchCsrfToken();
    }

    final uri = _buildUri(path);
    return _withReauth(path, () async {
      final mergedHeaders = _getHeaders(contentType: 'application/json');
      if (headers != null) mergedHeaders.addAll(headers);
      return _client.patch(
        uri,
        headers: mergedHeaders,
        body: body != null ? jsonEncode(body) : null,
      );
    });
  }

  Future<dynamic> delete(String path, {Object? body, Map<String, String>? headers}) async {
    if (_csrfToken == null) {
      await fetchCsrfToken();
    }

    final uri = _buildUri(path);
    return _withReauth(path, () async {
      final mergedHeaders = _getHeaders(contentType: 'application/json');
      if (headers != null) mergedHeaders.addAll(headers);
      return _client.delete(
        uri,
        headers: mergedHeaders,
        body: body != null ? jsonEncode(body) : null,
      );
    });
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
