import 'package:flutter/material.dart';

class DesktopWindow {
  final String id;
  final String title;
  final IconData icon;
  final String? iconUrl; // Optional network image icon (takes precedence over icon)
  final Widget child;
  final List<Widget>? actions; // Custom title bar actions
  Offset position;
  Size size;
  bool isMinimized;
  bool isMaximized;
  bool isClosing; // Animation state
  bool requiresInterceptor; // Whether to wrap in PointerInterceptor (disable for WebViews)

  // Restore state
  Offset? preMaximizePosition;
  Size? preMaximizeSize;

  DesktopWindow({
    required this.id,
    required this.title,
    required this.icon,
    this.iconUrl,
    required this.child,
    required this.position,
    required this.size,
    this.actions,
    this.isMinimized = false,
    this.isMaximized = false,
    this.isClosing = false,
    this.requiresInterceptor = true,
  });
}
