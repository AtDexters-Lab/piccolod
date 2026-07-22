import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/listener_health.dart';
import 'package:piccolo_os/core/models/network_models.dart';
import 'package:piccolo_os/core/models/resource_pressure.dart';
import 'package:piccolo_os/core/models/wifi_models.dart';
import 'package:piccolo_os/core/services/event_stream_client.dart';
import 'package:piccolo_os/core/services/websocket_connection.dart';
import 'package:piccolo_os/features/apps/app_launcher.dart';
import 'package:piccolo_os/shared/widgets/app_icon.dart';
import 'package:piccolo_os/shared/widgets/status_dot.dart';
import 'package:piccolo_os/shells/desktop/desktop_controller.dart';
import 'package:piccolo_os/shells/desktop/features/terminal/terminal_view.dart';
import 'package:piccolo_os/shells/desktop/models/desktop_window.dart';
import 'package:piccolo_os/shells/desktop/widgets/dock_health_presentation.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:pointer_interceptor/pointer_interceptor.dart';
import 'package:url_launcher/url_launcher.dart';

class Dock extends StatelessWidget {
  const Dock({required this.controller, super.key});
  final DesktopController controller;

  // IDs of pinned apps that shouldn't appear in running windows section
  static const Set<String> _pinnedAppIds = {
    'app-store',
    'settings',
    'terminal',
  };

  @override
  Widget build(BuildContext context) {
    final screenSize = MediaQuery.of(context).size;

    // Get running windows that aren't pinned apps, sorted by ID for stable order
    final runningWindows =
        controller.windows.where((w) => !_pinnedAppIds.contains(w.id)).toList()
          ..sort((a, b) => a.id.compareTo(b.id));

    return PointerInterceptor(
      intercepting: controller.hasVisibleWebWindow,
      child: Container(
        margin: const EdgeInsets.only(bottom: Spacing.md),
        padding: const EdgeInsets.symmetric(
          horizontal: Spacing.base,
          vertical: Spacing.md,
        ),
        decoration: BoxDecoration(
          color: PiccoloTheme.porcelain.withValues(alpha: 0.9),
          borderRadius: BorderRadius.circular(Radii.lg),
          boxShadow: Elevation.elev3,
          border: Border.all(
            color: Colors.white.withValues(alpha: 0.5),
            width: 1.5,
          ),
        ),
        child: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            // Home button
            DockItem(
              icon: PiccoloIcons.home,
              label: 'Home',
              onTap: controller.minimizeAllWindows,
            ),
            const SizedBox(width: Spacing.md),

            // Health indicator
            _HealthIndicator(controller: controller),
            const SizedBox(width: Spacing.md),

            // Network uplink indicator (WiFi signal, AP mode, reconnecting)
            _NetworkStatusIndicator(controller: controller),

            // Network peers indicator (hidden on remote access)
            if (AppLauncher.isLocalAccess(Uri.base.host.toLowerCase()))
              _NetworkPeersIndicator(controller: controller),
            const SizedBox(width: Spacing.base),

            _buildSeparator(),
            const SizedBox(width: Spacing.base),

            // Pinned apps
            DockItem(
              icon: PiccoloIcons.store,
              label: 'App Store',
              isActive: controller.isAppActive('app-store'),
              isOpen: controller.isAppOpen('app-store'),
              onTap: controller.openAppStore,
            ),
            const SizedBox(width: Spacing.md),
            DockItem(
              icon: PiccoloIcons.settings,
              label: 'Settings',
              isOpen: controller.isAppOpen('settings'),
              isActive: controller.isAppActive('settings'),
              onTap: controller.openSettings,
            ),
            const SizedBox(width: Spacing.md),
            DockItem(
              icon: PiccoloIcons.terminal,
              label: 'Terminal',
              isOpen: controller.isAppOpen('terminal'),
              isActive: controller.isAppActive('terminal'),
              onTap: () => controller.openApp(
                'terminal',
                'Terminal',
                PiccoloIcons.terminal,
                TerminalApp(
                  onSessionEnd: () => controller.closeWindow('terminal'),
                ),
                screenSize: screenSize,
                initialSize: const Size(850, 550),
              ),
            ),

            // Running windows section
            if (runningWindows.isNotEmpty) ...[
              const SizedBox(width: Spacing.base),
              _buildSeparator(),
              const SizedBox(width: Spacing.base),
              ...runningWindows.map(
                (window) => Padding(
                  padding: const EdgeInsets.only(right: Spacing.md),
                  child: _RunningWindowItem(
                    window: window,
                    isActive: controller.isAppActive(window.id),
                    onTap: () => controller.focusWindow(window.id),
                  ),
                ),
              ),
            ],

            const SizedBox(width: Spacing.base),
            _buildSeparator(),
            const SizedBox(width: Spacing.base),

            // Profile button
            _ProfileButton(onLogout: controller.logout),
          ],
        ),
      ),
    );
  }

  Widget _buildSeparator() {
    return Container(
      width: 1,
      height: 24,
      color: PiccoloTheme.ink.withValues(alpha: 0.1),
    );
  }
}

class _HealthIndicator extends StatefulWidget {
  const _HealthIndicator({required this.controller});
  final DesktopController controller;

  @override
  State<_HealthIndicator> createState() => _HealthIndicatorState();
}

class _HealthIndicatorState extends State<_HealthIndicator> {
  @override
  void initState() {
    super.initState();
    widget.controller.addListener(_onControllerChanged);
  }

  @override
  void didUpdateWidget(covariant _HealthIndicator oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (oldWidget.controller != widget.controller) {
      oldWidget.controller.removeListener(_onControllerChanged);
      widget.controller.addListener(_onControllerChanged);
    }
  }

  void _onControllerChanged() {
    if (mounted) setState(() {});
  }

  @override
  void dispose() {
    widget.controller.removeListener(_onControllerChanged);
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final client = widget.controller.eventStreamClient;
    return DockHealthIndicator(
      key: ValueKey(client),
      client: client,
    );
  }
}

/// Dock health UI backed by the unified event stream.
///
/// Kept independent from [DesktopController] so its connection and snapshot
/// lifecycle can be exercised as a mounted widget.
class DockHealthIndicator extends StatefulWidget {
  const DockHealthIndicator({required this.client, super.key});

  final EventStreamClient? client;

  @override
  State<DockHealthIndicator> createState() => _DockHealthIndicatorState();
}

class _DockHealthIndicatorState extends State<DockHealthIndicator> {
  // Map of app:listener -> health status
  final Map<String, ListenerHealth> _healthMap = {};
  StreamSubscription<ListenerHealthEvent>? _subscription;
  StreamSubscription<Map<String, dynamic>>? _remoteConfigSub;
  StreamSubscription<ResourcePressure>? _resourcePressureSub;
  StreamSubscription<String>? _snapshotCompleteSub;
  // Portal state from remote_config events (null = not configured).
  String? _portalState;
  // True between WebSocket connect and receiving health data (or grace timeout).
  // Prevents a brief "Healthy" flash on transient reconnects when backend is down.
  bool _pendingSnapshot = false;
  final Set<String> _completedSnapshots = {};
  static const Set<String> _requiredSnapshots = {
    'listener_health',
    'remote_config',
    'resource_pressure',
  };

  @override
  void initState() {
    super.initState();
    _subscribeToEvents();
  }

  @override
  void didUpdateWidget(covariant DockHealthIndicator oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (oldWidget.client != widget.client) {
      _unsubscribeFromClient(oldWidget.client);
      _subscribeToEvents();
    }
  }

  void _subscribeToEvents() {
    final client = widget.client;
    if (client != null) {
      client.addListener(_onClientStateChanged);
      _subscription = client.healthEvents.listen(_handleHealthEvent);
      _remoteConfigSub = client.remoteConfigEvents.listen(
        _handleRemoteConfigEvent,
      );
      _resourcePressureSub = client.resourcePressureEvents.listen(
        _handleResourcePressureEvent,
      );
      _snapshotCompleteSub = client.snapshotCompleteEvents.listen(
        _handleSnapshotComplete,
      );
      if (client.state == WebSocketConnectionState.connected) {
        _hydrateConnectedClient(client);
      }
    }
  }

  void _unsubscribeFromClient(EventStreamClient? client) {
    unawaited(_subscription?.cancel());
    _subscription = null;
    unawaited(_remoteConfigSub?.cancel());
    _remoteConfigSub = null;
    unawaited(_resourcePressureSub?.cancel());
    _resourcePressureSub = null;
    unawaited(_snapshotCompleteSub?.cancel());
    _snapshotCompleteSub = null;
    client?.removeListener(_onClientStateChanged);
  }

  void _onClientStateChanged() {
    if (!mounted) return;
    final client = widget.client;
    if (client?.state == WebSocketConnectionState.connected) {
      _healthMap.clear();
      _portalState = null;
      _completedSnapshots.clear();
      _pendingSnapshot = true;
      _hydrateConnectedClient(client!);
    } else {
      _pendingSnapshot = false;
      _completedSnapshots.clear();
    }
    setState(() {});
  }

  void _handleHealthEvent(ListenerHealthEvent event) {
    if (!mounted) return;
    setState(() {
      final key = '${event.app}:${event.listener}';
      _healthMap[key] = event.health;
    });
  }

  void _handleRemoteConfigEvent(Map<String, dynamic> event) {
    if (!mounted) return;
    final state = event['state'] as String?;
    if (state != _portalState) {
      setState(() {
        _portalState = state;
      });
    }
  }

  void _handleResourcePressureEvent(ResourcePressure _) {
    if (!mounted) return;
    setState(() {});
  }

  void _handleSnapshotComplete(String topic) {
    if (!mounted) return;
    setState(() {
      _completedSnapshots.add(topic);
      _pendingSnapshot = !_requiredSnapshots.every(
        _completedSnapshots.contains,
      );
    });
  }

  void _hydrateConnectedClient(EventStreamClient client) {
    for (final event in client.lastListenerHealthEvents) {
      _healthMap['${event.app}:${event.listener}'] = event.health;
    }
    final remote = client.lastRemoteConfig;
    _portalState = remote?['state'] as String?;
    _completedSnapshots.addAll(client.completedSnapshots);
    _pendingSnapshot = !_requiredSnapshots.every(_completedSnapshots.contains);
  }

  @override
  void dispose() {
    _unsubscribeFromClient(widget.client);
    super.dispose();
  }

  bool get _isConnected {
    final client = widget.client;
    if (client == null) return false;
    return client.state == WebSocketConnectionState.connected;
  }

  ResourcePressure? get _taskPressure => widget.client?.lastTaskPressure;

  ResourcePressure? get _globalRecoverySuppression =>
      widget.client?.lastGlobalRecoverySuppression;

  Iterable<ResourcePressure> get _runtimePressure =>
      widget.client?.lastRuntimePressure ?? const <ResourcePressure>[];

  /// Whether portal state is active enough to include in health aggregation.
  bool get _portalStateRelevant {
    final s = _portalState;
    return s != null && s != 'disabled' && s != 'stopped';
  }

  DockHealthLevel get _aggregateStatus {
    var hasError = false;
    var hasDegraded = false;
    var hasRecovering = false;

    if (_taskPressure?.isCritical ?? false) {
      return DockHealthLevel.recovering;
    }

    for (final health in _healthMap.values) {
      if (health.isError) hasError = true;
      if (health.isDegraded) hasDegraded = true;
      if (health.isRecovering) hasRecovering = true;
    }

    // Include portal/remote state when actively configured.
    if (_portalStateRelevant) {
      if (_portalState == 'error') hasError = true;
      if (_portalState == 'warning' || _portalState == 'preflight_required') {
        hasDegraded = true;
      }
    }

    if (_taskPressure?.isWarning ?? false) hasDegraded = true;
    if (_globalRecoverySuppression != null) hasDegraded = true;
    if (_runtimePressure.isNotEmpty) hasDegraded = true;

    if (hasError) return DockHealthLevel.error;
    if (hasDegraded) return DockHealthLevel.degraded;
    if (hasRecovering) return DockHealthLevel.recovering;
    return DockHealthLevel.healthy;
  }

  Color get _statusColor {
    if (!_isConnected || _pendingSnapshot) return PiccoloTheme.inkMuted;
    switch (_aggregateStatus) {
      case DockHealthLevel.error:
        return PiccoloTheme.critical;
      case DockHealthLevel.degraded:
      case DockHealthLevel.recovering:
        return PiccoloTheme.warning;
      case DockHealthLevel.healthy:
        return PiccoloTheme.success;
    }
  }

  DockHealthPresentation get _presentation {
    final hasAutomaticRecoveryBackoff =
        _globalRecoverySuppression != null ||
        _runtimePressure.any(
          (pressure) => pressure.isRecoverySuppressed,
        );
    final hasUnknownAppObservation = _runtimePressure.any(
      (pressure) => pressure.isRuntimeUnknown,
    );
    return resolveDockHealthPresentation(
      connected: _isConnected,
      snapshotsPending: _pendingSnapshot,
      aggregateLevel: _aggregateStatus,
      taskCritical: _taskPressure?.isCritical ?? false,
      taskWarning: _taskPressure?.isWarning ?? false,
      taskMonitorUnavailable: _taskPressure?.isMonitorUnavailable ?? false,
      automaticRecoveryBackoff: hasAutomaticRecoveryBackoff,
      unknownAppObservation: hasUnknownAppObservation,
    );
  }

  @override
  Widget build(BuildContext context) {
    final color = _statusColor;
    final presentation = _presentation;

    return Tooltip(
      message: presentation.message,
      child: Container(
        padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 6),
        decoration: BoxDecoration(
          color: color.withValues(alpha: 0.1),
          borderRadius: BorderRadius.circular(Radii.md),
          border: Border.all(color: color.withValues(alpha: 0.3)),
        ),
        child: StatusDot(
          color: color,
          label: presentation.label,
          labelStyle: PiccoloTheme.textTheme.labelSmall?.copyWith(
            color: PiccoloTheme.ink,
            fontWeight: FontWeight.w500,
          ),
        ),
      ),
    );
  }
}

class _NetworkStatusIndicator extends StatefulWidget {
  const _NetworkStatusIndicator({required this.controller});
  final DesktopController controller;

  @override
  State<_NetworkStatusIndicator> createState() =>
      _NetworkStatusIndicatorState();
}

class _NetworkStatusIndicatorState extends State<_NetworkStatusIndicator> {
  StreamSubscription<Map<String, dynamic>>? _subscription;
  NetworkStatus? _status;

  @override
  void initState() {
    super.initState();
    widget.controller.addListener(_onControllerChanged);
    _subscribe();
  }

  @override
  void didUpdateWidget(covariant _NetworkStatusIndicator oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (oldWidget.controller != widget.controller) {
      oldWidget.controller.removeListener(_onControllerChanged);
      widget.controller.addListener(_onControllerChanged);
      _unsubscribe();
      _subscribe();
    }
  }

  void _onControllerChanged() {
    final client = widget.controller.eventStreamClient;
    if (client != null && _subscription == null) {
      _subscribe();
    }
  }

  void _subscribe() {
    final client = widget.controller.eventStreamClient;
    if (client == null) return;
    _unsubscribe();
    _subscription = client.networkStatusEvents.listen(_handleEvent);
  }

  void _unsubscribe() {
    unawaited(_subscription?.cancel());
    _subscription = null;
  }

  void _handleEvent(Map<String, dynamic> event) {
    if (!mounted) return;
    setState(() {
      _status = NetworkStatus.fromJson(event);
    });
  }

  @override
  void dispose() {
    widget.controller.removeListener(_onControllerChanged);
    _unsubscribe();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    // Full Ethernet is the normal/default state. Limited Ethernet remains
    // visible because it requires operator attention.
    final status = _status;
    if (status == null ||
        (status.isEthernet && !status.isLimitedConnectivity)) {
      return const SizedBox.shrink();
    }

    final (color, icon, label, tooltip) = switch (status) {
      NetworkStatus(isAPMode: true) => (
        PiccoloTheme.cobalt600,
        Icons.wifi_tethering,
        'AP Mode',
        'Broadcasting setup access point',
      ),
      NetworkStatus(isLimitedConnectivity: true) => (
        PiccoloTheme.warning,
        Icons.signal_wifi_statusbar_connected_no_internet_4,
        'Limited',
        'Connected via ${status.activeUplink}, but internet access is limited',
      ),
      NetworkStatus(isPortal: true) => (
        PiccoloTheme.warning,
        Icons.login,
        'Sign-in required',
        'This network requires browser sign-in; choose another network',
      ),
      NetworkStatus(isUnknown: true) => (
        PiccoloTheme.inkMuted,
        Icons.sync,
        'Checking network',
        'Network status is temporarily unavailable',
      ),
      NetworkStatus(isWifiConnected: true) => (
        _signalColor,
        Icons.wifi,
        _signalLabel,
        'Connected via WiFi${status.signalTier != null ? " (${status.signalTier})" : ""}',
      ),
      NetworkStatus(isReconnecting: true) => (
        PiccoloTheme.warning,
        PiccoloIcons.wifiOff,
        'Reconnecting',
        'WiFi lost — attempting to reconnect',
      ),
      NetworkStatus(isDisconnected: true) => (
        PiccoloTheme.critical,
        PiccoloIcons.wifiOff,
        'Disconnected',
        'No network connection',
      ),
      _ => (
        PiccoloTheme.inkMuted,
        PiccoloIcons.wifiOff,
        status.connectivity,
        '',
      ),
    };

    return Padding(
      padding: const EdgeInsets.only(right: Spacing.md),
      child: Tooltip(
        message: tooltip,
        child: Container(
          padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 6),
          decoration: BoxDecoration(
            color: color.withValues(alpha: 0.1),
            borderRadius: BorderRadius.circular(Radii.md),
            border: Border.all(color: color.withValues(alpha: 0.3)),
          ),
          child: Row(
            mainAxisSize: MainAxisSize.min,
            children: [
              Icon(icon, size: 14, color: color),
              const SizedBox(width: 6),
              Text(
                label,
                style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                  color: PiccoloTheme.ink,
                  fontWeight: FontWeight.w500,
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }

  Color get _signalColor {
    return switch (_status?.signalTier) {
      'good' || 'fair' => PiccoloTheme.success,
      'weak' => PiccoloTheme.warning,
      'poor' => PiccoloTheme.critical,
      _ => PiccoloTheme.success,
    };
  }

  String get _signalLabel {
    return switch (_status?.signalTier) {
      'good' => 'WiFi',
      'fair' => 'WiFi',
      'weak' => 'WiFi (weak)',
      'poor' => 'WiFi (poor)',
      _ => 'WiFi',
    };
  }
}

class _RunningWindowItem extends StatelessWidget {
  const _RunningWindowItem({
    required this.window,
    required this.isActive,
    required this.onTap,
  });
  final DesktopWindow window;
  final bool isActive;
  final VoidCallback onTap;

  Widget _buildIcon(Color color) {
    if (window.iconUrl != null && window.iconUrl!.isNotEmpty) {
      return AppIcon(
        proxyUrl: window.iconUrl,
        originalIconUrl: window.originalIconUrl,
        size: 28,
        borderRadius: Radii.sm,
        fallbackIcon: window.icon,
        fallbackBackgroundColor: Colors.transparent,
      );
    }
    return Icon(window.icon, color: color, size: 28);
  }

  @override
  Widget build(BuildContext context) {
    final color = isActive ? PiccoloTheme.cobalt600 : PiccoloTheme.ink;

    return Tooltip(
      message: window.title,
      child: InkWell(
        onTap: onTap,
        borderRadius: BorderRadius.circular(Radii.md),
        child: Container(
          padding: const EdgeInsets.fromLTRB(10, 10, 10, 14),
          decoration: BoxDecoration(
            color: isActive
                ? PiccoloTheme.cobalt600.withValues(alpha: 0.1)
                : Colors.transparent,
            borderRadius: BorderRadius.circular(Radii.md),
          ),
          child: Stack(
            alignment: Alignment.center,
            clipBehavior: Clip.none,
            children: [
              _buildIcon(color),
              // Running indicator dot pinned to bottom
              Positioned(
                bottom: -8,
                child: StatusDot(
                  color: window.isMinimized
                      ? PiccoloTheme.inkMuted.withValues(alpha: 0.5)
                      : PiccoloTheme.ink,
                  size: 4,
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}

class _ProfileButton extends StatelessWidget {
  const _ProfileButton({required this.onLogout});
  final VoidCallback onLogout;

  @override
  Widget build(BuildContext context) {
    return PopupMenuButton<String>(
      offset: const Offset(0, -60),
      itemBuilder: (context) => [
        const PopupMenuItem(
          value: 'logout',
          child: Row(
            children: [
              Icon(PiccoloIcons.logout, size: 18, color: PiccoloTheme.ink),
              SizedBox(width: Spacing.md),
              Text('Log Out'),
            ],
          ),
        ),
      ],
      onSelected: (value) {
        if (value == 'logout') {
          onLogout();
        }
      },
      child: Tooltip(
        message: 'Profile',
        child: Container(
          padding: const EdgeInsets.all(6),
          decoration: BoxDecoration(
            color: PiccoloTheme.cobalt600.withValues(alpha: 0.1),
            borderRadius: BorderRadius.circular(Radii.md),
          ),
          child: const CircleAvatar(
            radius: 14,
            backgroundColor: PiccoloTheme.cobalt600,
            child: Icon(
              PiccoloIcons.person,
              size: 18,
              color: Colors.white,
            ),
          ),
        ),
      ),
    );
  }
}

class DockItem extends StatelessWidget {
  const DockItem({
    required this.icon,
    required this.label,
    super.key,
    this.isOpen = false,
    this.isActive = false,
    this.onTap,
  });
  final IconData icon;
  final String label;
  final bool isOpen;
  final bool isActive;
  final VoidCallback? onTap;

  @override
  Widget build(BuildContext context) {
    final color = isActive ? PiccoloTheme.cobalt600 : PiccoloTheme.ink;

    return Tooltip(
      message: label,
      child: InkWell(
        onTap: onTap ?? () {},
        borderRadius: BorderRadius.circular(Radii.md),
        child: Container(
          padding: const EdgeInsets.fromLTRB(10, 10, 10, 14),
          decoration: BoxDecoration(
            color: isActive
                ? PiccoloTheme.cobalt600.withValues(alpha: 0.1)
                : Colors.transparent,
            borderRadius: BorderRadius.circular(Radii.md),
          ),
          child: Stack(
            alignment: Alignment.center,
            clipBehavior: Clip.none,
            children: [
              Icon(icon, color: color, size: 28),
              // "Running" indicator dot pinned to bottom
              Positioned(
                bottom: -8,
                child: StatusDot(
                  color: isOpen ? PiccoloTheme.ink : Colors.transparent,
                  size: 4,
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}

class _NetworkPeersIndicator extends StatefulWidget {
  const _NetworkPeersIndicator({required this.controller});
  final DesktopController controller;

  @override
  State<_NetworkPeersIndicator> createState() => _NetworkPeersIndicatorState();
}

class _NetworkPeersIndicatorState extends State<_NetworkPeersIndicator> {
  List<DiscoveredPeer> _peers = [];
  StreamSubscription<NetworkPeersEvent>? _subscription;

  @override
  void initState() {
    super.initState();
    widget.controller.addListener(_onControllerChanged);
    _subscribeToEvents();
  }

  @override
  void didUpdateWidget(covariant _NetworkPeersIndicator oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (oldWidget.controller != widget.controller) {
      oldWidget.controller.removeListener(_onControllerChanged);
      widget.controller.addListener(_onControllerChanged);
      _unsubscribe();
      _subscribeToEvents();
    }
  }

  void _onControllerChanged() {
    final client = widget.controller.eventStreamClient;
    if (client != null && _subscription == null) {
      _subscribeToEvents();
    }
  }

  void _subscribeToEvents() {
    final client = widget.controller.eventStreamClient;
    if (client != null) {
      _unsubscribe();
      client.addListener(_onClientStateChanged);
      _subscription = client.networkPeersEvents.listen(_handlePeersEvent);
      // Hydrate from cached snapshot — the initial snapshot may have arrived
      // on the broadcast stream before this widget mounted.
      final cached = client.lastNetworkPeersEvent;
      if (cached != null && cached.peers.isNotEmpty) {
        _peers = cached.peers;
        if (mounted) setState(() {});
      }
    }
  }

  void _unsubscribe() {
    unawaited(_subscription?.cancel());
    _subscription = null;
    widget.controller.eventStreamClient?.removeListener(_onClientStateChanged);
  }

  void _onClientStateChanged() {
    if (!mounted) return;
    final client = widget.controller.eventStreamClient;
    if (client?.state == WebSocketConnectionState.connected) {
      // Clear stale data; fresh snapshot arrives from server
      setState(() => _peers = []);
    }
  }

  void _handlePeersEvent(NetworkPeersEvent event) {
    if (!mounted) return;
    setState(() {
      _peers = event.peers;
    });
  }

  @override
  void dispose() {
    widget.controller.removeListener(_onControllerChanged);
    _unsubscribe();
    super.dispose();
  }

  Future<void> _openPeerUrl(String url) async {
    final uri = Uri.parse(url);
    if (await canLaunchUrl(uri)) {
      await launchUrl(uri, mode: LaunchMode.externalApplication);
    }
  }

  @override
  Widget build(BuildContext context) {
    if (_peers.isEmpty) {
      return const SizedBox.shrink();
    }

    final onlinePeers = _peers.where((p) => p.online).length;

    return PopupMenuButton<DiscoveredPeer>(
      offset: const Offset(0, -80),
      tooltip: 'Other Piccolo devices on your network',
      onSelected: (peer) => _openPeerUrl(peer.url),
      itemBuilder: (context) => [
        const PopupMenuItem(
          enabled: false,
          child: Text(
            'Other Piccolo Devices',
            style: TextStyle(
              fontWeight: FontWeight.w600,
              color: PiccoloTheme.ink,
            ),
          ),
        ),
        const PopupMenuDivider(),
        ..._peers.map(
          (peer) => PopupMenuItem<DiscoveredPeer>(
            value: peer.online ? peer : null,
            enabled: peer.online,
            child: Row(
              children: [
                StatusDot(
                  color: peer.online
                      ? PiccoloTheme.success
                      : PiccoloTheme.inkMuted,
                ),
                const SizedBox(width: Spacing.md),
                Expanded(
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    mainAxisSize: MainAxisSize.min,
                    children: [
                      Text(
                        peer.displayName,
                        style: const TextStyle(fontWeight: FontWeight.w500),
                      ),
                      if (peer.model != null || peer.ipv4 != null)
                        Text(
                          peer.online
                              ? (peer.model ?? peer.ipv4 ?? '')
                              : '(offline)',
                          style: PiccoloTheme.textTheme.labelSmall,
                        ),
                    ],
                  ),
                ),
              ],
            ),
          ),
        ),
      ],
      child: Container(
        padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 6),
        decoration: BoxDecoration(
          color: PiccoloTheme.cobalt600.withValues(alpha: 0.1),
          borderRadius: BorderRadius.circular(Radii.md),
          border: Border.all(
            color: PiccoloTheme.cobalt600.withValues(alpha: 0.3),
          ),
        ),
        child: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            const Icon(
              PiccoloIcons.devices,
              size: 16,
              color: PiccoloTheme.cobalt600,
            ),
            const SizedBox(width: 6),
            Text(
              '$onlinePeers',
              style: PiccoloTheme.textTheme.labelSmall?.copyWith(
                color: PiccoloTheme.ink,
                fontWeight: FontWeight.w500,
              ),
            ),
          ],
        ),
      ),
    );
  }
}
