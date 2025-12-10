import 'package:flutter/material.dart';
import 'package:pointer_interceptor/pointer_interceptor.dart';
import '../../../theme/piccolo_theme.dart';
import '../desktop_controller.dart';

import '../features/settings/settings_app.dart';
import '../features/files/files_app.dart';
import '../features/terminal/terminal_view.dart';

class Dock extends StatelessWidget {
  final DesktopController controller;

  const Dock({super.key, required this.controller});

  @override
  Widget build(BuildContext context) {
    final screenSize = MediaQuery.of(context).size;

    return PointerInterceptor(
      intercepting: controller.hasVisibleWebWindow,
      child: Container(
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
            DockItem(
              icon: Icons.apps,
              label: "Apps",
              isActive: controller.isAppActive("app-store"),
              isOpen: controller.isAppOpen("app-store"),
              onTap: () => controller.openAppStore(),
            ),
            const SizedBox(width: 16),
            Container(
              width: 1,
              height: 24,
              color: PiccoloTheme.ink.withValues(alpha: 0.1),
            ), // Separator
            const SizedBox(width: 16),
            DockItem(
              icon: Icons.folder_open_rounded,
              label: "Files",
              isOpen: controller.isAppOpen("files"),
              isActive: controller.isAppActive("files"),
              onTap: () => controller.openApp(
                "files",
                "Files",
                Icons.folder_open_rounded,
                const FilesApp(),
                screenSize: screenSize,
                initialSize: const Size(1000, 650),
              ),
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
                SettingsApp(onLogout: controller.logout),
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
                const TerminalApp(),
                screenSize: screenSize,
                initialSize: const Size(850, 550),
              ),
            ),
          ],
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
            // Active state gets a subtle highlight
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
