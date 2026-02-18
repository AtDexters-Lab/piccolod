import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/core/models/app_status_event.dart';
import 'package:piccolo_os/core/models/listener_health.dart';
import 'package:piccolo_os/core/services/app_service.dart';
import 'package:piccolo_os/core/utils/task_id.dart';
import 'package:piccolo_os/features/apps/app_launcher.dart';
import 'package:piccolo_os/features/apps/widgets/edit_listeners_dialog.dart';
import 'package:piccolo_os/features/apps/widgets/health_banner.dart';
import 'package:piccolo_os/features/apps/workspace_terminal.dart';
import 'package:piccolo_os/shared/widgets/app_icon.dart';
import 'package:piccolo_os/shared/widgets/health_badge.dart';
import 'package:piccolo_os/shared/widgets/log_stream_viewer.dart';
import 'package:piccolo_os/shared/widgets/status_banner.dart';
import 'package:piccolo_os/shared/widgets/task_progress_panel.dart';
import 'package:piccolo_os/shared/widgets/uninstall_confirmation_dialog.dart';
import 'package:piccolo_os/shells/desktop/desktop_controller.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:url_launcher/url_launcher.dart';

class AppDetailView extends StatefulWidget {

  const AppDetailView({
    required this.appId, required this.appService, required this.desktopController, super.key,
    this.initialTab = 0,
    this.iconUrl,
    this.originalIconUrl,
  });
  static const int tabOverview = 0;
  static const int tabNetwork = 1;
  static const int tabConfiguration = 2;
  static const int tabLogs = 3;

  final String appId;
  final AppService appService;
  final DesktopController desktopController;
  final int initialTab;
  final String? iconUrl;
  final String? originalIconUrl;

  @override
  State<AppDetailView> createState() => _AppDetailViewState();
}

class _AppDetailViewState extends State<AppDetailView>
    with SingleTickerProviderStateMixin {
  late TabController _tabController;

  App? _app;
  List<ServiceEndpoint> _listeners = [];
  List<AppContainerStatus> _containers = [];
  String? _selectedService;
  bool _isLoading = true;
  String? _error;

  // Action states
  bool _isActionLoading = false;

  // SSE streams (via unified EventStreamClient)
  StreamSubscription<AppStatusEvent>? _statusSub;
  StreamSubscription<ListenerHealthEvent>? _healthSub;
  ListenerHealth? _primaryHealth;

  @override
  void initState() {
    super.initState();
    _tabController = TabController(
      length: 4,
      vsync: this,
      initialIndex: widget.initialTab.clamp(0, 3),
    );
    unawaited(_loadData());
    _connectStatusStream();
    _connectHealthStream();
  }

  String? _primaryListenerName;

  void _connectStatusStream() {
    final client = widget.desktopController.eventStreamClient;
    if (client == null) return;

    _statusSub = client.appStatusEvents.listen((event) {
      if (!mounted) return;
      if (event.app != widget.appId) return;
      setState(() {
        _app = _app?.copyWithStatus(event.status, statusMessage: event.message ?? '');
      });
    });
  }

  void _connectHealthStream() {
    final client = widget.desktopController.eventStreamClient;
    if (client == null) return;

    _healthSub = client.healthEvents.listen((event) {
      if (!mounted) return;
      // Only process events for this specific app
      if (event.app != widget.appId) return;
      // Only update primary health from the primary listener's events
      if (_primaryListenerName != null &&
          event.listener != _primaryListenerName) {
        return;
      }
      setState(() {
        _primaryHealth = event.health;
      });
    });
  }

  @override
  void dispose() {
    unawaited(_statusSub?.cancel());
    unawaited(_healthSub?.cancel());
    _tabController.dispose();
    super.dispose();
  }

  Future<void> _loadData() async {
    setState(() {
      _isLoading = true;
      _error = null;
    });

    try {
      final detail = await widget.appService.getAppDetail(widget.appId);

      if (!mounted) return;

      setState(() {
        _app = detail.app;
        _listeners = detail.listeners;
        _containers = detail.containers;
        _primaryHealth = detail.app.primaryListenerHealth;
        // Identify the primary listener name for stream filtering
        _primaryListenerName = _findPrimaryListenerName(detail.listeners);
        _selectedService = _pickSelectedService(
          _selectedService,
          detail.containers,
        );
        _isLoading = false;
      });
    } on Object catch (e) {
      if (!mounted) return;
      setState(() {
        _error = e.toString();
        _isLoading = false;
      });
    }
  }

  Future<bool> _handleActionWithProgress({
    required String taskType,
    required Future<void> Function(String taskId) action,
    bool refreshOnSuccess = true,
  }) async {
    if (!mounted) return false;

    final taskId = generateTaskId();
    final progressDone = Completer<void>();

    unawaited(showDialog<void>(
      context: context,
      barrierDismissible: false,
      builder: (context) => AlertDialog(
        title: Text(taskType),
        content: SizedBox(
          width: 520,
          child: TaskProgressPanel(
            taskId: taskId,
            taskType: taskType,
            onComplete: (_) {
              if (!progressDone.isCompleted) progressDone.complete();
            },
          ),
        ),
      ),
    ));

    setState(() => _isActionLoading = true);
    Object? actionError;
    try {
      await action(taskId);
      await progressDone.future.timeout(
        const Duration(seconds: 2),
        onTimeout: () {},
      );
    } on Object catch (e) {
      actionError = e;
    } finally {
      if (mounted) setState(() => _isActionLoading = false);
      if (mounted && Navigator.of(context).canPop()) {
        Navigator.of(context).pop();
      }
    }

    if (actionError != null && mounted) {
      ScaffoldMessenger.of(
        context,
      ).showSnackBar(SnackBar(content: Text('Action failed: $actionError')));
      return false;
    }

    // Notify other widgets (e.g., Stage) that app state changed
    widget.desktopController.notifyAppsChanged();

    if (!refreshOnSuccess || !mounted) return true;
    await _loadData();
    return true;
  }

  Future<void> _confirmUninstall() async {
    final confirmed = await UninstallConfirmationDialog.show(
      context,
      appDisplayTitle: _app?.displayTitle ?? widget.appId,
    );

    if (confirmed != true) return;

    final ok = await _handleActionWithProgress(
      taskType: 'uninstall_app',
      refreshOnSuccess: false,
      action: (taskId) => widget.appService.uninstallApp(
        widget.appId,
        taskId: taskId,
      ),
    );
    if (!ok || !mounted) return;
    setState(() {
      _app = null;
      _listeners = [];
      _containers = [];
      _selectedService = null;
    });
  }

  void _showEditListenersDialog() {
    if (_app == null) return;

    // Convert current services to AppListener list
    final initialListeners = _listeners
        .map(AppListener.fromServiceEndpoint)
        .toList();

    unawaited(showDialog<void>(
      context: context,
      builder: (context) => EditListenersDialog(
        initialListeners: initialListeners,
        onSave: (newListeners) async {
          await _handleActionWithProgress(
            taskType: 'update_listeners',
            action: (taskId) => widget.appService.updateAppListeners(
              _app!.name,
              newListeners,
              taskId: taskId,
            ),
          );
        },
      ),
    ));
  }

  void _openTerminal() {
    if (_app == null) return;

    final windowId = 'terminal-${_app!.name}';
    widget.desktopController.openApp(
      windowId,
      '${_app!.displayTitle} Terminal',
      PiccoloIcons.terminal,
      WorkspaceTerminal(
        appId: _app!.name,
        serviceName: _selectedService,
        onSessionEnd: () => widget.desktopController.closeWindow(windowId),
      ),
    );
  }

  @override
  Widget build(BuildContext context) {
    // Removed Scaffold/AppBar to fit into WindowFrame naturally
    return _buildBody();
  }

  Widget _buildBody() {
    if (_isLoading) return const Center(child: CircularProgressIndicator());
    if (_error != null) return Center(child: Text('Error: $_error'));
    if (_app == null) return const Center(child: Text('App uninstalled.'));

    return Column(
      children: [
        // Header
        _buildHeader(),

        // Starting status banner
        if (_app!.isStarting)
          _buildStartingBanner(),

        // Error status banner
        if (_app!.isError)
          _buildErrorBanner(),

        // Health banner
        if (_primaryHealth != null && !_primaryHealth!.isOk)
          AppDetailHealthBanner(
            health: _primaryHealth!,
            lanFallbackUrl: _getLanFallbackUrl(),
            appService: widget.appService,
            desktopController: widget.desktopController,
          ),

        // Tabs
        TabBar(
          controller: _tabController,
          labelColor: PiccoloTheme.cobalt600,
          unselectedLabelColor: PiccoloTheme.inkMuted,
          indicatorColor: PiccoloTheme.cobalt600,
          tabs: const [
            Tab(text: 'Overview'),
            Tab(text: 'Network'),
            Tab(text: 'Configuration'),
            Tab(text: 'Logs'),
          ],
        ),

        // Content
        Expanded(
          child: ColoredBox(
            color: PiccoloTheme.mist,
            child: TabBarView(
              controller: _tabController,
              children: [
                _buildOverviewTab(),
                _buildNetworkTab(),
                _buildConfigTab(),
                _buildLogsTab(),
              ],
            ),
          ),
        ),
      ],
    );
  }

  Widget _buildHeader() {
    var statusColor = PiccoloTheme.inkMuted;
    if (_app!.isRunning) statusColor = PiccoloTheme.success;
    if (_app!.isStarting) statusColor = PiccoloTheme.warning;
    if (_app!.isError) statusColor = PiccoloTheme.critical;

    return Container(
      color: PiccoloTheme.porcelain,
      padding: const EdgeInsets.all(Spacing.lg),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          // Icon
          Container(
            width: 80,
            height: 80,
            decoration: BoxDecoration(
              color: PiccoloTheme.mist,
              borderRadius: BorderRadius.circular(Radii.lg),
            ),
            child: Center(
              child: AppIcon(
                proxyUrl: widget.iconUrl,
                originalIconUrl: widget.originalIconUrl,
                size: 64,
                borderRadius: 12,
                fallbackText: _app!.displayTitle.isNotEmpty
                    ? _app!.displayTitle[0]
                    : '?',
                fallbackBackgroundColor: Colors.transparent,
              ),
            ),
          ),
          const SizedBox(width: Spacing.lg),

          // Info
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(
                  _app!.displayTitle,
                  style: PiccoloTheme.textTheme.displayLarge?.copyWith(
                    fontSize: 24,
                  ),
                ),
                const SizedBox(height: Spacing.sm),
                Row(
                  children: [
                    Container(
                      padding: const EdgeInsets.symmetric(
                        horizontal: Spacing.sm,
                        vertical: Spacing.xs,
                      ),
                      decoration: BoxDecoration(
                        color: statusColor.withValues(alpha: 0.1),
                        borderRadius: BorderRadius.circular(Radii.md),
                      ),
                      child: Row(
                        mainAxisSize: MainAxisSize.min,
                        children: [
                          Container(
                            width: 8,
                            height: 8,
                            decoration: BoxDecoration(
                              color: statusColor,
                              shape: BoxShape.circle,
                            ),
                          ),
                          const SizedBox(width: 6),
                          Text(
                            _app!.status.toUpperCase(),
                            style: TextStyle(
                              color: statusColor,
                              fontSize: 12,
                              fontWeight: FontWeight.bold,
                            ),
                          ),
                        ],
                      ),
                    ),
                    const SizedBox(width: Spacing.base),
                    Text(
                      'Image: ${_app!.image}',
                      style: PiccoloTheme.textTheme.labelSmall,
                    ),
                  ],
                ),
              ],
            ),
          ),

          const SizedBox(width: Spacing.lg),

          // Actions
          if (_isActionLoading)
            const CircularProgressIndicator()
          else
            Row(
              mainAxisSize: MainAxisSize.min,
              children: [
                if (_containers.length > 1) ...[
                  _buildServiceSelector(),
                  const SizedBox(width: Spacing.md),
                ],
                if (_app!.isRunning) ...[
                  FilledButton.icon(
                    onPressed: _openTerminal,
                    icon: const Icon(PiccoloIcons.terminal),
                    label: const Text('Terminal'),
                    style: FilledButton.styleFrom(
                      backgroundColor: PiccoloTheme.cobalt600,
                    ),
                  ),
                  const SizedBox(width: Spacing.md),
                ],
                if (_app!.isRunning)
                  FilledButton.icon(
                    onPressed: () => _handleActionWithProgress(
                      taskType: 'stop_app',
                      action: (taskId) =>
                          widget.appService.stopApp(_app!.name, taskId: taskId),
                    ),
                    icon: const Icon(PiccoloIcons.stop),
                    label: const Text('Stop'),
                    style: FilledButton.styleFrom(
                      backgroundColor: PiccoloTheme.inkMuted,
                    ),
                  )
                else
                  FilledButton.icon(
                    onPressed: () => _handleActionWithProgress(
                      taskType: 'start_app',
                      action: (taskId) =>
                          widget.appService.startApp(_app!.name, taskId: taskId),
                    ),
                    icon: const Icon(PiccoloIcons.play),
                    label: const Text('Start'),
                    style: FilledButton.styleFrom(
                      backgroundColor: PiccoloTheme.success,
                    ),
                  ),
                const SizedBox(width: Spacing.md),
                IconButton.outlined(
                  onPressed: _confirmUninstall,
                  icon: const Icon(PiccoloIcons.delete, size: 20),
                  tooltip: 'Uninstall',
                  style: IconButton.styleFrom(
                    foregroundColor: PiccoloTheme.critical,
                    side: BorderSide(
                      color: PiccoloTheme.critical.withValues(alpha: 0.3),
                    ),
                  ),
                ),
              ],
            ),
        ],
      ),
    );
  }

  static String? _findPrimaryListenerName(List<ServiceEndpoint> listeners) {
    // Prefer the endpoint marked as primary
    for (final l in listeners) {
      if (l.primary) return l.name;
    }
    // RFC: first HTTP or WebSocket listener; null for raw-only apps
    for (final l in listeners) {
      if (l.protocol == 'http' || l.protocol == 'ws') return l.name;
    }
    return null;
  }

  String _getLanFallbackUrl() {
    // Prefer the primary listener's fallback URL
    final primary = _primaryListenerName;
    if (primary != null) {
      for (final l in _listeners) {
        if (l.name == primary) {
          return l.lanFallbackUrl ?? l.lanHostUrl ?? l.localUrl ?? '';
        }
      }
    }
    // Fallback: first listener with a URL
    for (final l in _listeners) {
      if (l.lanFallbackUrl != null) return l.lanFallbackUrl!;
    }
    for (final l in _listeners) {
      if (l.lanHostUrl != null) return l.lanHostUrl!;
    }
    for (final l in _listeners) {
      if (l.localUrl != null) return l.localUrl!;
    }
    return '';
  }

  Widget _buildOverviewTab() {
    return ListView(
      padding: const EdgeInsets.all(Spacing.lg),
      children: [
        _buildSectionTitle('Storage Volumes'),
        if (_app!.volumes.isEmpty)
          const Text('No persistent volumes configured.')
        else
          ..._app!.volumes.map(
            (v) => Card(
              child: ListTile(
                leading: const Icon(PiccoloIcons.storage),
                title: Text(v.containerPath),
                subtitle: Text('Host: ${v.hostPath}'),
                trailing: Text(v.sizeLimit),
              ),
            ),
          ),

        const SizedBox(height: Spacing.lg),
        _buildSectionTitle(
          _containers.length > 1 && (_selectedService?.isNotEmpty ?? false)
              ? 'Environment Variables (${_selectedService!})'
              : 'Environment Variables',
        ),
        if (_app!.environmentForService(_selectedService).isEmpty)
          const Text('No environment variables.')
        else
          Container(
            padding: const EdgeInsets.all(Spacing.base),
            decoration: BoxDecoration(
              color: PiccoloTheme.porcelain,
              borderRadius: BorderRadius.circular(Radii.sm),
              border: Border.all(color: PiccoloTheme.hairline),
            ),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: _app!
                  .environmentForService(_selectedService)
                  .entries
                  .map(
                    (e) => Padding(
                      padding: const EdgeInsets.symmetric(vertical: Spacing.xs),
                      child: Row(
                        crossAxisAlignment: CrossAxisAlignment.start,
                        children: [
                          SizedBox(
                            width: 120,
                            child: SelectableText(
                              e.key,
                              style: const TextStyle(
                                fontWeight: FontWeight.bold,
                                fontFamily: 'JetBrainsMono',
                                fontSize: 12,
                              ),
                            ),
                          ),
                          Expanded(
                            child: SelectableText(
                              e.value,
                              style: const TextStyle(
                                fontFamily: 'JetBrainsMono',
                                fontSize: 12,
                              ),
                            ),
                          ),
                        ],
                      ),
                    ),
                  )
                  .toList(),
            ),
          ),
      ],
    );
  }

  Widget _buildNetworkTab() {
    final content = _listeners.isEmpty
        ? const Center(child: Text('No network services exposed.'))
        : ListView(
            padding: const EdgeInsets.all(Spacing.lg),
            children: _listeners
                .map(
                  (svc) => Card(
                    child: Padding(
                      padding: const EdgeInsets.all(Spacing.base),
                      child: Column(
                        crossAxisAlignment: CrossAxisAlignment.start,
                        children: [
                          Row(
                            children: [
                              const Icon(
                                PiccoloIcons.router,
                                color: PiccoloTheme.cobalt600,
                              ),
                              const SizedBox(width: Spacing.md),
                              Text(
                                svc.name,
                                style: const TextStyle(
                                  fontWeight: FontWeight.bold,
                                  fontSize: 16,
                                ),
                              ),
                              if (svc.health != null && !svc.health!.isOk) ...[
                                const SizedBox(width: Spacing.sm),
                                HealthBadge(health: svc.health),
                              ],
                              const Spacer(),
                              Container(
                                padding: const EdgeInsets.symmetric(
                                  horizontal: Spacing.sm,
                                  vertical: 2,
                                ),
                                decoration: BoxDecoration(
                                  color: PiccoloTheme.mist,
                                  borderRadius: BorderRadius.circular(Radii.xxs),
                                  border: Border.all(color: PiccoloTheme.hairline),
                                ),
                                child: Text(
                                  svc.protocol.toUpperCase(),
                                  style: const TextStyle(fontSize: 12),
                                ),
                              ),
                            ],
                          ),
                          const Divider(height: Spacing.lg),
                          _buildNetworkRow('Internal Port', '${svc.guestPort}'),
                          if (svc.lanHostUrl != null)
                            Builder(builder: (_) {
                              final host = Uri.base.host.toLowerCase();
                              // When accessed via IP, prefer localUrl (port-based) since
                              // mDNS hostname won't resolve for this user.
                              final useLocal = AppLauncher.isIpAddress(host) ||
                                  AppLauncher.isLoopback(host);
                              final url = useLocal
                                  ? (svc.localUrl ?? svc.lanHostUrl!)
                                  : svc.lanHostUrl!;
                              return _buildNetworkLinkRow(
                                'LAN Access',
                                url,
                                onTap: () => launchUrl(Uri.parse(url)),
                                icon: PiccoloIcons.openExternal,
                                tooltip: 'Opens in new tab',
                              );
                            }),
                          if (svc.localUrl != null)
                            _buildNetworkLinkRow(
                              svc.lanHostUrl != null
                                  ? 'LAN Fallback'
                                  : 'LAN Access',
                              '${svc.localUrl} (Port ${svc.publicPort})',
                              onTap: () => AppLauncher.openAppWindow(
                                controller: widget.desktopController,
                                appService: widget.appService,
                                app: _app!,
                                service: svc,
                              ),
                              icon: PiccoloIcons.webAsset,
                              tooltip: 'Opens in app window',
                            ),
                          if (svc.remoteUrl != null)
                            _buildNetworkLinkRow(
                              'Remote Access',
                              svc.remoteUrl!,
                              onTap: () => AppLauncher.healthGatedOpen(
                                context: context,
                                controller: widget.desktopController,
                                appService: widget.appService,
                                app: _app!,
                                service: svc,
                                overrideUrl: svc.remoteUrl,
                                // Only override with live stream health for primary listener
                                healthOverride: svc.name == _primaryListenerName
                                    ? _primaryHealth
                                    : null,
                              ),
                              icon: PiccoloIcons.webAsset,
                              tooltip: 'Opens in app window',
                            ),
                        ],
                      ),
                    ),
                  ),
                )
                .toList(),
          );

    return Column(
      children: [
        if (_app!.isWorkspace)
          Padding(
            padding: const EdgeInsets.fromLTRB(Spacing.lg, Spacing.lg, Spacing.lg, 0),
            child: Row(
              mainAxisAlignment: MainAxisAlignment.end,
              children: [
                OutlinedButton.icon(
                  onPressed: _showEditListenersDialog,
                  icon: const Icon(PiccoloIcons.edit, size: 16),
                  label: const Text('Edit Listeners'),
                ),
              ],
            ),
          ),
        Expanded(child: content),
      ],
    );
  }

  Widget _buildConfigTab() {
    // Ideally this would show the original YAML, but the API doesn't return it yet.
    // We can just show a JSON dump of the App object for now.
    return Padding(
      padding: const EdgeInsets.all(Spacing.lg),
      child: Container(
        padding: const EdgeInsets.all(Spacing.base),
        decoration: BoxDecoration(
          color: PiccoloTheme.porcelain,
          borderRadius: BorderRadius.circular(Radii.sm),
          border: Border.all(color: PiccoloTheme.hairline),
        ),
        child: SelectableText(
          'App ID: ${_app!.id}\n'
          'Type: ${_app!.type}\n'
          'Container ID: ${_app!.containerId}\n',
          style: const TextStyle(fontFamily: 'JetBrainsMono'),
        ),
      ),
    );
  }

  Widget _buildLogsTab() {
    return Padding(
      padding: const EdgeInsets.all(Spacing.lg),
      child: LogStreamViewer(
        appName: _app!.name,
        serviceName: _selectedService,
      ),
    );
  }

  String? _pickSelectedService(
    String? current,
    List<AppContainerStatus> containers,
  ) {
    if (containers.isEmpty) return null;
    if (current != null && containers.any((c) => c.service == current)) {
      return current;
    }
    return containers.first.service;
  }

  Widget _buildServiceSelector() {
    if (_containers.length <= 1) return const SizedBox.shrink();

    var selected = _selectedService;
    if (selected == null || !_containers.any((c) => c.service == selected)) {
      selected = _containers.first.service;
    }

    return Container(
      padding: const EdgeInsets.symmetric(horizontal: Spacing.md, vertical: Spacing.xs),
      decoration: BoxDecoration(
        color: PiccoloTheme.mist,
        borderRadius: BorderRadius.circular(Radii.sm),
        border: Border.all(color: PiccoloTheme.hairline),
      ),
      child: DropdownButtonHideUnderline(
        child: DropdownButton<String>(
          value: selected,
          isDense: true,
          items: _containers
              .map(
                (c) => DropdownMenuItem<String>(
                  value: c.service,
                  child: Text(c.running ? c.service : '${c.service} (stopped)'),
                ),
              )
              .toList(),
          onChanged: (value) => setState(() => _selectedService = value),
        ),
      ),
    );
  }

  Widget _buildStartingBanner() {
    return StatusBanner(
      severity: BannerSeverity.warning,
      icon: PiccoloIcons.hourglass,
      title: 'App is starting...',
      message: _app!.statusMessage.isNotEmpty
          ? _app!.statusMessage
          : 'The app is initializing. Check logs if startup takes too long.',
      action: TextButton.icon(
        onPressed: () => _tabController.animateTo(AppDetailView.tabLogs),
        icon: const Icon(PiccoloIcons.article, size: 16),
        label: const Text('View Logs'),
      ),
    );
  }

  Widget _buildErrorBanner() {
    return StatusBanner(
      severity: BannerSeverity.error,
      title: 'App failed to start',
      message: _app!.statusMessage.isNotEmpty
          ? _app!.statusMessage
          : 'The app failed to start. Check logs or try restarting.',
      action: TextButton.icon(
        onPressed: () => _tabController.animateTo(AppDetailView.tabLogs),
        icon: const Icon(PiccoloIcons.article, size: 16),
        label: const Text('View Logs'),
      ),
    );
  }

  Widget _buildSectionTitle(String title) {
    return Padding(
      padding: const EdgeInsets.only(bottom: Spacing.md),
      child: Text(
        title,
        style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
          fontWeight: FontWeight.bold,
        ),
      ),
    );
  }

  Widget _buildNetworkRow(String label, String value) {
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: Spacing.xs),
      child: Row(
        children: [
          SizedBox(
            width: 120,
            child: Text(
              label,
              style: const TextStyle(color: PiccoloTheme.inkMuted),
            ),
          ),
          Expanded(
            child: SelectableText(
              value,
              style: const TextStyle(fontFamily: 'JetBrainsMono'),
            ),
          ),
        ],
      ),
    );
  }

  Widget _buildNetworkLinkRow(
    String label,
    String value, {
    required VoidCallback onTap,
    required IconData icon,
    String? tooltip,
  }) {
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: Spacing.xs),
      child: Row(
        children: [
          SizedBox(
            width: 120,
            child: Text(
              label,
              style: const TextStyle(color: PiccoloTheme.inkMuted),
            ),
          ),
          Expanded(
            child: InkWell(
              onTap: onTap,
              borderRadius: BorderRadius.circular(Radii.xxs),
              child: Padding(
                padding: const EdgeInsets.symmetric(vertical: 2),
                child: Row(
                  children: [
                    Flexible(
                      child: Text(
                        value,
                        style: const TextStyle(
                          fontFamily: 'JetBrainsMono',
                          color: PiccoloTheme.cobalt600,
                          decoration: TextDecoration.underline,
                        ),
                      ),
                    ),
                    const SizedBox(width: 6),
                    Tooltip(
                      message: tooltip ?? '',
                      child: Icon(icon, size: 14, color: PiccoloTheme.cobalt600),
                    ),
                  ],
                ),
              ),
            ),
          ),
        ],
      ),
    );
  }
}
