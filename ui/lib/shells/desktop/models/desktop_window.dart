import 'package:flutter/material.dart';

class DesktopWindow {
  final String id;
  final String title;
  final Widget child;
  final IconData icon;

  // Mutable state managed by the controller
  Offset position;
  Size size;

  bool isMinimized;
  bool isMaximized;
  bool isClosing; // For animation

  // Restore state
  Offset? preMaximizePosition;
  Size? preMaximizeSize;

  DesktopWindow({
    required this.id,
    required this.title,
    required this.child,
    required this.icon,
    this.position = const Offset(100, 100),
    this.size = const Size(600, 400),
    this.isMinimized = false,
    this.isMaximized = false,
    this.isClosing = false,
  });
}
