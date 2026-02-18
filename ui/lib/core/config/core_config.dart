class CoreConfig {
  /// The base URL for API calls.
  ///
  /// In production (embedded), this is empty, implying relative paths.
  /// In development, this can be set via `--dart-define=API_BASE_URL=http://localhost:8080`.
  static const String apiBaseUrl = String.fromEnvironment(
    'API_BASE_URL',
  );

  /// Whether the app is running in debug mode.
  static const bool isDebug = !bool.fromEnvironment('dart.vm.product');

  /// Returns the WebSocket base URL derived from [apiBaseUrl].
  ///
  /// If [apiBaseUrl] is empty, returns empty string.
  /// If [apiBaseUrl] starts with 'http', replaces it with 'ws'.
  /// If [apiBaseUrl] starts with 'https', replaces it with 'wss'.
  static String get wsBaseUrl {
    if (apiBaseUrl.isEmpty) return '';
    if (apiBaseUrl.startsWith('https://')) {
      return apiBaseUrl.replaceFirst('https://', 'wss://');
    } else if (apiBaseUrl.startsWith('http://')) {
      return apiBaseUrl.replaceFirst('http://', 'ws://');
    }
    // Fallback for weird schemes or bare domains
    return 'ws://$apiBaseUrl';
  }

  /// Returns the proxy URL for fetching a catalog app's icon.
  ///
  /// The icon proxy endpoint provides SSRF protection and caching.
  /// Example: `catalogIconUrl('nextcloud')` returns `/api/v1/catalog/nextcloud/icon`
  /// (or `http://localhost:8080/api/v1/catalog/nextcloud/icon` in dev).
  static String catalogIconUrl(String appName) {
    final base = apiBaseUrl.endsWith('/') ? apiBaseUrl.substring(0, apiBaseUrl.length - 1) : apiBaseUrl;
    return '$base/api/v1/catalog/${Uri.encodeComponent(appName)}/icon';
  }
}
