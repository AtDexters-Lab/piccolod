import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/listener_health.dart';
import 'package:piccolo_os/core/models/network_models.dart';
import 'package:piccolo_os/core/services/websocket_connection.dart';
import 'package:piccolo_os/features/apps/app_launcher.dart';
import 'package:piccolo_os/shared/widgets/app_icon.dart';
import 'package:piccolo_os/shared/widgets/status_dot.dart';
import 'package:piccolo_os/shells/desktop/desktop_controller.dart';
import 'package:piccolo_os/shells/desktop/features/terminal/terminal_view.dart';
import 'package:piccolo_os/shells/desktop/models/desktop_window.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:pointer_interceptor/pointer_interceptor.dart';
import 'package:url_launcher/url_launcher.dart';

class Dock extends StatelessWidget {

  const Dock({required this.controller, super.key});
  final DesktopController controller;

  // IDs of pinned apps that shouldn't appear in running windows section
  static const Set<String> _pinnedAppIds = {'app-store', 'settings', 'terminal'};

  @override
  Widget build(BuildContext context) {
    final screenSize = MediaQuery.of(context).size;

    // Get running windows that aren't pinned apps, sorted by ID for stable order
    final runningWindows = controller.windows
        .where((w) => !_pinnedAppIds.contains(w.id))
        .toList()
      ..sort((a, b) => a.id.compareTo(b.id));

    return PointerInterceptor(
      intercepting: controller.hasVisibleWebWindow,
      child: Container(
        margin: const EdgeInsets.only(bottom: Spacing.md),
        padding: const EdgeInsets.symmetric(horizontal: Spacing.base, vertical: Spacing.md),
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
              ...runningWindows.map((window) => Padding(
                    padding: const EdgeInsets.only(right: Spacing.md),
                    child: _RunningWindowItem(
                      window: window,
                      isActive: controller.isAppActive(window.id),
                      onTap: () => controller.focusWindow(window.id),
                    ),
                  )),
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
  // Map of app:listener -> health status
  final Map<String, ListenerHealth> _healthMap = {};
  StreamSubscription<ListenerHealthEvent>? _subscription;
  // True between WebSocket connect and receiving health data (or grace timeout).
  // Prevents a brief "Healthy" flash on transient reconnects when backend is down.
  bool _pendingSnapshot = false;
  Timer? _snapshotGrace;

  @override
  void initState() {
    super.initState();
    widget.controller.addListener(_onControllerChanged);
    _subscribeToEvents();
  }

  @override
  void didUpdateWidget(covariant _HealthIndicator oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (oldWidget.controller != widget.controller) {
      oldWidget.controller.removeListener(_onControllerChanged);
      widget.controller.addListener(_onControllerChanged);
      _unsubscribeFromClient();
      _subscribeToEvents();
    }
  }

  void _onControllerChanged() {
    // Re-subscribe when eventStreamClient becomes available or changes
    final client = widget.controller.eventStreamClient;
    if (client != null && _subscription == null) {
      _subscribeToEvents();
    }
    // Trigger rebuild to reflect connection state changes
    if (mounted) setState(() {});
  }

  void _subscribeToEvents() {
    final client = widget.controller.eventStreamClient;
    if (client != null) {
      _unsubscribeFromClient();
      client.addListener(_onClientStateChanged);
      _subscription = client.healthEvents.listen(_handleHealthEvent);
    }
  }

  void _unsubscribeFromClient() {
    unawaited(_subscription?.cancel());
    _subscription = null;
    widget.controller.eventStreamClient?.removeListener(_onClientStateChanged);
  }

  void _onClientStateChanged() {
    if (!mounted) return;
    final client = widget.controller.eventStreamClient;
    if (client?.state == WebSocketConnectionState.connected) {
      _healthMap.clear();
      _pendingSnapshot = true;
      _snapshotGrace?.cancel();
      _snapshotGrace = Timer(const Duration(seconds: 3), () {
        if (!mounted) return;
        if (_healthMap.isNotEmpty) {
          setState(() => _pendingSnapshot = false);
        }
      });
    } else {
      _snapshotGrace?.cancel();
      _pendingSnapshot = false;
    }
    setState(() {});
  }

  void _handleHealthEvent(ListenerHealthEvent event) {
    if (!mounted) return;
    setState(() {
      final key = '${event.app}:${event.listener}';
      _healthMap[key] = event.health;
      if (_pendingSnapshot) {
        _pendingSnapshot = false;
        _snapshotGrace?.cancel();
      }
    });
  }

  @override
  void dispose() {
    _snapshotGrace?.cancel();
    widget.controller.removeListener(_onControllerChanged);
    _unsubscribeFromClient();
    super.dispose();
  }

  bool get _isConnected {
    final client = widget.controller.eventStreamClient;
    if (client == null) return false;
    return client.state == WebSocketConnectionState.connected;
  }

  String get _aggregateStatus {
    if (_healthMap.isEmpty) return 'ok';

    var hasError = false;
    var hasDegraded = false;
    var hasRecovering = false;

    for (final health in _healthMap.values) {
      if (health.isError) hasError = true;
      if (health.isDegraded) hasDegraded = true;
      if (health.isRecovering) hasRecovering = true;
    }

    if (hasError) return 'error';
    if (hasDegraded) return 'degraded';
    if (hasRecovering) return 'recovering';
    return 'ok';
  }

  Color get _statusColor {
    if (!_isConnected || _pendingSnapshot) return PiccoloTheme.inkMuted;
    switch (_aggregateStatus) {
      case 'error':
        return PiccoloTheme.critical;
      case 'degraded':
      case 'recovering':
        return PiccoloTheme.warning;
      default:
        return PiccoloTheme.success;
    }
  }

  String get _statusLabel {
    if (!_isConnected || _pendingSnapshot) return 'Offline';
    switch (_aggregateStatus) {
      case 'error':
        return 'Error';
      case 'degraded':
        return 'Degraded';
      case 'recovering':
        return 'Recovering';
      default:
        return 'Healthy';
    }
  }

  String get _tooltipMessage {
    if (!_isConnected) return 'Connection lost - Reconnecting...';
    if (_pendingSnapshot) return 'Connected - Waiting for health data...';
    switch (_aggregateStatus) {
      case 'error':
        return 'System Error - Check app details';
      case 'degraded':
        return 'System Degraded - Action may be required';
      case 'recovering':
        return 'System Recovering - Auto-healing in progress';
      default:
        return 'System Healthy';
    }
  }

  @override
  Widget build(BuildContext context) {
    final color = _statusColor;

    return Tooltip(
      message: _tooltipMessage,
      child: Container(
        padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 6),
        decoration: BoxDecoration(
          color: color.withValues(alpha: 0.1),
          borderRadius: BorderRadius.circular(Radii.md),
          border: Border.all(color: color.withValues(alpha: 0.3)),
        ),
        child: StatusDot(
          color: color,
          label: _statusLabel,
          labelStyle: PiccoloTheme.textTheme.labelSmall?.copyWith(
            color: PiccoloTheme.ink,
            fontWeight: FontWeight.w500,
          ),
        ),
      ),
    );
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
    required this.icon, required this.label, super.key,
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
        ..._peers.map((peer) => PopupMenuItem<DiscoveredPeer>(
              value: peer.online ? peer : null,
              enabled: peer.online,
              child: Row(
                children: [
                  StatusDot(
                    color: peer.online ? PiccoloTheme.success : PiccoloTheme.inkMuted,
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
            )),
      ],
      child: Container(
        padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 6),
        decoration: BoxDecoration(
          color: PiccoloTheme.cobalt600.withValues(alpha: 0.1),
          borderRadius: BorderRadius.circular(Radii.md),
          border: Border.all(color: PiccoloTheme.cobalt600.withValues(alpha: 0.3)),
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
