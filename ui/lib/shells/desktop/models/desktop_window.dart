import 'package:flutter/material.dart';

class DesktopWindow {

  DesktopWindow({
    required this.id,
    required this.title,
    required this.icon,
    required this.child, required this.position, required this.size, this.iconUrl,
    this.originalIconUrl,
    this.actions,
    this.isMinimized = false,
    this.isMaximized = false,
    this.isClosing = false,
    this.requiresInterceptor = true,
  });
  final String id;
  final String title;
  final IconData icon;
  final String? iconUrl; // Optional network image icon (takes precedence over icon)
  final String? originalIconUrl; // Original catalog icon URL for SVG detection
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
}
