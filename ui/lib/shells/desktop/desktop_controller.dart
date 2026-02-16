import 'package:flutter/material.dart';
import 'models/desktop_window.dart';
import '../../core/services/api_client.dart';
import '../../core/services/app_service.dart';
import '../../core/services/event_stream_client.dart';
import '../../theme/piccolo_icons.dart';
import '../../features/apps/app_store_window.dart';

/// Manages the state of the Desktop Shell.
///
/// Handles:
/// - Launcher (Dock) visibility/mode.
/// - Active windows (z-index orchestration).
/// - Global overlays (Search, Notifications).
class DesktopController extends ChangeNotifier {
  // Window positioning constants
  static const double kTopMargin = 8.0;
  static const double kDockAreaHeight = 120.0; // ~90px dock + padding
  static const double kBottomReserve = 100.0; // Space for dock when maximized
  static const double kMinVisibleWidth = 50.0;
  static const double kMinVisibleHeight = 30.0; // Title bar visibility

  bool _isLauncherOpen = false;
  bool get isLauncherOpen => _isLauncherOpen;

  final List<DesktopWindow> _windows = [];
  List<DesktopWindow> get windows => List.unmodifiable(_windows);

  // App Service for accessing installed apps
  late final AppService appService;

  // Unified event stream client
  EventStreamClient? _eventStreamClient;
  EventStreamClient? get eventStreamClient => _eventStreamClient;

  // Callbacks for app lifecycle changes (install, uninstall, start, stop)
  final List<VoidCallback> _appChangeListeners = [];

  void addAppChangeListener(VoidCallback listener) {
    _appChangeListeners.add(listener);
  }

  void removeAppChangeListener(VoidCallback listener) {
    _appChangeListeners.remove(listener);
  }

  void notifyAppsChanged() {
    for (final listener in List.of(_appChangeListeners)) {
      listener();
    }
  }

  // Setup State
  bool _needsSetup = false; // Default to false to avoid flash
  bool get needsSetup => _needsSetup;

  bool _isInitializing = true;
  bool get isInitializing => _isInitializing;

  DesktopController() {
    appService = AppService(ApiClient());
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
      // If already authenticated (returning user with valid session), transition to authenticated state
      if (!_needsSetup) {
        _onAuthenticated(isFirstSetup: false);
      } else {
        notifyListeners();
      }
    }
  }

  /// Called from SetupWizard when login/setup completes.
  void completeSetup(bool isFirstSetupFlow) {
    _onAuthenticated(isFirstSetup: isFirstSetupFlow);
  }

  /// Single source of truth for "user is now authenticated" state transition.
  /// Handles event stream connection and optional first-setup welcome.
  void _onAuthenticated({required bool isFirstSetup}) {
    _needsSetup = false;

    // Connect event stream
    _eventStreamClient ??= EventStreamClient();
    _eventStreamClient!.connect();

    notifyListeners();

    if (isFirstSetup) {
      // Open a welcome window only after first device setup.
      openApp(
        "welcome",
        "Welcome",
        PiccoloIcons.handWaving,
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
        PiccoloIcons.store,
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

    // Disconnect event stream
    _eventStreamClient?.disconnect();
    _eventStreamClient?.dispose();
    _eventStreamClient = null;

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
    String? iconUrl,
    String? originalIconUrl,
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
      // Ensure window isn't larger than the screen (minus dock area)
      final double maxWidth = screenSize.width;
      final double maxHeight = screenSize.height - kDockAreaHeight;

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
      // Available height for centering
      final double availableHeight = screenSize.height - kTopMargin - kDockAreaHeight;

      // Clamp height to available space to ensure dock visibility
      if (targetSize.height > availableHeight) {
        targetSize = Size(targetSize.width, availableHeight);
      }

      // Center horizontally
      x = (screenSize.width - targetSize.width) / 2;

      // Center vertically within the safe area, then add top offset
      y = kTopMargin + (availableHeight - targetSize.height) / 2;

      // Final safety clamps
      if (y < kTopMargin) y = kTopMargin;
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
      iconUrl: iconUrl,
      originalIconUrl: originalIconUrl,
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

  void minimizeAllWindows() {
    bool changed = false;
    for (final window in List.of(_windows)) {
      if (!window.isMinimized) {
        window.isMinimized = true;
        changed = true;
      }
    }
    if (changed) notifyListeners();
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

      // Apply max dimensions with space for dock at bottom
      window.position = Offset.zero;
      window.size = Size(
        availableSpace.width,
        availableSpace.height - kBottomReserve,
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

    double x = newPosition.dx;
    double y = newPosition.dy;

    // 1. Top Constraint: Cannot go above the top margin
    if (y < kTopMargin) {
      y = kTopMargin;
    }

    // 2. Bottom Constraint: Title bar must stay visible
    if (y > screenSize.height - kMinVisibleHeight) {
      y = screenSize.height - kMinVisibleHeight;
    }

    // 3. Horizontal Constraints: Keep at least some window visible on screen
    if (x + window.size.width < kMinVisibleWidth) {
      x = kMinVisibleWidth - window.size.width;
    }
    if (x > screenSize.width - kMinVisibleWidth) {
      x = screenSize.width - kMinVisibleWidth;
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

  @override
  void dispose() {
    _eventStreamClient?.dispose();
    _eventStreamClient = null;
    super.dispose();
  }
}
