import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/core/models/listener_health.dart';
import 'package:piccolo_os/core/services/app_service.dart';
import 'package:piccolo_os/features/apps/app_detail_view.dart';
import 'package:piccolo_os/features/apps/widgets/app_web_view.dart';
import 'package:piccolo_os/features/apps/widgets/local_fallback_overlay.dart';
import 'package:piccolo_os/shells/desktop/desktop_controller.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:url_launcher/url_launcher.dart';

class AppLauncher {
  /// URL for embedding in an iframe. Prefers port-based (localUrl) on HTTP
  /// because it shares the portal's hostname, keeping cookies same-site.
  /// On HTTPS portal, uses host-based LAN URL (backend returns https:// when
  /// request is HTTPS, routed via the internal :443 TLS mux). On remote, uses
  /// remoteUrl.
  static String? _iframeUrl(ServiceEndpoint service, {String? overrideUrl}) {
    if (overrideUrl != null) return overrideUrl;

    final currentHost = Uri.base.host.toLowerCase();

    if (isLocalAccess(currentHost)) {
      if (Uri.base.scheme == 'https') {
        // HTTPS + IP/loopback: can't embed via mDNS hostname (user lacks mDNS)
        // and can't embed HTTP port-based URL (Mixed Content).
        // Return null to trigger browser tab fallback in openAppWindow().
        if (isIpAddress(currentHost) || isLoopback(currentHost)) {
          return null;
        }
        // HTTPS + .local: lanHostUrl is already https:// (backend honors request scheme).
        if (service.lanHostUrl != null) return service.lanHostUrl;
        // No host-based URL -- can't embed HTTP in HTTPS iframe
        return null;
      }
      // HTTP LAN access: port-based URL is same-site with portal -> cookies work
      return service.localUrl ?? service.remoteUrl;
    } else {
      // Remote (HTTPS): remoteUrl works for iframes with Secure cookies
      if (service.remoteUrl != null) return service.remoteUrl;
      // Avoid Mixed Content: do not fall back to HTTP localUrl on an HTTPS page
      if (Uri.base.scheme == 'https') return null;
      return service.localUrl;
    }
  }

  /// URL for opening in a new browser tab. Prefers host-based LAN URL
  /// when on .local since top-level navigations have no cookie restrictions.
  /// Backend returns lanHostUrl with the correct scheme (https when portal is HTTPS).
  static String? _browserUrl(ServiceEndpoint service) {
    final currentHost = Uri.base.host.toLowerCase();

    if (currentHost.endsWith('.local')) {
      return service.lanHostUrl ?? service.localUrl ?? service.remoteUrl;
    } else if (isIpAddress(currentHost) || isLoopback(currentHost)) {
      // IP/loopback access: skip lanHostUrl (mDNS hostname won't resolve for this user)
      return service.localUrl ?? service.remoteUrl;
    } else {
      return service.remoteUrl ?? service.lanHostUrl ?? service.localUrl;
    }
  }

  static bool isLocalAccess(String host) {
    return host.endsWith('.local') || isLoopback(host) || isIpAddress(host);
  }

  static bool isLoopback(String host) {
    return host == 'localhost';
  }

  static bool isIpAddress(String host) {
    // IPv4: digits and dots
    if (RegExp(r'^\d{1,3}(\.\d{1,3}){3}$').hasMatch(host)) return true;
    // IPv6: contains colon (may be bracketed)
    if (host.contains(':')) return true;
    return false;
  }

  /// Health-gates remote URL navigation. If the app's listener health is not
  /// ok, shows a LocalFallbackOverlay instead of navigating to a potentially
  /// broken TLS endpoint.
  static void healthGatedOpen({
    required BuildContext context,
    required DesktopController controller,
    required AppService appService,
    required App app,
    required ServiceEndpoint service,
    String? overrideUrl,
    ListenerHealth? healthOverride,
    String? iconUrl,
    String? originalIconUrl,
  }) {
    // Prefer explicit override (live stream data), then per-listener, then app-level
    final health = healthOverride ?? service.health ?? app.primaryListenerHealth;
    final currentHost = Uri.base.host.toLowerCase();

    // Local access doesn't need health-gating UNLESS the target URL is remote
    // (e.g., user clicks "Remote Access" link from local portal -- still TLS)
    final targetIsRemote = overrideUrl != null &&
        Uri.tryParse(overrideUrl)?.scheme == 'https';
    if (isLocalAccess(currentHost) && !targetIsRemote) {
      openAppWindow(
        controller: controller,
        appService: appService,
        app: app,
        service: service,
        overrideUrl: overrideUrl,
        iconUrl: iconUrl,
        originalIconUrl: originalIconUrl,
      );
      return;
    }

    // Health-gate remote access
    if (health == null) {
      // Unknown health -- fetch fresh data before deciding
      unawaited(_fetchAndGate(
        context: context,
        controller: controller,
        appService: appService,
        app: app,
        service: service,
        overrideUrl: overrideUrl,
        iconUrl: iconUrl,
        originalIconUrl: originalIconUrl,
      ));
      return;
    }

    _applyHealthGate(
      context: context,
      controller: controller,
      appService: appService,
      app: app,
      service: service,
      health: health,
      overrideUrl: overrideUrl,
      iconUrl: iconUrl,
      originalIconUrl: originalIconUrl,
    );
  }

  static Future<void> _fetchAndGate({
    required BuildContext context,
    required DesktopController controller,
    required AppService appService,
    required App app,
    required ServiceEndpoint service,
    String? overrideUrl,
    String? iconUrl,
    String? originalIconUrl,
  }) async {
    // Show a brief loading dialog while we fetch health
    unawaited(showDialog<void>(
      context: context,
      barrierDismissible: false,
      builder: (_) => const AlertDialog(
        content: Row(
          children: [
            CircularProgressIndicator(),
            SizedBox(width: 16),
            Text('Checking app health...'),
          ],
        ),
      ),
    ));

    final navigator = Navigator.of(context);

    try {
      final detail = await appService.getAppDetail(app.name);
      if (!navigator.canPop()) return;
      navigator.pop(); // dismiss loading
      if (!context.mounted) return;

      final health = detail.app.primaryListenerHealth;
      if (health == null) {
        // Still unknown after fetch -- show overlay explaining the situation
        final fallbackUrl = service.lanFallbackUrl ??
            service.lanHostUrl ??
            service.localUrl ??
            '';
        unawaited(showDialog<void>(
          context: context,
          builder: (_) => LocalFallbackOverlay(
            health: const ListenerHealth(
              status: 'recovering',
              reason: 'Health status is not yet available. Please try again shortly.',
            ),
            appName: app.displayTitle,
            lanFallbackUrl: fallbackUrl,
          ),
        ));
        return;
      }

      _applyHealthGate(
        context: context,
        controller: controller,
        appService: appService,
        app: app,
        service: service,
        health: health,
        overrideUrl: overrideUrl,
        iconUrl: iconUrl,
        originalIconUrl: originalIconUrl,
      );
    } on Object catch (_) {
      if (navigator.canPop()) navigator.pop();
      if (!context.mounted) return;
      // On fetch failure, show overlay rather than risk TLS error
      final fallbackUrl = service.lanFallbackUrl ??
          service.lanHostUrl ??
          service.localUrl ??
          '';
      unawaited(showDialog<void>(
        context: context,
        builder: (_) => LocalFallbackOverlay(
          health: const ListenerHealth(
            status: 'error',
            reason: 'Unable to verify app health. Remote access may not work.',
          ),
          appName: app.displayTitle,
          lanFallbackUrl: fallbackUrl,
        ),
      ));
    }
  }

  static void _applyHealthGate({
    required BuildContext context,
    required DesktopController controller,
    required AppService appService,
    required App app,
    required ServiceEndpoint service,
    required ListenerHealth health,
    String? overrideUrl,
    String? iconUrl,
    String? originalIconUrl,
  }) {
    // RFC 6.1: ok and degraded are usable -- only gate recovering/error
    if (health.isOk || health.isDegraded) {
      openAppWindow(
        controller: controller,
        appService: appService,
        app: app,
        service: service,
        overrideUrl: overrideUrl,
        iconUrl: iconUrl,
        originalIconUrl: originalIconUrl,
      );
      return;
    }

    // Recovering or error -- show fallback overlay
    final fallbackUrl =
        service.lanFallbackUrl ?? service.lanHostUrl ?? service.localUrl ?? '';
    unawaited(showDialog<void>(
      context: context,
      builder: (_) => LocalFallbackOverlay(
        health: health,
        appName: app.displayTitle,
        lanFallbackUrl: fallbackUrl,
        appService: appService,
        desktopController: controller,
        actionableCertId: _actionableCertId(health),
      ),
    ));
  }

  static String? _actionableCertId(ListenerHealth health) {
    final certs = health.certStatuses;
    if (certs == null || certs.isEmpty) return null;
    for (final entry in certs.entries) {
      if (entry.value.reasonCode == health.reasonCode) return entry.key;
    }
    for (final entry in certs.entries) {
      if (entry.value.status != 'ok') return entry.key;
    }
    return null;
  }

  static void openAppWindow({
    required DesktopController controller,
    required AppService appService,
    required App app,
    required ServiceEndpoint service,
    String? overrideUrl, // Allow passing a specific URL (e.g. remote vs local)
    String? iconUrl,
    String? originalIconUrl,
  }) {
    final iframeUrl = _iframeUrl(service, overrideUrl: overrideUrl);
    final browserUrl = _browserUrl(service) ?? iframeUrl;
    final windowId = 'app-window-${app.name}-${service.name}-$iframeUrl';
    final title = '${app.displayTitle} (${service.name})';

    if (iframeUrl == null || iframeUrl.isEmpty) {
      // Iframe embedding unavailable (e.g., mixed content on HTTPS portal).
      // Fall back to opening in a new browser tab if a URL exists.
      if (browserUrl != null && browserUrl.isNotEmpty) {
        unawaited(launchUrl(Uri.parse(browserUrl)));
        return;
      }
      // No URL available at all
      controller.openApp(
        windowId,
        'Launch failed',
        PiccoloIcons.error,
        const Center(child: Text('No URL available to launch the app.')),
      );
      return;
    }

    controller.openApp(
      windowId,
      title,
      PiccoloIcons.webAsset,
      AppWebView(url: iframeUrl),
      initialSize: const Size(1280, 800),
      requiresInterceptor: false,
      iconUrl: iconUrl,
      originalIconUrl: originalIconUrl,
      actions: [
        IconButton(
          icon: const Icon(PiccoloIcons.openExternal, size: 20),
          tooltip: 'Open in Browser',
          onPressed: () => launchUrl(Uri.parse(browserUrl!)),
        ),
        IconButton(
          icon: const Icon(PiccoloIcons.settings, size: 20),
          tooltip: 'Settings',
          onPressed: () {
            // Check if settings window is already open, if so focus it
            final settingsId = 'app-detail-${app.name}';
            if (controller.isAppOpen(settingsId)) {
              controller.focusWindow(settingsId);
            } else {
              // Open it
              controller.openApp(
                settingsId,
                app.displayTitle,
                PiccoloIcons.settingsApp,
                AppDetailView(
                  appId: app.name,
                  appService: appService,
                  desktopController: controller,
                  iconUrl: iconUrl,
                  originalIconUrl: originalIconUrl,
                ),
                iconUrl: iconUrl,
                originalIconUrl: originalIconUrl,
              );
            }
          },
        ),
      ],
    );
  }
}
