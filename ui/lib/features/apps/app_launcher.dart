import 'package:flutter/material.dart';
import 'package:url_launcher/url_launcher.dart';
import '../../core/models/app_models.dart';
import '../../core/services/app_service.dart';
import '../../shells/desktop/desktop_controller.dart';
import 'app_detail_view.dart';
import 'widgets/app_web_view.dart';

class AppLauncher {
  static void openAppWindow({
    required DesktopController controller,
    required AppService appService,
    required App app,
    required ServiceEndpoint service,
    String? overrideUrl, // Allow passing a specific URL (e.g. remote vs local)
  }) {
    final url = overrideUrl ?? service.lanUrl;
    final windowId = "app-window-${app.name}-${service.name}";
    final title = "${app.displayTitle} (${service.name})";

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
