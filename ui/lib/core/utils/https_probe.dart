import 'dart:js_interop';

import 'package:web/web.dart' as web;

/// Probes whether HTTPS is available on the current host.
///
/// Returns true if the CA is trusted and HTTPS is reachable.
/// Skips probe (returns false) for localhost, IPs, piccolo.local,
/// and when already on HTTPS.
Future<bool> probeHttpsAvailable() async {
  final uri = Uri.base;

  // Already HTTPS — nothing to do
  if (uri.scheme == 'https') return false;

  // Skip localhost / dev environments
  final host = uri.host.toLowerCase();
  if (host == 'localhost' || host == '127.0.0.1') return false;

  // Skip IP addresses — cert doesn't cover LAN IPs (DHCP-assigned),
  // and HTTPS+IP can't embed apps in iframes anyway.
  if (isIpAddress(host)) return false;

  // Skip the floating gateway hostname — any device can serve piccolo.local
  // via leader election, each with its own CA. HTTPS upgrade would cause TLS
  // errors when leadership changes. Device-specific hostnames still upgrade.
  if (host == 'piccolo.local') return false;

  // Probe HTTPS using no-cors mode (cross-origin safe).
  // A non-throwing fetch means TLS succeeded -> CA is trusted.
  try {
    final probeUrl = 'https://$host/api/v1/health/live';
    final init = web.RequestInit(mode: 'no-cors');
    final response = await web.window
        .fetch(probeUrl.toJS, init)
        .toDart
        .timeout(const Duration(seconds: 2));
    if (response.type == 'opaque' || response.ok) {
      return true;
    }
  } on Object catch (_) {
    // HTTPS unavailable or CA not trusted
  }

  return false;
}

/// Whether the given host string is an IP address (v4 or v6).
bool isIpAddress(String host) {
  if (RegExp(r'^\d{1,3}(\.\d{1,3}){3}$').hasMatch(host)) return true;
  if (host.contains(':')) return true; // IPv6
  return false;
}
