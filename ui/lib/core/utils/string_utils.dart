/// Capitalizes the first letter of a string.
/// Returns empty string if input is empty.
String capitalize(String s) {
  if (s.isEmpty) return s;
  return s[0].toUpperCase() + s.substring(1);
}

/// Formats a Piccolo hostname into a display name.
/// Example: "piccolo-abc123.local" -> "Piccolo ABC123"
String formatHostnameDisplayName(String hostname) {
  final name = hostname.replaceAll('.local', '');
  final parts = name.split('-');
  if (parts.length == 2) {
    return '${capitalize(parts[0])} ${parts[1].toUpperCase()}';
  }
  return name;
}
