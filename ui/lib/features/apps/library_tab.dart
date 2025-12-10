import 'package:flutter/material.dart';
import '../../core/models/app_models.dart';
import '../../core/services/app_service.dart';
import '../../theme/piccolo_theme.dart';
import '../../shells/desktop/desktop_controller.dart';
import 'app_detail_view.dart';
import 'app_launcher.dart';

class LibraryTab extends StatefulWidget {
  final AppService appService;
  final String searchQuery;
  final DesktopController desktopController;

  const LibraryTab({
    super.key,
    required this.appService,
    required this.searchQuery,
    required this.desktopController,
  });

  @override
  State<LibraryTab> createState() => _LibraryTabState();
}

class _LibraryTabState extends State<LibraryTab> {
  List<App> _allApps = [];
  bool _isLoading = true;
  String? _error;

  @override
  void initState() {
    super.initState();
    _loadApps();
  }
  
  // Expose for parent/siblings to trigger refresh
  void refresh() => _loadApps();

  Future<void> _loadApps() async {
    setState(() {
      _isLoading = true;
      _error = null;
    });

    try {
      final apps = await widget.appService.getApps();
      if (!mounted) return;
      setState(() {
        _allApps = apps;
        _isLoading = false;
      });
    } catch (e) {
      if (!mounted) return;
      setState(() {
        _error = e.toString();
        _isLoading = false;
      });
    }
  }
  
  void _openAppDetail(App app) {
    widget.desktopController.openApp(
      "app-detail-${app.name}",
      app.name,
      Icons.settings_applications,
      AppDetailView(
        appId: app.name,
        appService: widget.appService,
        desktopController: widget.desktopController,
      ),
    );
  }

  Future<void> _launchApp(App app) async {
    try {
      // If app is stopped/error, go straight to settings to fix it
      if (!app.isRunning) {
        _openAppDetail(app);
        return;
      }

      final services = await widget.appService.getAppServices(app.name);
      
      // Find primary web service
      ServiceEndpoint? primary;
      try {
        primary = services.firstWhere((s) => s.protocol == 'http' || s.name == 'web');
      } catch (_) {
        if (services.isNotEmpty) primary = services.first;
      }

      if (primary != null && mounted) {
        AppLauncher.openAppWindow(
          controller: widget.desktopController,
          appService: widget.appService,
          app: app,
          service: primary,
        );
      } else {
        _openAppDetail(app);
      }
    } catch (e) {
      _openAppDetail(app);
    }
  }

  @override
  Widget build(BuildContext context) {
    if (_isLoading) {
      return const Center(child: CircularProgressIndicator());
    }

    if (_error != null) {
      return Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            const Icon(Icons.error_outline, color: PiccoloTheme.critical, size: 48),
            const SizedBox(height: 16),
            Text(_error!, style: PiccoloTheme.textTheme.bodyMedium),
            const SizedBox(height: 16),
            OutlinedButton(
              onPressed: _loadApps,
              child: const Text("Retry"),
            ),
          ],
        ),
      );
    }

    final filteredApps = _allApps.where((app) {
      final q = widget.searchQuery.toLowerCase();
      return app.name.toLowerCase().contains(q) ||
          app.image.toLowerCase().contains(q);
    }).toList();

    if (filteredApps.isEmpty) {
      if (widget.searchQuery.isNotEmpty) {
         return const Center(child: Text("No apps match your search."));
      }
      return Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            const Icon(Icons.inventory_2_outlined, size: 64, color: PiccoloTheme.inkMuted),
            const SizedBox(height: 16),
            Text("No apps installed.", style: PiccoloTheme.textTheme.bodyLarge),
            const SizedBox(height: 8),
            const Text("Go to the Store tab to install your first app."),
          ],
        ),
      );
    }

    return GridView.builder(
      padding: const EdgeInsets.all(24),
      gridDelegate: const SliverGridDelegateWithMaxCrossAxisExtent(
        maxCrossAxisExtent: 220,
        mainAxisSpacing: 16,
        crossAxisSpacing: 16,
        childAspectRatio: 0.9,
      ),
      itemCount: filteredApps.length,
      itemBuilder: (context, index) {
        final app = filteredApps[index];
        return _AppCard(
          app: app,
          onTap: () => _launchApp(app),
        );
      },
    );
  }
}

class _AppCard extends StatelessWidget {
  final App app;
  final VoidCallback onTap;

  const _AppCard({required this.app, required this.onTap});

  @override
  Widget build(BuildContext context) {
    Color statusColor = PiccoloTheme.inkMuted;
    if (app.isRunning) statusColor = PiccoloTheme.success;
    if (app.isError) statusColor = PiccoloTheme.critical;

    return Card(
      elevation: 0,
      color: Colors.white, // White card on porcelain bg? No, Window is white/porcelain.
      // Let's use surface color.
      shape: RoundedRectangleBorder(
        borderRadius: BorderRadius.circular(12),
        side: const BorderSide(color: Colors.black12),
      ),
      child: InkWell(
        onTap: onTap,
        borderRadius: BorderRadius.circular(12),
        child: Padding(
          padding: const EdgeInsets.all(16.0),
          child: Column(
            mainAxisAlignment: MainAxisAlignment.center,
            children: [
              // Icon Placeholder
              Container(
                width: 64,
                height: 64,
                decoration: BoxDecoration(
                  color: PiccoloTheme.mist,
                  borderRadius: BorderRadius.circular(16),
                ),
                child: Center(
                    child: Text(
                        app.name.isNotEmpty ? app.name[0].toUpperCase() : "?",
                        style: const TextStyle(fontSize: 24, fontWeight: FontWeight.bold, color: PiccoloTheme.cobalt600),
                    ),
                ),
              ),
              const SizedBox(height: 16),
              // Name
              Text(
                app.name,
                style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold),
                textAlign: TextAlign.center,
                maxLines: 1,
                overflow: TextOverflow.ellipsis,
              ),
              const SizedBox(height: 8),
              // Status Badge
              Container(
                padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 4),
                decoration: BoxDecoration(
                  color: statusColor.withValues(alpha: 0.1),
                  borderRadius: BorderRadius.circular(12),
                ),
                child: Row(
                  mainAxisSize: MainAxisSize.min,
                  children: [
                    Container(
                      width: 8,
                      height: 8,
                      decoration: BoxDecoration(color: statusColor, shape: BoxShape.circle),
                    ),
                    const SizedBox(width: 6),
                    Text(
                      app.status.toUpperCase(),
                      style: TextStyle(color: statusColor, fontSize: 10, fontWeight: FontWeight.bold),
                    ),
                  ],
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
