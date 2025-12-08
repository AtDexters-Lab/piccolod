class CoreConfig {
  /// The base URL for API calls.
  ///
  /// In production (embedded), this is empty, implying relative paths.
  /// In development, this can be set via `--dart-define=API_BASE_URL=http://localhost:8080`.
  static const String apiBaseUrl = String.fromEnvironment(
    'API_BASE_URL',
    defaultValue: '',
  );

  /// Whether the app is running in debug mode.
  static const bool isDebug = bool.fromEnvironment('dart.vm.product') == false;

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
}
