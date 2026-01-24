import 'package:flutter/material.dart';
import 'package:url_launcher/url_launcher.dart';
import '../../core/models/app_models.dart';
import '../../core/services/app_service.dart';
import '../../shells/desktop/desktop_controller.dart';
import 'app_detail_view.dart';
import 'widgets/app_web_view.dart';

class AppLauncher {
  /// URL for embedding in an iframe. Prefers port-based (localUrl) on HTTP
  /// because it shares the portal's hostname, keeping cookies same-site.
  /// On HTTPS (remote), uses remoteUrl where SameSite=None;Secure works.
  static String? _iframeUrl(ServiceEndpoint service, {String? overrideUrl}) {
    if (overrideUrl != null) return overrideUrl;

    final currentHost = Uri.base.host.toLowerCase();

    if (_isLocalAccess(currentHost)) {
      // HTTP LAN access: port-based URL is same-site with portal → cookies work
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
  static String? _browserUrl(ServiceEndpoint service) {
    final currentHost = Uri.base.host.toLowerCase();

    if (currentHost.endsWith('.local')) {
      return service.lanHostUrl ?? service.localUrl ?? service.remoteUrl;
    } else if (_isIpAddress(currentHost) || _isLoopback(currentHost)) {
      return service.localUrl ?? service.lanHostUrl ?? service.remoteUrl;
    } else {
      return service.remoteUrl ?? service.lanHostUrl ?? service.localUrl;
    }
  }

  static bool _isLocalAccess(String host) {
    return host.endsWith('.local') || _isLoopback(host) || _isIpAddress(host);
  }

  static bool _isLoopback(String host) {
    return host == 'localhost';
  }

  static bool _isIpAddress(String host) {
    // IPv4: digits and dots
    if (RegExp(r'^\d{1,3}(\.\d{1,3}){3}$').hasMatch(host)) return true;
    // IPv6: contains colon (may be bracketed)
    if (host.contains(':')) return true;
    return false;
  }

  static void openAppWindow({
    required DesktopController controller,
    required AppService appService,
    required App app,
    required ServiceEndpoint service,
    String? overrideUrl, // Allow passing a specific URL (e.g. remote vs local)
  }) {
    final iframeUrl = _iframeUrl(service, overrideUrl: overrideUrl);
    final browserUrl = _browserUrl(service) ?? iframeUrl;
    final windowId = "app-window-${app.name}-${service.name}-$iframeUrl";
    final title = "${app.displayTitle} (${service.name})";

    if (iframeUrl == null || iframeUrl.isEmpty) {
      // Cannot open app without a URL
      controller.openApp(
        windowId,
        "Launch failed",
        Icons.error_outline,
        const Center(child: Text("No URL available to launch the app.")),
      );
      return;
    }

    controller.openApp(
      windowId,
      title,
      Icons.web_asset,
      AppWebView(url: iframeUrl),
      initialSize: const Size(1280, 800),
      requiresInterceptor: false,
      actions: [
        IconButton(
          icon: const Icon(Icons.open_in_new, size: 20),
          tooltip: "Open in Browser",
          onPressed: () => launchUrl(Uri.parse(browserUrl!)),
        ),
        IconButton(
          icon: const Icon(Icons.settings, size: 20),
          tooltip: "Settings",
          onPressed: () {
            // Check if settings window is already open, if so focus it
            final settingsId = "app-detail-${app.name}";
            if (controller.isAppOpen(settingsId)) {
              controller.focusWindow(settingsId);
            } else {
              // Open it
              controller.openApp(
                settingsId,
                app.displayTitle,
                Icons.settings_applications,
                AppDetailView(
                  appId: app.name,
                  appService: appService,
                  desktopController: controller,
                ),
              );
            }
          },
        ),
      ],
    );
  }
}
