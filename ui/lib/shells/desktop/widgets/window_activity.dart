import 'package:flutter/material.dart';

/// Propagates the active state of a window down to its children.
/// This allows widgets like WebViews to self-manage their interactivity
/// (e.g., disabling pointer events when the window is in the background).
class WindowActivity extends InheritedWidget {
  final bool isActive;

  const WindowActivity({
    super.key,
    required this.isActive,
    required super.child,
  });

  static bool isWindowActive(BuildContext context) {
    final activity = context.dependOnInheritedWidgetOfExactType<WindowActivity>();
    return activity?.isActive ?? true; // Default to true if not in a window (e.g. strict widgets)
  }

  @override
  bool updateShouldNotify(WindowActivity oldWidget) {
    return isActive != oldWidget.isActive;
  }
}
