import 'package:flutter/material.dart';
import '../../../theme/piccolo_theme.dart';
import '../desktop_controller.dart';

class Dock extends StatelessWidget {
  final DesktopController controller;

  const Dock({super.key, required this.controller});

  @override
  Widget build(BuildContext context) {
    return Container(
      margin: const EdgeInsets.only(bottom: 24), // Floating
      padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 12),
      decoration: BoxDecoration(
        color: PiccoloTheme.porcelain.withValues(alpha: 0.8),
        borderRadius: BorderRadius.circular(24),
        boxShadow: [
          BoxShadow(
            color: Colors.black.withValues(alpha: 0.1),
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
          _DockItem(
            icon: Icons.grid_view_rounded,
            label: "Apps",
            isLauncher: true,
            onTap: controller.toggleLauncher,
          ),
          const SizedBox(width: 16),
          Container(
            width: 1,
            height: 24,
            color: PiccoloTheme.ink.withValues(alpha: 0.1),
          ), // Separator
          const SizedBox(width: 16),
          _DockItem(
            icon: Icons.folder_open_rounded,
            label: "Files",
            isOpen: controller.isAppOpen("files"),
            isActive: controller.isAppActive("files"),
            onTap: () => controller.openApp(
              "files",
              "Files",
              Icons.folder_open_rounded,
              const Center(child: Text("Files App")),
            ),
          ),
          const SizedBox(width: 12),
          _DockItem(
            icon: Icons.settings_rounded,
            label: "Settings",
            isOpen: controller.isAppOpen("settings"),
            isActive: controller.isAppActive("settings"),
            onTap: () => controller.openApp(
              "settings",
              "Settings",
              Icons.settings_rounded,
              const Center(child: Text("Settings App")),
            ),
          ),
          const SizedBox(width: 12),
          _DockItem(
            icon: Icons.terminal_rounded,
            label: "Terminal",
            isOpen: controller.isAppOpen("terminal"),
            isActive: controller.isAppActive("terminal"),
            onTap: () => controller.openApp(
              "terminal",
              "Terminal",
              Icons.terminal_rounded,
              const Center(child: Text("Terminal App")),
            ),
          ),
        ],
      ),
    );
  }
}

class _DockItem extends StatelessWidget {
  final IconData icon;
  final String label;
  final bool isLauncher;
  final bool isOpen;
  final bool isActive;
  final VoidCallback? onTap;

  const _DockItem({
    required this.icon,
    required this.label,
    this.isLauncher = false,
    this.isOpen = false,
    this.isActive = false,
    this.onTap,
  });

  @override
  Widget build(BuildContext context) {
    final color = isLauncher ? PiccoloTheme.cobalt600 : PiccoloTheme.ink;

    return Tooltip(
      message: label,
      child: InkWell(
        onTap: onTap ?? () {},
        borderRadius: BorderRadius.circular(12),
        child: Container(
          padding: const EdgeInsets.all(10),
          decoration: BoxDecoration(
            // Active state gets a subtle highlight
            color: (isLauncher || isActive)
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
