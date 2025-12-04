import 'package:flutter/material.dart';
import '../../../../theme/piccolo_theme.dart';
import 'settings_controller.dart';
import 'tabs/profile_tab.dart';
import 'tabs/network_tab.dart';
import 'tabs/system_tab.dart';

class SettingsApp extends StatefulWidget {
  final VoidCallback? onLogout;

  const SettingsApp({super.key, this.onLogout});

  @override
  State<SettingsApp> createState() => _SettingsAppState();
}

class _SettingsAppState extends State<SettingsApp> {
  final SettingsController _controller = SettingsController();

  @override
  void initState() {
    super.initState();
    _controller.onSessionExpired = widget.onLogout;
    _controller.selectTab(0); // Load initial data
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
                color: Colors.white, // Or porcelain
                child: SingleChildScrollView(
                  child: Column(
                    children: [
                      const SizedBox(height: 24),
                      Padding(
                        padding: const EdgeInsets.symmetric(horizontal: 24),
                        child: Text(
                          "Settings",
                          style: PiccoloTheme.textTheme.displayLarge?.copyWith(
                            fontSize: 24,
                          ),
                        ),
                      ),
                      const SizedBox(height: 32),
                      _SidebarItem(
                        icon: Icons.person_outline,
                        label: "Profile",
                        isSelected: _controller.selectedIndex == 0,
                        onTap: () => _controller.selectTab(0),
                      ),
                      _SidebarItem(
                        icon: Icons.wifi,
                        label: "Network",
                        isSelected: _controller.selectedIndex == 1,
                        onTap: () => _controller.selectTab(1),
                      ),
                      _SidebarItem(
                        icon: Icons.dns_outlined, // or system icon
                        label: "System",
                        isSelected: _controller.selectedIndex == 2,
                        onTap: () => _controller.selectTab(2),
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
                      padding: const EdgeInsets.all(32.0),
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
            const Icon(Icons.error_outline, color: PiccoloTheme.critical, size: 48),
            const SizedBox(height: 16),
            Text("Error loading settings", style: PiccoloTheme.textTheme.bodyLarge),
            const SizedBox(height: 8),
            Text(_controller.error!, style: PiccoloTheme.textTheme.labelSmall),
            const SizedBox(height: 16),
            ElevatedButton(
              onPressed: () => _controller.selectTab(_controller.selectedIndex),
              child: const Text("Retry"),
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
        return NetworkTab(controller: _controller);
      case 2:
        return SystemTab(controller: _controller);
      default:
        return const SizedBox.shrink();
    }
  }
}

class _SidebarItem extends StatelessWidget {
  final IconData icon;
  final String label;
  final bool isSelected;
  final VoidCallback onTap;

  const _SidebarItem({
    required this.icon,
    required this.label,
    required this.isSelected,
    required this.onTap,
  });

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
