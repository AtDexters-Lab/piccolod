import 'package:flutter/material.dart';

/// Propagates the active state of a window down to its children.
/// This allows widgets like WebViews to self-manage their interactivity
/// (e.g., disabling pointer events when the window is in the background).
class WindowActivity extends InheritedWidget {

  const WindowActivity({
    required this.isActive, required super.child, super.key,
  });
  final bool isActive;

  static bool isWindowActive(BuildContext context) {
    final activity = context.dependOnInheritedWidgetOfExactType<WindowActivity>();
    return activity?.isActive ?? true; // Default to true if not in a window (e.g. strict widgets)
  }

  @override
  bool updateShouldNotify(WindowActivity oldWidget) {
    return isActive != oldWidget.isActive;
  }
}
