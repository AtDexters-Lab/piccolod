/// Non-web stub. Returns null so the X-Browser-Timezone header is simply
/// omitted on platforms without a browser-resolved zone (e.g. analyzer
/// runs, hypothetical native builds).
String? resolvedBrowserTimezone() => null;
