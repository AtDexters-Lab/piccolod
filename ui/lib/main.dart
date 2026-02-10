import 'dart:js_interop';

import 'package:flutter/material.dart';
import 'package:web/web.dart' as web;
import 'shells/desktop/desktop_shell.dart';
import 'shells/gateway/gateway_shell.dart';
import 'theme/piccolo_theme.dart';

void main() async {
  // Attempt HTTPS upgrade before anything else (no-op if already HTTPS or dev)
  if (await _tryUpgradeToHttps()) return;

  // Determine if this is a gateway access (piccolo.local)
  final isGateway = _isGatewayAccess();

  if (isGateway) {
    runApp(const GatewayShell());
  } else {
    runApp(const PiccoloApp());
  }
}

/// Probes HTTPS on the current host and redirects if the CA is trusted.
/// Returns true if a redirect was initiated (caller should bail out).
/// Skips upgrade when already on HTTPS or on localhost (dev environment).
Future<bool> _tryUpgradeToHttps() async {
  final uri = Uri.base;

  // Already HTTPS — nothing to do
  if (uri.scheme == 'https') return false;

  // Skip localhost / dev environments
  final host = uri.host.toLowerCase();
  if (host == 'localhost' || host == '127.0.0.1') return false;

  // Skip IP addresses — cert doesn't cover LAN IPs (DHCP-assigned),
  // and HTTPS+IP can't embed apps in iframes anyway.
  if (_isIpAddress(host)) return false;

  // Skip the floating gateway hostname — any device can serve piccolo.local
  // via leader election, each with its own CA. HTTPS upgrade would cause TLS
  // errors when leadership changes. Device-specific hostnames still upgrade.
  if (host == 'piccolo.local') return false;

  // Probe HTTPS using no-cors mode (cross-origin safe).
  // A non-throwing fetch means TLS succeeded → CA is trusted.
  try {
    final probeUrl = 'https://$host/api/v1/health/live';
    final init = web.RequestInit(mode: 'no-cors');
    final response = await web.window.fetch(probeUrl.toJS, init).toDart
        .timeout(const Duration(seconds: 2));
    if (response.type == 'opaque' || response.ok) {
      // Rebuild current URL with https scheme, preserving path and query
      final httpsUrl = uri.replace(scheme: 'https').toString();
      web.window.location.replace(httpsUrl);
      return true;
    }
  } catch (_) {
    // HTTPS unavailable or CA not trusted — continue on HTTP
  }

  return false;
}

bool _isIpAddress(String host) {
  if (RegExp(r'^\d{1,3}(\.\d{1,3}){3}$').hasMatch(host)) return true;
  if (host.contains(':')) return true; // IPv6
  return false;
}

/// Checks if the current access is via the gateway domain (piccolo.local).
/// Returns true if the hostname is exactly "piccolo.local".
bool _isGatewayAccess() {
  final host = web.window.location.hostname.toLowerCase();
  return host == 'piccolo.local';
}

class PiccoloApp extends StatelessWidget {
  const PiccoloApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Piccolo',
      debugShowCheckedModeBanner: false,
      theme: PiccoloTheme.lightTheme,
      // In the future, we can detect platform/screen size here to choose shell.
      home: const DesktopShell(),
    );
  }
}
