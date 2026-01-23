import 'package:flutter/material.dart';
import 'package:url_launcher/url_launcher.dart';
import '../../core/models/app_models.dart';
import '../../core/services/app_service.dart';
import '../../shells/desktop/desktop_controller.dart';
import 'app_detail_view.dart';
import 'widgets/app_web_view.dart';

class AppLauncher {
  static String? _preferredUrl(ServiceEndpoint service, {String? overrideUrl}) {
    if (overrideUrl != null) return overrideUrl;

    final currentHost = Uri.base.host.toLowerCase();

    if (currentHost.endsWith('.local')) {
      // Portal loaded via mDNS → resolver works → prefer host-based LAN URL
      return service.lanHostUrl ?? service.localUrl ?? service.remoteUrl;
    } else if (_isIpAddress(currentHost)) {
      // Portal loaded via IP → mDNS likely not working → prefer port-based
      return service.localUrl ?? service.lanHostUrl ?? service.remoteUrl;
    } else {
      // Remote access (external hostname)
      return service.remoteUrl ?? service.lanHostUrl ?? service.localUrl;
    }
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
    final url = _preferredUrl(service, overrideUrl: overrideUrl);
    final windowId = "app-window-${app.name}-${service.name}-$url";
    final title = "${app.displayTitle} (${service.name})";

    if (url == null || url.isEmpty) {
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
      AppWebView(url: url),
      initialSize: const Size(1280, 800),
      requiresInterceptor: false,
      actions: [
        IconButton(
          icon: const Icon(Icons.open_in_new, size: 20),
          tooltip: "Open in Browser",
          onPressed: () => launchUrl(Uri.parse(url)),
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
