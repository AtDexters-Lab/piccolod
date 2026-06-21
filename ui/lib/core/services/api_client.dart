import 'dart:async';
import 'dart:convert';

import 'package:flutter/foundation.dart';
import 'package:http/http.dart' as http;
import 'package:piccolo_os/core/config/core_config.dart';
import 'package:piccolo_os/core/services/http_client_factory.dart'; // Import the factory
import 'package:piccolo_os/core/utils/browser_tz/browser_tz.dart';

class ApiClient {
  factory ApiClient() => _instance;

  ApiClient._internal() : _baseUrl = CoreConfig.apiBaseUrl {
    _client = createHttpClient(); // Use the factory
  }
  // Singleton instance
  static final ApiClient _instance = ApiClient._internal();

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
    '/api/v1/auth/login-options',
    '/api/v1/auth/passkey/login/begin',
    '/api/v1/auth/passkey/login/finish',
    '/api/v1/system/boot',
    '/api/v1/identity/setup-hostname',
    '/api/v1/identity/remote-readiness',
  };

  /// Pre-auth setup endpoints that skip automatic CSRF token fetch.
  static const _preAuthPaths = {
    '/api/v1/crypto/setup',
    '/api/v1/identity/setup-hostname',
    '/api/v1/identity/remote-readiness',
  };

  static bool _isPreAuthPath(String path) => _preAuthPaths.contains(path);

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
        _csrfToken = response['token'] as String?;
      }
    } on Object catch (e) {
      debugPrint('Failed to fetch CSRF token: $e');
      // Don't rethrow, just proceed. Some endpoints might not need it.
    }
  }

  /// Called by the re-auth overlay after login succeeds or is cancelled.
  /// Guarded against double-completion (e.g. timeout racing with user action).
  void completeReauth({required bool success}) {
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
    return _handleResponse(await _withReauthRaw(path, request));
  }

  /// _withReauthRaw runs [request], driving the re-auth overlay + single retry
  /// on a 401, and returns the raw [http.Response] (no JSON decoding). Shared by
  /// [_withReauth] (which adds JSON handling) and [getText] (which wants the raw
  /// body) so both authenticated GET paths get the same session-expiry flow.
  Future<http.Response> _withReauthRaw(
    String path,
    Future<http.Response> Function() request,
  ) async {
    final response = await request();

    final cleanPath = path.contains('?')
        ? path.substring(0, path.indexOf('?'))
        : path;
    if (response.statusCode == 401 &&
        !_reauthExcludedPaths.contains(cleanPath) &&
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
          completeReauth(success: false);
          return false;
        },
      );

      if (success) {
        // Retry once — if the retry also 401s, the caller surfaces it.
        return request();
      }

      // Re-auth failed or cancelled — throw original 401.
      throw ApiException(response.statusCode, response.body);
    }

    return response;
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

  /// getText performs a GET expecting a non-JSON (text/plain) body, returning
  /// it raw. Throws [ApiException] (carrying the HTTP status) on non-2xx so
  /// callers can branch on 503/429/etc. Used by the app-log download, which
  /// needs status visibility rather than the JSON-decoding [get].
  Future<String> getText(
    String path, {
    Map<String, dynamic>? queryParameters,
  }) async {
    final uri = _buildUri(path, queryParameters);
    final response = await _withReauthRaw(
      path,
      () async => _client.get(uri, headers: _getHeaders()),
    );
    if (response.statusCode >= 200 && response.statusCode < 300) {
      return response.body;
    }
    throw ApiException(response.statusCode, response.body);
  }

  Future<dynamic> post(
    String path, {
    Object? body,
    Map<String, String>? headers,
  }) async {
    // Automatically ensure we have a CSRF token before mutating state.
    // Skip for pre-auth setup endpoints (no session exists yet, CSRF fetch would fail).
    if (_csrfToken == null && !_isPreAuthPath(path)) {
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

  Future<dynamic> put(
    String path, {
    Object? body,
    Map<String, String>? headers,
  }) async {
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

  Future<dynamic> patch(
    String path, {
    Object? body,
    Map<String, String>? headers,
  }) async {
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

  Future<dynamic> delete(
    String path, {
    Object? body,
    Map<String, String>? headers,
  }) async {
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
    // X-Browser-Timezone: one-shot hint for the backend to seed
    // /etc/localtime if it's still on the UTC default. The middleware
    // skips when the device timezone is already configured, so this is
    // safe to include on every request. Cached after the first lookup.
    final tz = resolvedBrowserTimezone();
    if (tz != null) {
      headers['X-Browser-Timezone'] = tz;
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
      } on FormatException catch (_) {
        return response.body;
      }
    } else {
      throw ApiException(response.statusCode, response.body);
    }
  }

  // --- Passkey API ---

  Future<Map<String, dynamic>> getLoginOptions() async {
    return await get('/api/v1/auth/login-options') as Map<String, dynamic>;
  }

  Future<Map<String, dynamic>> beginPasskeyRegistration() async {
    return await post('/api/v1/auth/passkey/register/begin')
        as Map<String, dynamic>;
  }

  Future<Map<String, dynamic>> finishPasskeyRegistration(
    String sessionId,
    Map<String, dynamic> credential,
  ) async {
    return await post(
          '/api/v1/auth/passkey/register/finish?session_id=$sessionId',
          body: credential,
        )
        as Map<String, dynamic>;
  }

  Future<Map<String, dynamic>> beginPasskeyLogin() async {
    return await post('/api/v1/auth/passkey/login/begin')
        as Map<String, dynamic>;
  }

  Future<Map<String, dynamic>> finishPasskeyLogin(
    String sessionId,
    Map<String, dynamic> credential,
  ) async {
    return await post(
          '/api/v1/auth/passkey/login/finish?session_id=$sessionId',
          body: credential,
        )
        as Map<String, dynamic>;
  }

  Future<List<dynamic>> listPasskeys() async {
    final result = await get('/api/v1/auth/passkeys') as Map<String, dynamic>;
    return result['passkeys'] as List<dynamic>? ?? [];
  }

  /// Returns the response body which carries `rp_id` and `credential_id` for
  /// the post-delete WebAuthn Signal API call (D-9 / D-14).
  Future<Map<String, dynamic>> deletePasskey(String id) async {
    final res = await delete('/api/v1/auth/passkeys/$id');
    return (res is Map<String, dynamic>) ? res : <String, dynamic>{};
  }

  /// Renames a passkey's friendly label. No Signal API call is fired on
  /// rename: the WebAuthn Signal API's signalCurrentUserDetails mutates
  /// account-level user identity, not per-credential labels — firing it here
  /// would clobber the OS/browser's account display with a credential nickname.
  Future<Map<String, dynamic>> renamePasskey(String id, String name) async {
    final res = await patch(
      '/api/v1/auth/passkeys/$id',
      body: {'friendly_name': name},
    );
    return (res is Map<String, dynamic>) ? res : <String, dynamic>{};
  }

  // --- Invite API ---

  Future<Map<String, dynamic>> createInvite({
    required String username,
    required String email,
    List<String>? allowedApps,
  }) async {
    return await post(
          '/api/v1/users/invite',
          body: {
            'username': username,
            'email': email,
            'allowed_apps': ?allowedApps,
          },
        )
        as Map<String, dynamic>;
  }

  Future<Map<String, dynamic>> validateInvite(String token) async {
    return await get('/api/v1/auth/invite/$token') as Map<String, dynamic>;
  }

  Future<Map<String, dynamic>> beginInvitePasskey(String token) async {
    return await post('/api/v1/auth/invite/$token/passkey/begin')
        as Map<String, dynamic>;
  }

  Future<Map<String, dynamic>> finishInvitePasskey(
    String token,
    String sessionId,
    Map<String, dynamic> credential,
  ) async {
    return await post(
          '/api/v1/auth/invite/$token/passkey/finish?session_id=$sessionId',
          body: credential,
        )
        as Map<String, dynamic>;
  }

  Future<Map<String, dynamic>> reinviteUser(String userId) async {
    return await post('/api/v1/users/$userId/reinvite') as Map<String, dynamic>;
  }
}

class ApiException implements Exception {
  ApiException(this.statusCode, String body)
    : rawBody = body,
      message = _extractApiErrorMessage(statusCode, body),
      key = _extractApiErrorKey(body);
  final int statusCode;
  final String message;
  final String? key;
  final String rawBody;

  @override
  String toString() => message;
}

Map<String, dynamic>? _decodeApiErrorBody(String body) {
  if (body.trim().isEmpty) return null;
  try {
    final decoded = jsonDecode(body);
    if (decoded is! Map) return null;
    final map = Map<String, dynamic>.from(decoded);
    final error = map['error'];
    if (error is Map) {
      return Map<String, dynamic>.from(error);
    }
    if (error is String && error.trim().isNotEmpty) {
      final normalized = Map<String, dynamic>.from(map);
      normalized['message'] ??= error.trim();
      return normalized;
    }
    return map;
  } on FormatException {
    return null;
  }
}

String _extractApiErrorMessage(int statusCode, String body) {
  final decoded = _decodeApiErrorBody(body);
  final message = decoded?['message'];
  if (message is String && message.trim().isNotEmpty) {
    return message.trim();
  }
  if (body.trim().isNotEmpty && decoded == null) {
    return body.trim();
  }
  return 'Request failed ($statusCode)';
}

String? _extractApiErrorKey(String body) {
  final decoded = _decodeApiErrorBody(body);
  final key = decoded?['key'];
  if (key is String && key.trim().isNotEmpty) {
    return key.trim();
  }
  return null;
}
