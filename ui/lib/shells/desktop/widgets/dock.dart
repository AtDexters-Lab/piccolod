import 'dart:async';

import 'package:flutter/material.dart';
import 'package:pointer_interceptor/pointer_interceptor.dart';
import '../../../core/models/listener_health.dart';
import '../../../core/services/websocket_connection.dart';
import '../../../theme/piccolo_theme.dart';
import '../desktop_controller.dart';
import '../models/desktop_window.dart';

import '../features/settings/settings_app.dart';
import '../features/terminal/terminal_view.dart';

class Dock extends StatelessWidget {
  final DesktopController controller;

  const Dock({super.key, required this.controller});

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
        margin: const EdgeInsets.only(bottom: 12),
        padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 12),
        decoration: BoxDecoration(
          color: PiccoloTheme.porcelain.withValues(alpha: 0.9),
          borderRadius: BorderRadius.circular(24),
          boxShadow: [
            BoxShadow(
              color: Colors.black.withValues(alpha: 0.15),
              blurRadius: 20,
              offset: const Offset(0, 8),
            ),
          ],
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
              icon: Icons.home_rounded,
              label: "Home",
              onTap: controller.minimizeAllWindows,
            ),
            const SizedBox(width: 12),

            // Health indicator
            _HealthIndicator(controller: controller),
            const SizedBox(width: 16),

            _buildSeparator(),
            const SizedBox(width: 16),

            // Pinned apps
            DockItem(
              icon: Icons.storefront,
              label: "App Store",
              isActive: controller.isAppActive("app-store"),
              isOpen: controller.isAppOpen("app-store"),
              onTap: () => controller.openAppStore(),
            ),
            const SizedBox(width: 12),
            DockItem(
              icon: Icons.settings_rounded,
              label: "Settings",
              isOpen: controller.isAppOpen("settings"),
              isActive: controller.isAppActive("settings"),
              onTap: () => controller.openApp(
                "settings",
                "Settings",
                Icons.settings_rounded,
                SettingsApp(
                  onLogout: controller.logout,
                  eventStreamClient: controller.eventStreamClient,
                ),
                screenSize: screenSize,
                initialSize: const Size(1100, 750),
              ),
            ),
            const SizedBox(width: 12),
            DockItem(
              icon: Icons.terminal_rounded,
              label: "Terminal",
              isOpen: controller.isAppOpen("terminal"),
              isActive: controller.isAppActive("terminal"),
              onTap: () => controller.openApp(
                "terminal",
                "Terminal",
                Icons.terminal_rounded,
                TerminalApp(
                  onSessionEnd: () => controller.closeWindow("terminal"),
                ),
                screenSize: screenSize,
                initialSize: const Size(850, 550),
              ),
            ),

            // Running windows section
            if (runningWindows.isNotEmpty) ...[
              const SizedBox(width: 16),
              _buildSeparator(),
              const SizedBox(width: 16),
              ...runningWindows.map((window) => Padding(
                    padding: const EdgeInsets.only(right: 12),
                    child: _RunningWindowItem(
                      window: window,
                      isActive: controller.isAppActive(window.id),
                      onTap: () => controller.focusWindow(window.id),
                    ),
                  )),
            ],

            const SizedBox(width: 16),
            _buildSeparator(),
            const SizedBox(width: 16),

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
  final DesktopController controller;

  const _HealthIndicator({required this.controller});

  @override
  State<_HealthIndicator> createState() => _HealthIndicatorState();
}

class _HealthIndicatorState extends State<_HealthIndicator> {
  // Map of app:listener -> health status
  final Map<String, ListenerHealth> _healthMap = {};
  StreamSubscription<ListenerHealthEvent>? _subscription;

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
    _subscription?.cancel();
    _subscription = null;
    widget.controller.eventStreamClient?.removeListener(_onClientStateChanged);
  }

  void _onClientStateChanged() {
    if (!mounted) return;
    final client = widget.controller.eventStreamClient;
    // Clear stale health data on reconnect; server will send fresh snapshot
    if (client?.state == WebSocketConnectionState.connected) {
      _healthMap.clear();
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

  @override
  void dispose() {
    widget.controller.removeListener(_onControllerChanged);
    _unsubscribeFromClient();
    super.dispose();
  }

  bool get _isConnected {
    final client = widget.controller.eventStreamClient;
    if (client == null) return false;
    return client.state == WebSocketConnectionState.connected;
  }

  /// Aggregates health across all listeners to find worst status.
  /// Priority: error > degraded > recovering > ok
  String get _aggregateStatus {
    if (_healthMap.isEmpty) return 'ok';

    bool hasError = false;
    bool hasDegraded = false;
    bool hasRecovering = false;

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
    if (!_isConnected) return PiccoloTheme.inkMuted;
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
    if (!_isConnected) return 'Offline';
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
          borderRadius: BorderRadius.circular(12),
          border: Border.all(color: color.withValues(alpha: 0.3)),
        ),
        child: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            Container(
              width: 8,
              height: 8,
              decoration: BoxDecoration(
                color: color,
                shape: BoxShape.circle,
              ),
            ),
            const SizedBox(width: 6),
            Text(
              _statusLabel,
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

class _RunningWindowItem extends StatelessWidget {
  final DesktopWindow window;
  final bool isActive;
  final VoidCallback onTap;

  const _RunningWindowItem({
    required this.window,
    required this.isActive,
    required this.onTap,
  });

  Widget _buildIcon(Color color) {
    if (window.iconUrl != null && window.iconUrl!.isNotEmpty) {
      return ClipRRect(
        borderRadius: BorderRadius.circular(8),
        child: Image.network(
          window.iconUrl!,
          width: 28,
          height: 28,
          fit: BoxFit.cover,
          errorBuilder: (context, error, stackTrace) => Icon(window.icon, color: color, size: 28),
        ),
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
        borderRadius: BorderRadius.circular(12),
        child: Container(
          padding: const EdgeInsets.all(10),
          decoration: BoxDecoration(
            color: isActive
                ? PiccoloTheme.cobalt600.withValues(alpha: 0.1)
                : Colors.transparent,
            borderRadius: BorderRadius.circular(12),
          ),
          child: Column(
            mainAxisSize: MainAxisSize.min,
            children: [
              _buildIcon(color),
              const SizedBox(height: 4),
              // Running indicator dot
              Container(
                width: 4,
                height: 4,
                decoration: BoxDecoration(
                  color: window.isMinimized
                      ? PiccoloTheme.inkMuted.withValues(alpha: 0.5)
                      : PiccoloTheme.ink,
                  shape: BoxShape.circle,
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
  final VoidCallback onLogout;

  const _ProfileButton({required this.onLogout});

  @override
  Widget build(BuildContext context) {
    return PopupMenuButton<String>(
      offset: const Offset(0, -60),
      shape: RoundedRectangleBorder(
        borderRadius: BorderRadius.circular(12),
      ),
      itemBuilder: (context) => [
        const PopupMenuItem(
          value: 'logout',
          child: Row(
            children: [
              Icon(Icons.logout, size: 18, color: PiccoloTheme.ink),
              SizedBox(width: 12),
              Text("Log Out"),
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
        message: "Profile",
        child: Container(
          padding: const EdgeInsets.all(6),
          decoration: BoxDecoration(
            color: PiccoloTheme.cobalt600.withValues(alpha: 0.1),
            borderRadius: BorderRadius.circular(12),
          ),
          child: const CircleAvatar(
            radius: 14,
            backgroundColor: PiccoloTheme.cobalt600,
            child: Icon(
              Icons.person,
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
  final IconData icon;
  final String label;
  final bool isOpen;
  final bool isActive;
  final VoidCallback? onTap;

  const DockItem({
    super.key,
    required this.icon,
    required this.label,
    this.isOpen = false,
    this.isActive = false,
    this.onTap,
  });

  @override
  Widget build(BuildContext context) {
    final color = isActive ? PiccoloTheme.cobalt600 : PiccoloTheme.ink;

    return Tooltip(
      message: label,
      child: InkWell(
        onTap: onTap ?? () {},
        borderRadius: BorderRadius.circular(12),
        child: Container(
          padding: const EdgeInsets.all(10),
          decoration: BoxDecoration(
            color: isActive
                ? PiccoloTheme.cobalt600.withValues(alpha: 0.1)
                : Colors.transparent,
            borderRadius: BorderRadius.circular(12),
          ),
          child: Column(
            mainAxisSize: MainAxisSize.min,
            children: [
              Icon(icon, color: color, size: 28),
              const SizedBox(height: 4),
              // "Running" indicator dot
              Container(
                width: 4,
                height: 4,
                decoration: BoxDecoration(
                  color: isOpen ? PiccoloTheme.ink : Colors.transparent,
                  shape: BoxShape.circle,
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
