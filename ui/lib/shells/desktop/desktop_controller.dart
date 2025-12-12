import 'package:flutter/material.dart';
import 'models/desktop_window.dart';
import '../../core/services/api_client.dart';
import '../../features/apps/app_store_window.dart';

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

  // Setup State
  bool _needsSetup = false; // Default to false to avoid flash
  bool get needsSetup => _needsSetup;

  bool _isInitializing = true;
  bool get isInitializing => _isInitializing;

  DesktopController() {
    _checkSystemStatus();
  }

  Future<void> _checkSystemStatus() async {
    try {
      // Check if admin setup is complete
      final response = await ApiClient().get('/api/v1/auth/initialized');
      // response is { "initialized": boolean }
      final initialized = response['initialized'] == true;
      
      if (!initialized) {
        _needsSetup = true;
      } else {
        // If initialized, check if we have a valid session (are we logged in?)
         try {
           final session = await ApiClient().get('/api/v1/auth/session');
           if (session['authenticated'] != true) {
             // Not authenticated, but system IS initialized. 
             // We reuse the Setup Wizard in "Login Mode".
             // The SetupWizard (implemented in features/setup) handles logic to show Login if already setup.
             _needsSetup = true;
           } else {
             _needsSetup = false;
           }
         } catch (_) {
           // If session check fails, assume we need login
           _needsSetup = true;
         }
      }
    } catch (e) {
      debugPrint("System status check failed: $e");
      // If backend is down or unreachable, what do we do?
      // For now, assume we might need setup/login to be safe.
      _needsSetup = true;
    } finally {
      _isInitializing = false;
      notifyListeners();
    }
  }

  void completeSetup(bool isFirstSetupFlow) async {
    _needsSetup = false;
    notifyListeners();

    if (isFirstSetupFlow) {
      // Open a welcome window only after first device setup.
      openApp(
        "welcome",
        "Welcome",
        Icons.waving_hand,
        const Center(child: Text("Welcome to Piccolo OS!")),
      );
    }
  }
  
  void openAppStore() {
    if (isAppOpen("app-store")) {
      focusWindow("app-store");
    } else {
      openApp(
        "app-store",
        "App Store",
        Icons.storefront,
        AppStoreWindow(desktopController: this),
        initialSize: const Size(900, 650),
      );
    }
  }

  void logout() async {
    try {
      await ApiClient().post('/api/v1/auth/logout');
    } catch (e) {
      debugPrint("Logout failed: $e");
    }
    // Force UI back to SetupWizard (which will detect unauthenticated state and show Login)
    _needsSetup = true;
    _windows.clear(); // Clean up windows
    notifyListeners();
  }

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

  bool get hasVisibleWebWindow {
    return _windows.any((w) => !w.isMinimized && !w.requiresInterceptor);
  }

  void toggleLauncher() {
    _isLauncherOpen = !_isLauncherOpen;
    notifyListeners();
  }

  void openApp(
    String appId,
    String title,
    IconData icon,
    Widget content, {
    Size? screenSize,
    Size? initialSize,
    List<Widget>? actions,
    bool requiresInterceptor = true,
  }) {
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

    // 1. Determine Target Size
    // Use provided initialSize or a sensible default (1024x700)
    Size targetSize = initialSize ?? const Size(1024, 700);

    // 2. Clamp to Screen Size (Safety)
    if (screenSize != null) {
      // Ensure window isn't larger than the screen (minus some padding)
      final double maxWidth = screenSize.width;
      final double maxHeight =
          screenSize.height - 48; // Subtract top bar

      if (targetSize.width > maxWidth) {
        targetSize = Size(maxWidth, targetSize.height);
      }
      if (targetSize.height > maxHeight) {
        targetSize = Size(targetSize.width, maxHeight);
      }
    }

    // 3. Calculate Position (Smart Center)
    double x, y;

    if (screenSize != null) {
      const double topBarHeight = 48.0;
      const double dockAreaHeight = 110.0; // 90px dock + padding

      // Available height for centering
      final double availableHeight = screenSize.height - topBarHeight - dockAreaHeight;
      
      // Clamp height to available space to ensure dock visibility
      if (targetSize.height > availableHeight) {
        targetSize = Size(targetSize.width, availableHeight);
      }
      
      // Center horizontally
      x = (screenSize.width - targetSize.width) / 2;
      
      // Center vertically within the safe area, then add top offset
      y = topBarHeight + (availableHeight - targetSize.height) / 2;

      // Final safety clamps
      if (y < topBarHeight) y = topBarHeight;
    } else {
      // Fallback: Cascade based on window count
      final offset = _windows.length * 30.0;
      x = 100.0 + offset;
      y = 80.0 + offset;
    }

    final newWindow = DesktopWindow(
      id: appId,
      title: title,
      icon: icon,
      child: content,
      position: Offset(x, y),
      size: targetSize,
      actions: actions,
      requiresInterceptor: requiresInterceptor,
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
      if (window.preMaximizePosition != null &&
          window.preMaximizeSize != null) {
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
        availableSpace.height - topOffset - bottomReserve,
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
    const double minVisibleHeight =
        30.0; // Minimal title bar visibility at bottom

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

  void resizeWindow(String id, Size newSize, Size screenSize) {
    final window = _windows.firstWhere((w) => w.id == id);

    if (window.isMaximized) return;

    const double minWidth = 300.0;
    const double minHeight = 200.0;

    // Calculate maximum allowed dimensions based on current position
    // This ensures the bottom-right corner never leaves the screen
    final double maxWidth = screenSize.width - window.position.dx;
    final double maxHeight = screenSize.height - window.position.dy;

    double w = newSize.width;
    double h = newSize.height;

    // 1. Clamp to Minimums
    if (w < minWidth) w = minWidth;
    if (h < minHeight) h = minHeight;

    // 2. Clamp to Maximums (Screen Bounds)
    if (w > maxWidth) w = maxWidth;
    if (h > maxHeight) h = maxHeight;

    window.size = Size(w, h);
    notifyListeners();
  }
}
