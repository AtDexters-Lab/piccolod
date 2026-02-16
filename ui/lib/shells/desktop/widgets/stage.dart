import 'dart:async';

import 'package:flutter/material.dart';
import '../../../core/config/core_config.dart';
import '../../../core/models/app_models.dart';
import '../../../core/models/app_status_event.dart';
import '../../../features/apps/app_detail_view.dart';
import '../../../features/apps/app_launcher.dart';
import '../../../shared/widgets/action_progress_dialog.dart';
import '../../../shared/widgets/app_icon.dart';
import '../../../shared/widgets/uninstall_confirmation_dialog.dart';
import '../../../theme/piccolo_icons.dart';
import '../../../theme/piccolo_theme.dart';
import '../desktop_controller.dart';

class Stage extends StatefulWidget {
  final DesktopController controller;

  const Stage({super.key, required this.controller});

  @override
  State<Stage> createState() => _StageState();
}

/// Cached icon data for an app.
class _CachedIcon {
  /// The proxy URL to load the icon from.
  final String proxyUrl;

  /// The original icon URL from the catalog (for SVG detection).
  final String? originalUrl;

  _CachedIcon({required this.proxyUrl, this.originalUrl});
}

class _StageState extends State<Stage> {
  List<App> _apps = [];
  bool _isLoading = true;
  String? _error;

  // Icon cache: maps app name/catalogSource to cached icon data
  final Map<String, _CachedIcon?> _iconCache = {};

  // Track previous auth state to detect changes
  bool _wasAuthenticated = false;

  // Event stream subscription for reactive updates
  StreamSubscription<AppStatusEvent>? _eventSubscription;

  bool get _isAuthenticated =>
      !widget.controller.needsSetup && !widget.controller.isInitializing;

  @override
  void initState() {
    super.initState();
    widget.controller.addAppChangeListener(_loadApps);
    widget.controller.addListener(_onControllerChanged);
    _wasAuthenticated = _isAuthenticated;
    if (_isAuthenticated) {
      _loadApps();
      _subscribeToEvents();
    }
  }

  void _subscribeToEvents() {
    _eventSubscription?.cancel();
    final client = widget.controller.eventStreamClient;
    if (client != null) {
      _eventSubscription = client.appStatusEvents.listen(_handleAppStatusEvent);
    }
  }

  void _handleAppStatusEvent(AppStatusEvent event) {
    if (!mounted) return;

    // For install/uninstall events, reload the full list
    if (event.isInstalled || event.isUninstalled) {
      _loadApps();
      return;
    }

    // For status changes (running, stopped, error, starting), update in-place
    final index = _apps.indexWhere((a) => a.name == event.app);
    if (index != -1) {
      setState(() {
        _apps[index] = _apps[index].copyWithStatus(event.status, statusMessage: event.message ?? '');
      });
    }
  }

  void _onControllerChanged() {
    if (!mounted) return;
    final nowAuthenticated = _isAuthenticated;
    if (!_wasAuthenticated && nowAuthenticated) {
      // Just became authenticated - load apps and subscribe to events
      _loadApps();
      _subscribeToEvents();
    } else if (_wasAuthenticated && !nowAuthenticated) {
      // Just logged out - cancel subscription and clear apps
      _eventSubscription?.cancel();
      _eventSubscription = null;
      setState(() {
        _apps = [];
        _iconCache.clear();
        _isLoading = true;
        _error = null;
      });
    }
    _wasAuthenticated = nowAuthenticated;
  }

  @override
  void dispose() {
    _eventSubscription?.cancel();
    widget.controller.removeAppChangeListener(_loadApps);
    widget.controller.removeListener(_onControllerChanged);
    super.dispose();
  }

  Future<void> _loadApps() async {
    // Don't load if not authenticated or widget is disposed
    if (!mounted) return;
    if (widget.controller.needsSetup || widget.controller.isInitializing) {
      return;
    }

    setState(() {
      _isLoading = true;
      _error = null;
    });

    try {
      final apps = await widget.controller.appService.getApps();
      if (!mounted) return;

      // Sort alphabetically by display title
      apps.sort((a, b) =>
          a.displayTitle.toLowerCase().compareTo(b.displayTitle.toLowerCase()));

      setState(() {
        _apps = apps;
        _isLoading = false;
      });

      // Load icons from catalog for apps that don't have cached icons
      _loadAppIcons(apps);
    } catch (e) {
      if (!mounted) return;
      setState(() {
        _error = e.toString();
        _isLoading = false;
      });
    }
  }

  Future<void> _loadAppIcons(List<App> apps) async {
    // Get catalog sources for apps that need icons
    // Use catalogSource if available (tracks which catalog item this was installed from),
    // otherwise fall back to name (instanceID) which may match catalog item name
    final catalogKeys = <String>{};
    for (final app in apps) {
      final key = app.catalogSource.isNotEmpty ? app.catalogSource : app.name;
      if (key.isNotEmpty && !_iconCache.containsKey(key)) {
        catalogKeys.add(key);
      }
    }
    if (catalogKeys.isEmpty) return;

    try {
      // Fetch catalog to get icons (fetch enough to cover installed apps)
      final catalog = await widget.controller.appService.getCatalog(
        pageSize: 100,
      );
      if (!mounted) return;

      // Build icon cache from catalog items using proxy URLs
      for (final item in catalog.apps) {
        if (catalogKeys.contains(item.name)) {
          if (item.icon != null && item.icon!.isNotEmpty) {
            _iconCache[item.name] = _CachedIcon(
              proxyUrl: CoreConfig.catalogIconUrl(item.name),
              originalUrl: item.icon,
            );
          } else {
            _iconCache[item.name] = null;
          }
        }
      }

      // Mark remaining apps as having no icon (so we don't refetch)
      for (final key in catalogKeys) {
        if (!_iconCache.containsKey(key)) {
          _iconCache[key] = null;
        }
      }

      if (mounted) setState(() {});
    } catch (e) {
      debugPrint('Failed to load app icons: $e');
    }
  }

  _CachedIcon? _getAppIcon(App app) {
    // Use catalogSource for icon lookup if available, otherwise fall back to name
    final key = app.catalogSource.isNotEmpty ? app.catalogSource : app.name;
    return _iconCache[key];
  }

  void _openApp(App app) async {
    if (app.isRunning) {
      // Running app: try to open via AppLauncher with health-gating
      final services = await widget.controller.appService.getAppServices(app.name);
      if (!mounted) return;

      if (services.isEmpty) {
        // No services exposed, open detail view instead
        _openSettings(app);
        return;
      }

      // Find the primary service (first http/ws or first overall)
      ServiceEndpoint? primary;
      for (final s in services) {
        if (s.primary) {
          primary = s;
          break;
        }
      }
      primary ??= services.firstWhere(
        (s) => s.protocol == 'http' || s.protocol == 'ws',
        orElse: () => services.first,
      );

      final cachedIcon = _getAppIcon(app);
      AppLauncher.healthGatedOpen(
        context: context,
        controller: widget.controller,
        appService: widget.controller.appService,
        app: app,
        service: primary,
        iconUrl: cachedIcon?.proxyUrl,
        originalIconUrl: cachedIcon?.originalUrl,
      );
    } else {
      // Stopped app: open detail view
      _openSettings(app);
    }
  }

  void _openSettings(App app, {int initialTab = 0}) {
    final windowId = "app-detail-${app.name}";
    final cachedIcon = _getAppIcon(app);
    if (widget.controller.isAppOpen(windowId)) {
      widget.controller.focusWindow(windowId);
    } else {
      widget.controller.openApp(
        windowId,
        app.displayTitle,
        PiccoloIcons.settingsApp,
        AppDetailView(
          appId: app.name,
          appService: widget.controller.appService,
          desktopController: widget.controller,
          initialTab: initialTab,
          iconUrl: cachedIcon?.proxyUrl,
          originalIconUrl: cachedIcon?.originalUrl,
        ),
        iconUrl: cachedIcon?.proxyUrl,
        originalIconUrl: cachedIcon?.originalUrl,
      );
    }
  }

  Future<void> _startApp(App app) async {
    await runWithProgressDialog(
      context: context,
      title: 'Starting App',
      taskType: 'start_app',
      action: (taskId) =>
          widget.controller.appService.startApp(app.name, taskId: taskId),
    );
    // notifyAppsChanged triggers _loadApps via the listener
    widget.controller.notifyAppsChanged();
  }

  Future<void> _stopApp(App app) async {
    await runWithProgressDialog(
      context: context,
      title: 'Stopping App',
      taskType: 'stop_app',
      action: (taskId) =>
          widget.controller.appService.stopApp(app.name, taskId: taskId),
    );
    widget.controller.notifyAppsChanged();
  }

  void _confirmUninstall(App app) async {
    final confirmed = await UninstallConfirmationDialog.show(
      context,
      appDisplayTitle: app.displayTitle,
    );
    if (!mounted) return;

    if (confirmed == true) {
      final ok = await runWithProgressDialog(
        context: context,
        title: 'Uninstalling App',
        taskType: 'uninstall_app',
        action: (taskId) => widget.controller.appService.uninstallApp(
          app.name,
          taskId: taskId,
        ),
      );

      if (ok) {
        final windowId = "app-detail-${app.name}";
        if (widget.controller.isAppOpen(windowId)) {
          widget.controller.closeWindow(windowId);
        }
      }
      widget.controller.notifyAppsChanged();
    }
  }

  void _showContextMenu(BuildContext context, App app, Offset position) {
    final RenderBox overlay =
        Overlay.of(context).context.findRenderObject() as RenderBox;

    showMenu<String>(
      context: context,
      position: RelativeRect.fromRect(
        position & const Size(40, 40),
        Offset.zero & overlay.size,
      ),
      shape: RoundedRectangleBorder(
        borderRadius: BorderRadius.circular(Radii.md),
      ),
      color: PiccoloTheme.porcelain,
      elevation: 2,
      constraints: const BoxConstraints(minWidth: 180),
      items: [
        PopupMenuItem(
          value: 'open',
          height: 44,
          child: Row(
            children: [
              Icon(PiccoloIcons.openExternal, size: 18, color: PiccoloTheme.ink),
              const SizedBox(width: 12),
              Text('Open', style: TextStyle(color: PiccoloTheme.ink)),
            ],
          ),
        ),
        PopupMenuItem(
          value: 'settings',
          height: 44,
          child: Row(
            children: [
              Icon(PiccoloIcons.settings, size: 18, color: PiccoloTheme.ink),
              const SizedBox(width: 12),
              Text('Settings', style: TextStyle(color: PiccoloTheme.ink)),
            ],
          ),
        ),
        PopupMenuItem(
          value: 'logs',
          height: 44,
          child: Row(
            children: [
              Icon(PiccoloIcons.article, size: 18, color: PiccoloTheme.ink),
              const SizedBox(width: 12),
              Text('Logs', style: TextStyle(color: PiccoloTheme.ink)),
            ],
          ),
        ),
        const PopupMenuDivider(height: 8),
        if (app.isRunning)
          PopupMenuItem(
            value: 'stop',
            height: 44,
            child: Row(
              children: [
                Icon(PiccoloIcons.stop, size: 18, color: PiccoloTheme.ink),
                const SizedBox(width: 12),
                Text('Stop', style: TextStyle(color: PiccoloTheme.ink)),
              ],
            ),
          )
        else
          PopupMenuItem(
            value: 'start',
            height: 44,
            child: Row(
              children: [
                Icon(PiccoloIcons.play, size: 18, color: PiccoloTheme.ink),
                const SizedBox(width: 12),
                Text('Start', style: TextStyle(color: PiccoloTheme.ink)),
              ],
            ),
          ),
        PopupMenuItem(
          value: 'uninstall',
          height: 44,
          child: Row(
            children: [
              Icon(PiccoloIcons.delete, size: 18, color: PiccoloTheme.critical),
              const SizedBox(width: 12),
              Text(
                'Uninstall',
                style: TextStyle(color: PiccoloTheme.critical),
              ),
            ],
          ),
        ),
      ],
    ).then((value) {
      if (value == null) return;
      switch (value) {
        case 'open':
          _openApp(app);
          break;
        case 'settings':
          _openSettings(app);
          break;
        case 'logs':
          _openSettings(app, initialTab: AppDetailView.tabLogs);
          break;
        case 'start':
          _startApp(app);
          break;
        case 'stop':
          _stopApp(app);
          break;
        case 'uninstall':
          _confirmUninstall(app);
          break;
      }
    });
  }

  @override
  Widget build(BuildContext context) {
    return Stack(
      children: [
        // Background image
        Positioned.fill(
          child: Image.asset(
            'assets/piccolo_bg.jpg',
            fit: BoxFit.cover,
            errorBuilder: (context, error, stackTrace) {
              // Fallback to gradient if image fails to load
              return Container(
                decoration: const BoxDecoration(
                  gradient: LinearGradient(
                    begin: Alignment.topLeft,
                    end: Alignment.bottomRight,
                    colors: [
                      Color(0xFFF6F8FC),
                      Color(0xFFE9EEF6),
                      Color(0xFFE4EAF3),
                    ],
                  ),
                ),
              );
            },
          ),
        ),

        // App grid overlay - only show when authenticated
        if (!widget.controller.needsSetup && !widget.controller.isInitializing)
          Positioned.fill(
            child: _buildContent(),
          ),
      ],
    );
  }

  Widget _buildContent() {
    if (_isLoading) {
      return const Center(
        child: CircularProgressIndicator(color: Colors.white),
      );
    }

    if (_error != null) {
      return Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            Icon(
              PiccoloIcons.error,
              size: 48,
              color: Colors.white.withValues(alpha: 0.8),
            ),
            const SizedBox(height: 16),
            Text(
              'Failed to load apps',
              style: TextStyle(
                color: Colors.white.withValues(alpha: 0.9),
                fontSize: 16,
              ),
            ),
            const SizedBox(height: 8),
            OutlinedButton(
              onPressed: _loadApps,
              style: OutlinedButton.styleFrom(
                foregroundColor: Colors.white,
                side: const BorderSide(color: Colors.white54),
              ),
              child: const Text('Retry'),
            ),
          ],
        ),
      );
    }

    if (_apps.isEmpty) {
      return Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            Icon(
              PiccoloIcons.apps,
              size: 64,
              color: Colors.white.withValues(alpha: 0.6),
            ),
            const SizedBox(height: 16),
            Text(
              'No apps installed',
              style: TextStyle(
                color: Colors.white.withValues(alpha: 0.9),
                fontSize: 18,
                fontWeight: FontWeight.w500,
              ),
            ),
            const SizedBox(height: 8),
            Text(
              'Visit the App Store to get started',
              style: TextStyle(
                color: Colors.white.withValues(alpha: 0.7),
                fontSize: 14,
              ),
            ),
            const SizedBox(height: 24),
            FilledButton.icon(
              onPressed: () => widget.controller.openAppStore(),
              icon: const Icon(PiccoloIcons.store),
              label: const Text('Open App Store'),
              style: FilledButton.styleFrom(
                backgroundColor: PiccoloTheme.cobalt600,
                foregroundColor: Colors.white,
              ),
            ),
          ],
        ),
      );
    }

    // Centered grid of app tiles
    return Center(
      child: SingleChildScrollView(
        padding: const EdgeInsets.fromLTRB(48, 32, 48, 140), // Space for dock
        child: Wrap(
          alignment: WrapAlignment.center,
          spacing: 24,
          runSpacing: 24,
          children: [
            // App tiles
            ..._apps.map((app) {
              final cachedIcon = _getAppIcon(app);
              return _AppTile(
                app: app,
                iconUrl: cachedIcon?.proxyUrl,
                originalIconUrl: cachedIcon?.originalUrl,
                onTap: () => _openApp(app),
                onSecondaryTap: (position) =>
                    _showContextMenu(context, app, position),
              );
            }),
            // Add tile
            _AddTile(onTap: () => widget.controller.openAppStore()),
          ],
        ),
      ),
    );
  }
}

class _AppTile extends StatefulWidget {
  final App app;
  final String? iconUrl;
  final String? originalIconUrl;
  final VoidCallback onTap;
  final void Function(Offset position) onSecondaryTap;

  const _AppTile({
    required this.app,
    this.iconUrl,
    this.originalIconUrl,
    required this.onTap,
    required this.onSecondaryTap,
  });

  @override
  State<_AppTile> createState() => _AppTileState();
}

class _AppTileState extends State<_AppTile> {
  bool _isHovered = false;

  Color get _statusColor {
    if (widget.app.isRunning) return PiccoloTheme.success;
    if (widget.app.isError) return PiccoloTheme.critical;
    return PiccoloTheme.inkMuted;
  }

  @override
  Widget build(BuildContext context) {
    return MouseRegion(
      onEnter: (_) => setState(() => _isHovered = true),
      onExit: (_) => setState(() => _isHovered = false),
      cursor: SystemMouseCursors.click,
      child: GestureDetector(
        onTap: widget.onTap,
        onSecondaryTapUp: (details) =>
            widget.onSecondaryTap(details.globalPosition),
        child: AnimatedScale(
          scale: _isHovered ? 1.08 : 1.0,
          duration: Motion.medium,
          curve: Curves.easeOut,
          child: SizedBox(
            width: 100,
            height: 120,
            child: Column(
              mainAxisSize: MainAxisSize.min,
              children: [
                // Icon container
                AnimatedContainer(
                  duration: Motion.medium,
                  width: 64,
                  height: 64,
                  decoration: BoxDecoration(
                    color: Colors.white
                        .withValues(alpha: _isHovered ? 1.0 : 0.9),
                    borderRadius: BorderRadius.circular(16),
                    boxShadow: [
                      BoxShadow(
                        color: Colors.black.withValues(
                          alpha: _isHovered ? 0.25 : 0.15,
                        ),
                        blurRadius: _isHovered ? 12 : 8,
                        offset: Offset(0, _isHovered ? 4 : 2),
                      ),
                    ],
                  ),
                  child: Stack(
                    children: [
                      // App icon using AppIcon widget with SVG support
                      Center(
                        child: AppIcon(
                          proxyUrl: widget.iconUrl,
                          originalIconUrl: widget.originalIconUrl,
                          size: 48,
                          borderRadius: 12,
                          fallbackText: widget.app.displayTitle.isNotEmpty
                              ? widget.app.displayTitle[0]
                              : '?',
                          fallbackBackgroundColor: Colors.transparent,
                        ),
                      ),
                      // Status dot
                      Positioned(
                        right: 4,
                        bottom: 4,
                        child: Container(
                          width: 12,
                          height: 12,
                          decoration: BoxDecoration(
                            color: _statusColor,
                            shape: BoxShape.circle,
                            border: Border.all(color: Colors.white, width: 2),
                          ),
                        ),
                      ),
                    ],
                  ),
                ),
                const SizedBox(height: 8),
                // App name
                Container(
                  padding:
                      const EdgeInsets.symmetric(horizontal: 8, vertical: 4),
                  decoration: BoxDecoration(
                    color: PiccoloTheme.scrim,
                    borderRadius: BorderRadius.circular(Radii.sm),
                  ),
                  child: Text(
                    widget.app.displayTitle,
                    maxLines: 2,
                    overflow: TextOverflow.ellipsis,
                    textAlign: TextAlign.center,
                    style: const TextStyle(
                      color: Colors.white,
                      fontSize: 12,
                      fontWeight: FontWeight.w500,
                    ),
                  ),
                ),
              ],
            ),
          ),
        ),
      ),
    );
  }
}

class _AddTile extends StatefulWidget {
  final VoidCallback onTap;

  const _AddTile({required this.onTap});

  @override
  State<_AddTile> createState() => _AddTileState();
}

class _AddTileState extends State<_AddTile> {
  bool _isHovered = false;

  @override
  Widget build(BuildContext context) {
    return MouseRegion(
      onEnter: (_) => setState(() => _isHovered = true),
      onExit: (_) => setState(() => _isHovered = false),
      cursor: SystemMouseCursors.click,
      child: GestureDetector(
        onTap: widget.onTap,
        child: AnimatedScale(
          scale: _isHovered ? 1.08 : 1.0,
          duration: Motion.medium,
          curve: Curves.easeOut,
          child: SizedBox(
            width: 100,
            height: 120,
            child: Column(
              mainAxisSize: MainAxisSize.min,
              children: [
                // Icon container
                AnimatedContainer(
                  duration: Motion.medium,
                  width: 64,
                  height: 64,
                  decoration: BoxDecoration(
                    color: Colors.white.withValues(alpha: _isHovered ? 0.5 : 0.3),
                    borderRadius: BorderRadius.circular(16),
                    border: Border.all(
                      color: Colors.white.withValues(alpha: _isHovered ? 0.8 : 0.5),
                      width: 2,
                    ),
                  ),
                  child: Icon(
                    PiccoloIcons.add,
                    size: 32,
                    color: Colors.white.withValues(alpha: _isHovered ? 1.0 : 0.9),
                  ),
                ),
                const SizedBox(height: 8),
                // Label
                Container(
                  padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 4),
                  decoration: BoxDecoration(
                    color: PiccoloTheme.scrim,
                    borderRadius: BorderRadius.circular(Radii.sm),
                  ),
                  child: const Text(
                    'App Store',
                    textAlign: TextAlign.center,
                    style: TextStyle(
                      color: Colors.white,
                      fontSize: 12,
                      fontWeight: FontWeight.w500,
                    ),
                  ),
                ),
              ],
            ),
          ),
        ),
      ),
    );
  }
}
