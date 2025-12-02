import 'package:flutter/material.dart';
import 'models/desktop_window.dart';

/// Manages the state of the Desktop Shell.
/// 
/// Handles:
/// - Launcher (Dock) visibility/mode.
/// - Active windows (z-index orchestration).
/// - Global overlays (Search, Notifications).
class DesktopController extends ChangeNotifier {
  bool _isLauncherOpen = false;
  bool get isLauncherOpen => _isLauncherOpen;

  final List<DesktopWindow> _windows = [];
  List<DesktopWindow> get windows => List.unmodifiable(_windows);

  // Helper for Dock to know state
  bool isAppOpen(String id) => _windows.any((w) => w.id == id);
  
  bool isAppActive(String id) {
    if (_windows.isEmpty) return false;
    final topWindow = _windows.last;
    return topWindow.id == id && !topWindow.isMinimized;
  }
  
  bool isAppMinimized(String id) {
    final index = _windows.indexWhere((w) => w.id == id);
    if (index == -1) return false;
    return _windows[index].isMinimized;
  }

  void toggleLauncher() {
    _isLauncherOpen = !_isLauncherOpen;
    notifyListeners();
  }

  void openApp(String appId, String title, IconData icon, Widget content) {
    final existingIndex = _windows.indexWhere((w) => w.id == appId);
    
    if (existingIndex != -1) {
      final window = _windows[existingIndex];
      // If it's the top-most active window, minimize it (Windows-style toggle)
      if (isAppActive(appId)) {
        minimizeWindow(appId);
      } else {
        // Otherwise, restore/bring to front
        if (window.isMinimized) {
          window.isMinimized = false;
        }
        focusWindow(appId);
      }
      return;
    }

    // Basic cascade positioning logic
    final offset = _windows.length * 30.0;
    
    final newWindow = DesktopWindow(
      id: appId,
      title: title,
      icon: icon,
      child: content,
      position: Offset(100 + offset, 80 + offset),
    );

    _windows.add(newWindow);
    notifyListeners();
  }

  void closeWindow(String id) {
    final index = _windows.indexWhere((w) => w.id == id);
    if (index != -1) {
      // Mark as closing to trigger UI animation
      _windows[index].isClosing = true;
      notifyListeners();
      
      // Actual removal is now triggered by the UI callback 'onAnimationComplete'
      // Or we can set a timer here as a fallback, but UI callback is cleaner.
    }
  }
  
  void removeWindowInternal(String id) {
    _windows.removeWhere((w) => w.id == id);
    notifyListeners();
  }

  void focusWindow(String id) {
    final index = _windows.indexWhere((w) => w.id == id);
    if (index != -1) {
      final window = _windows.removeAt(index);
      // Ensure it's visible
      window.isMinimized = false;
      _windows.add(window); // Move to end (top of stack)
      notifyListeners();
    }
  }
  
  void minimizeWindow(String id) {
    final index = _windows.indexWhere((w) => w.id == id);
    if (index != -1) {
      _windows[index].isMinimized = true;
      notifyListeners();
    }
  }

  void maximizeWindow(String id, Size availableSpace) {
    final window = _windows.firstWhere((w) => w.id == id);
    
    if (window.isMaximized) {
      // Restore
      if (window.preMaximizePosition != null && window.preMaximizeSize != null) {
        window.position = window.preMaximizePosition!;
        window.size = window.preMaximizeSize!;
      }
      window.isMaximized = false;
    } else {
      // Maximize
      // Save state
      window.preMaximizePosition = window.position;
      window.preMaximizeSize = window.size;
      
      // Apply max dimensions
      // Respecting TopBar (48) and roughly the Dock area (bottom 90)
      const double topOffset = 48.0;
      const double bottomReserve = 90.0;
      
      window.position = const Offset(0, topOffset);
      window.size = Size(
        availableSpace.width, 
        availableSpace.height - topOffset - bottomReserve
      );
      
      window.isMaximized = true;
      
      // Also bring to front
      focusWindow(id);
    }
    notifyListeners();
  }

  void moveWindow(String id, Offset newPosition, Size screenSize) {
    final window = _windows.firstWhere((w) => w.id == id);
    
    // If maximized, dragging typically restores it. 
    // For simplicity in this iteration, disable dragging if maximized 
    // OR we could implement "snap out of maximize" logic. 
    // Let's lock it for now.
    if (window.isMaximized) return;
    
    // Constants for constraints
    const double topBarHeight = 48.0;
    const double minVisibleWidth = 50.0;
    const double minVisibleHeight = 30.0; // Minimal title bar visibility at bottom

    double x = newPosition.dx;
    double y = newPosition.dy;

    // 1. Top Constraint: Cannot go above the Top Bar
    if (y < topBarHeight) {
      y = topBarHeight;
    }

    // 2. Bottom Constraint: Title bar must stay visible
    // (y cannot be greater than screen height - min visible amount)
    if (y > screenSize.height - minVisibleHeight) {
      y = screenSize.height - minVisibleHeight;
    }

    // 3. Horizontal Constraints: Keep at least 'minVisibleWidth' on screen
    // Left Edge: Window right edge > minVisibleWidth
    // (x + width > minVisibleWidth) => x > minVisibleWidth - width
    if (x + window.size.width < minVisibleWidth) {
      x = minVisibleWidth - window.size.width;
    }

    // Right Edge: Window left edge < screenWidth - minVisibleWidth
    if (x > screenSize.width - minVisibleWidth) {
      x = screenSize.width - minVisibleWidth;
    }

    window.position = Offset(x, y);
    notifyListeners();
  }

  void resizeWindow(String id, Size newSize) {
    final window = _windows.firstWhere((w) => w.id == id);
    
    if (window.isMaximized) return;

    const double minWidth = 300.0;
    const double minHeight = 200.0;

    double w = newSize.width;
    double h = newSize.height;

    if (w < minWidth) w = minWidth;
    if (h < minHeight) h = minHeight;

    window.size = Size(w, h);
    notifyListeners();
  }
}
