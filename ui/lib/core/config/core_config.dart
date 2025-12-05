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
}
