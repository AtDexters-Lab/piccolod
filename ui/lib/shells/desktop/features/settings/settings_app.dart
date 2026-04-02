import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/event_stream_client.dart';
import 'package:piccolo_os/shells/desktop/features/settings/settings_controller.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/profile_tab.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/remote_tab.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/security/security_tab.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/system_tab.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/network/network_tab.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/users/users_tab.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Named indices for the Settings sidebar tabs.
class SettingsTab {
  SettingsTab._();
  static const int profile = 0;
  static const int remoteAccess = 1;
  static const int users = 2;
  static const int security = 3;
  static const int system = 4;
  static const int network = 5;
}

class SettingsApp extends StatefulWidget {

  const SettingsApp({super.key, this.onLogout, this.eventStreamClient, this.initialTab = 0});
  final VoidCallback? onLogout;
  final EventStreamClient? eventStreamClient;
  final int initialTab;

  @override
  State<SettingsApp> createState() => _SettingsAppState();
}

class _SettingsAppState extends State<SettingsApp> {
  final SettingsController _controller = SettingsController();

  @override
  void initState() {
    super.initState();
    _controller.onSessionExpired = widget.onLogout;
    _controller.selectTab(widget.initialTab);
  }

  @override
  void didUpdateWidget(SettingsApp oldWidget) {
    super.didUpdateWidget(oldWidget);
    _controller.onSessionExpired = widget.onLogout;
  }

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return ListenableBuilder(
      listenable: _controller,
      builder: (context, child) {
        return Scaffold(
          backgroundColor: PiccoloTheme.mist,
          body: Row(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              // Sidebar
              Container(
                width: 250,
                color: PiccoloTheme.porcelain,
                child: SingleChildScrollView(
                  child: Column(
                    children: [
                      const SizedBox(height: 24),
                      Padding(
                        padding: const EdgeInsets.symmetric(horizontal: 24),
                        child: Text(
                          'Settings',
                          style: PiccoloTheme.textTheme.headlineMedium,
                        ),
                      ),
                      const SizedBox(height: 32),
                      _SidebarItem(
                        icon: PiccoloIcons.person,
                        label: 'Profile',
                        isSelected: _controller.selectedIndex == 0,
                        onTap: () => _controller.selectTab(0),
                      ),
                      _SidebarItem(
                        icon: PiccoloIcons.cloud,
                        label: 'Remote Access',
                        isSelected: _controller.selectedIndex == 1,
                        onTap: () => _controller.selectTab(1),
                      ),
                      _SidebarItem(
                        icon: PiccoloIcons.people,
                        label: 'Users',
                        isSelected: _controller.selectedIndex == 2,
                        onTap: () => _controller.selectTab(2),
                      ),
                      _SidebarItem(
                        icon: PiccoloIcons.shield,
                        label: 'Security',
                        isSelected: _controller.selectedIndex == 3,
                        onTap: () => _controller.selectTab(3),
                      ),
                      _SidebarItem(
                        icon: PiccoloIcons.hardDrives,
                        label: 'System',
                        isSelected: _controller.selectedIndex == 4,
                        onTap: () => _controller.selectTab(4),
                      ),
                      _SidebarItem(
                        icon: PiccoloIcons.router,
                        label: 'Network',
                        isSelected: _controller.selectedIndex == 5,
                        onTap: () => _controller.selectTab(5),
                      ),
                    ],
                  ),
                ),
              ),
              // Content Area
              Expanded(
                child: LayoutBuilder(
                  builder: (context, constraints) {
                    return SingleChildScrollView(
                      padding: const EdgeInsets.all(32),
                      child: ConstrainedBox(
                        constraints: BoxConstraints(
                          minHeight: constraints.maxHeight > 64
                              ? constraints.maxHeight - 64
                              : 0,
                        ),
                        child: _buildContent(),
                      ),
                    );
                  },
                ),
              ),
            ],
          ),
        );
      },
    );
  }

  Widget _buildContent() {
    if (_controller.isLoading) {
      return const Center(child: CircularProgressIndicator());
    }

    if (_controller.error != null) {
      return Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            const Icon(PiccoloIcons.error, color: PiccoloTheme.critical, size: 48),
            const SizedBox(height: 16),
            Text('Error loading settings', style: PiccoloTheme.textTheme.bodyLarge),
            const SizedBox(height: 8),
            Text(_controller.error!, style: PiccoloTheme.textTheme.labelSmall),
            const SizedBox(height: 16),
            FilledButton(
              onPressed: () => _controller.selectTab(_controller.selectedIndex),
              child: const Text('Retry'),
            ),
          ],
        ),
      );
    }

    switch (_controller.selectedIndex) {
      case 0:
        return ProfileTab(
          controller: _controller,
          onLogout: widget.onLogout,
        );
      case 1:
        return RemoteTab(eventStreamClient: widget.eventStreamClient);
      case 2:
        return const UsersTab();
      case 3:
        return SecurityTab(controller: _controller);
      case 4:
        return SystemTab(controller: _controller);
      case 5:
        return NetworkTab(eventStreamClient: widget.eventStreamClient);
      default:
        return const SizedBox.shrink();
    }
  }
}

class _SidebarItem extends StatelessWidget {

  const _SidebarItem({
    required this.icon,
    required this.label,
    required this.isSelected,
    required this.onTap,
  });
  final IconData icon;
  final String label;
  final bool isSelected;
  final VoidCallback onTap;

  @override
  Widget build(BuildContext context) {
    return ListTile(
      leading: Icon(
        icon,
        color: isSelected ? PiccoloTheme.cobalt600 : PiccoloTheme.inkMuted,
      ),
      title: Text(
        label,
        style: TextStyle(
          color: isSelected ? PiccoloTheme.cobalt600 : PiccoloTheme.ink,
          fontWeight: isSelected ? FontWeight.w600 : FontWeight.w400,
        ),
      ),
      selected: isSelected,
      selectedTileColor: PiccoloTheme.cobalt600.withValues(alpha: 0.1),
      onTap: onTap,
      contentPadding: const EdgeInsets.symmetric(horizontal: 24, vertical: 4),
      shape: const RoundedRectangleBorder(
        borderRadius: BorderRadius.horizontal(right: Radius.circular(24)),
      ),
    );
  }
}
