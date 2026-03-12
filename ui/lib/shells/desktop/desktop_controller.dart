import 'dart:async';
import 'dart:js_interop';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/app_service.dart';
import 'package:piccolo_os/core/services/event_stream_client.dart';
import 'package:piccolo_os/core/utils/https_probe.dart';
import 'package:piccolo_os/features/apps/app_store_window.dart';
import 'package:piccolo_os/shells/desktop/features/settings/settings_app.dart';
import 'package:piccolo_os/shells/desktop/features/welcome/welcome_screen.dart';
import 'package:piccolo_os/shells/desktop/models/desktop_window.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:web/web.dart' as web;

/// Manages the state of the Desktop Shell.
///
/// Handles:
/// - Launcher (Dock) visibility/mode.
/// - Active windows (z-index orchestration).
/// - Global overlays (Search, Notifications).
class DesktopController extends ChangeNotifier {

  DesktopController() {
    appService = AppService(ApiClient());
    // Don't register onAuthRequired yet — _checkSystemStatus will determine
    // whether we're in desktop mode (reauth applies) or setup mode (setup
    // wizard owns auth). Registering eagerly causes stale reauth state when
    // 401s arrive while the setup wizard is active.
    unawaited(_checkSystemStatus());
  }
  // Query param used to preserve the welcome screen across HTTPS redirects.
  // Intentionally client-only and spoofable — worst case is a harmless re-show.
  static const _kWelcomeParam = 'welcome';

  // Window positioning constants
  static const double kTopMargin = 8;
  static const double kDockAreaHeight = 120; // ~90px dock + padding
  static const double kBottomReserve = 100; // Space for dock when maximized
  static const double kMinVisibleWidth = 50;
  static const double kMinVisibleHeight = 30; // Title bar visibility

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

  // Re-auth overlay state
  bool _showReauth = false;
  bool get showReauth => _showReauth;

  String? _lastKnownUsername;
  String? get lastKnownUsername => _lastKnownUsername;

  // Setup State
  bool _needsSetup = false; // Default to false to avoid flash
  bool get needsSetup => _needsSetup;

  bool _isInitializing = true;
  bool get isInitializing => _isInitializing;

  Future<void> _checkSystemStatus() async {
    try {
      // Check if admin setup is complete
      final response = await ApiClient().get('/api/v1/auth/initialized');
      // response is { "initialized": boolean }
      final initialized = (response as Map<String, dynamic>)['initialized'] == true;

      if (!initialized) {
        _needsSetup = true;
      } else {
        // If initialized, check if we have a valid session (are we logged in?)
         try {
           final session = await ApiClient().get('/api/v1/auth/session') as Map<String, dynamic>;
           if (session['authenticated'] != true) {
             // Not authenticated, but system IS initialized.
             // We reuse the Setup Wizard in "Login Mode".
             // The SetupWizard (implemented in features/setup) handles logic to show Login if already setup.
             _needsSetup = true;
           } else {
             _needsSetup = false;
             // Capture username for re-auth overlay pre-fill.
             if (session['user'] is String) {
               _lastKnownUsername = session['user'] as String;
             }
           }
         } on Object catch (_) {
           // If session check fails, assume we need login
           _needsSetup = true;
         }
      }
    } on Object catch (e) {
      debugPrint('System status check failed: $e');
      // If backend is down or unreachable, what do we do?
      // For now, assume we might need setup/login to be safe.
      _needsSetup = true;
    } finally {
      _isInitializing = false;
      // If already authenticated (returning user with valid session), transition to authenticated state
      if (!_needsSetup) {
        final showWelcome = Uri.base.queryParameters[_kWelcomeParam] == '1';
        if (showWelcome) {
          _clearWelcomeParam();
        }
        _onAuthenticated(isFirstSetup: showWelcome);
      } else {
        // Setup wizard will handle authentication — deactivate the reauth
        // interceptor so 401s during the setup flow don't trigger the overlay.
        _showReauth = false;
        ApiClient().onAuthRequired = null;
        ApiClient().completeReauth(success: false);
        notifyListeners();
      }
    }
  }

  /// Called from SetupWizard when login/setup completes.
  // Positional bool required to match SetupWizard `void Function(bool)` callback.
  // ignore: avoid_positional_boolean_parameters
  void completeSetup(bool isFirstSetupFlow) {
    _onAuthenticated(isFirstSetup: isFirstSetupFlow);
  }

  void _onAuthRequired() {
    if (_showReauth) return; // Already showing
    _showReauth = true;
    notifyListeners();
  }

  void onReauthSuccess() {
    _showReauth = false;
    // Reconnect event stream after re-auth.
    _eventStreamClient
      ?..disconnect(clearError: true)
      ..connect();
    notifyListeners();
  }

  void onReauthCancel() {
    _showReauth = false;
    unawaited(logout());
  }

  /// Single source of truth for "user is now authenticated" state transition.
  /// Handles event stream connection and optional first-setup welcome.
  void _onAuthenticated({required bool isFirstSetup}) {
    _needsSetup = false;

    // Clear any stale re-auth state — the user just completed a full
    // authentication flow (setup wizard or fresh session check), so any
    // prior "Session Expired" overlay is obsolete.
    _showReauth = false;
    ApiClient().completeReauth(success: true);

    // Activate the reauth interceptor now that the desktop is the active UI.
    // This is intentionally deferred from the constructor: during setup wizard
    // mode, 401s should NOT trigger the reauth overlay.
    ApiClient().onAuthRequired = _onAuthRequired;

    // Capture username for re-auth overlay pre-fill (best-effort, fire-and-forget).
    // Covers the fresh-login path where _checkSystemStatus didn't reach the session check.
    unawaited(ApiClient().get('/api/v1/auth/session').then((session) {
      if (session is Map && session['user'] is String) {
        _lastKnownUsername = session['user'] as String;
      }
    }).catchError((_) {}));

    // Connect event stream
    _eventStreamClient ??= EventStreamClient();
    _eventStreamClient!.onAuthFailure = _onAuthRequired;
    _eventStreamClient!.connect();

    notifyListeners();

    if (isFirstSetup) {
      unawaited(_showWelcomeOrUpgrade());
    }
  }

  Future<void> _showWelcomeOrUpgrade() async {
    if (await probeHttpsAvailable()) {
      // Redirect to HTTPS, preserving existing query params + adding welcome flag
      final uri = Uri.base;
      final params = Map<String, String>.from(uri.queryParameters)
        ..[_kWelcomeParam] = '1';
      final httpsUri = uri.replace(scheme: 'https', queryParameters: params);
      web.window.location.replace(httpsUri.toString());
      return;
    }
    // HTTPS unavailable — show welcome directly
    openApp(
      'welcome',
      'Welcome',
      PiccoloIcons.handWaving,
      WelcomeScreen(controller: this),
      initialSize: const Size(640, 420),
    );
  }

  void _clearWelcomeParam() {
    final params = Map<String, String>.from(Uri.base.queryParameters)
      ..remove(_kWelcomeParam);
    // Use query: '' when empty — queryParameters: null would preserve the
    // original query string (Dart Uri.replace semantics).
    final cleanUri = params.isEmpty
        ? Uri.base.replace(query: '')
        : Uri.base.replace(queryParameters: params);
    web.window.history.replaceState(JSObject(), '', cleanUri.toString());
  }

  /// Opens (or focuses) the Settings window.
  ///
  /// [initialTab] is only applied when opening a new window; if Settings is
  /// already open the existing window is focused on its current tab.
  void openSettings({int initialTab = 0}) {
    if (isAppOpen('settings')) {
      focusWindow('settings');
    } else {
      openApp(
        'settings',
        'Settings',
        PiccoloIcons.settings,
        SettingsApp(
          initialTab: initialTab,
          onLogout: logout,
          eventStreamClient: eventStreamClient,
        ),
        initialSize: const Size(1100, 750),
      );
    }
  }

  void openAppStore() {
    if (isAppOpen('app-store')) {
      focusWindow('app-store');
    } else {
      openApp(
        'app-store',
        'App Store',
        PiccoloIcons.store,
        AppStoreWindow(desktopController: this),
        initialSize: const Size(900, 650),
      );
    }
  }

  Future<void> logout() async {
    try {
      await ApiClient().post('/api/v1/auth/logout');
    } on Object catch (e) {
      debugPrint('Logout failed: $e');
    }

    // Disconnect event stream
    _eventStreamClient?.disconnect();
    _eventStreamClient?.dispose();
    _eventStreamClient = null;

    // Deactivate reauth interceptor — setup wizard will own auth now.
    _showReauth = false;
    ApiClient().onAuthRequired = null;
    ApiClient().completeReauth(success: false);
    _lastKnownUsername = null;

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
    var targetSize = initialSize ?? const Size(1024, 700);

    // 2. Clamp to Screen Size (Safety)
    if (screenSize != null) {
      // Ensure window isn't larger than the screen (minus dock area)
      final maxWidth = screenSize.width;
      final maxHeight = screenSize.height - kDockAreaHeight;

      if (targetSize.width > maxWidth) {
        targetSize = Size(maxWidth, targetSize.height);
      }
      if (targetSize.height > maxHeight) {
        targetSize = Size(targetSize.width, maxHeight);
      }
    }

    // 3. Calculate Position (Smart Center)
    double x;
    double y;

    if (screenSize != null) {
      // Available height for centering
      final availableHeight = screenSize.height - kTopMargin - kDockAreaHeight;

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
      final window = _windows.removeAt(index)
        // Ensure it's visible
        ..isMinimized = false;
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
    var changed = false;
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
        window
          ..position = window.preMaximizePosition!
          ..size = window.preMaximizeSize!;
      }
      window.isMaximized = false;
    } else {
      // Maximize
      // Save state
      window
        ..preMaximizePosition = window.position
        ..preMaximizeSize = window.size

        // Apply max dimensions with space for dock at bottom
        ..position = Offset.zero
        ..size = Size(
          availableSpace.width,
          availableSpace.height - kBottomReserve,
        )

        ..isMaximized = true;

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

    var x = newPosition.dx;
    var y = newPosition.dy;

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

    const minWidth = 300.0;
    const minHeight = 200.0;

    // Calculate maximum allowed dimensions based on current position
    // This ensures the bottom-right corner never leaves the screen
    final maxWidth = screenSize.width - window.position.dx;
    final maxHeight = screenSize.height - window.position.dy;

    var w = newSize.width;
    var h = newSize.height;

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
    ApiClient().onAuthRequired = null;
    ApiClient().completeReauth(success: false);
    _eventStreamClient?.dispose();
    _eventStreamClient = null;
    super.dispose();
  }
}
