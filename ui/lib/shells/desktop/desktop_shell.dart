import 'package:flutter/material.dart';
import 'desktop_controller.dart';
import 'widgets/stage.dart';
import 'widgets/top_bar.dart';
import 'widgets/dock.dart';
import 'widgets/window_frame.dart';
import 'features/setup/setup_wizard.dart';
import 'features/access_denied/access_denied_view.dart';

class DesktopShell extends StatefulWidget {
  const DesktopShell({super.key});

  @override
  State<DesktopShell> createState() => _DesktopShellState();
}

class _DesktopShellState extends State<DesktopShell> {
  // Instantiating the controller here. In a larger app, this might live in a Provider.
  final DesktopController _controller = DesktopController();

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final screenSize = MediaQuery.of(context).size;
    final uri = Uri.base;
    final isAccessDeniedRoute = uri.path == '/access-denied';
    final next = uri.queryParameters['next'];

    // ListenableBuilder listens to the controller and rebuilds only this widget when it changes.
    return ListenableBuilder(
      listenable: _controller,
      builder: (context, child) {
        return Scaffold(
          body: Stack(
            children: [
              // Layer B: The Stage (Background)
              const Positioned.fill(child: Stage()),

              // Layer A: Frame (Top Bar) - Always visible
              Positioned(
                top: 0,
                left: 0,
                right: 0,
                child: TopBar(controller: _controller),
              ),

              if (_controller.isInitializing)
                 const Positioned.fill(
                   child: Center(
                     child: CircularProgressIndicator(color: Colors.white),
                   ),
                 )
              else if (_controller.needsSetup)
                Positioned.fill(
                  child: SetupWizard(onComplete: _controller.completeSetup),
                )
              else if (isAccessDeniedRoute)
                Positioned.fill(
                  child: AccessDeniedView(next: next),
                )
              else ...[
                // Layer D: Windows
                ..._controller.windows
                    .where(
                      (w) => !w.isMinimized,
                    ) // Don't render minimized windows
                    .map(
                      (window) => WindowFrame(
                        key: ValueKey(window.id),
                        window: window,
                        isClosing: window.isClosing,
                        isActive: _controller.isAppActive(window.id),
                        onClose: () => _controller.closeWindow(window.id),
                        onMinimize: () => _controller.minimizeWindow(window.id),
                        onMaximize: () =>
                            _controller.maximizeWindow(window.id, screenSize),
                        onTap: () => _controller.focusWindow(window.id),
                        onDrag: (newPos) => _controller.moveWindow(
                          window.id,
                          newPos,
                          screenSize,
                        ),
                        onResize: (newSize) => _controller.resizeWindow(
                          window.id,
                          newSize,
                          screenSize,
                        ),
                        onAnimationComplete: () =>
                            _controller.removeWindowInternal(window.id),
                      ),
                    ),

                // Layer C: Launcher (Dock)
                Positioned(
                  bottom: 0,
                  left: 0,
                  right: 0,
                  child: Center(child: Dock(controller: _controller)),
                ),

                // Launcher Overlay (Example of interacting with state)
                if (_controller.isLauncherOpen)
                  Positioned.fill(
                    child: GestureDetector(
                      onTap: _controller.toggleLauncher, // Close on tap outside
                      child: Container(
                        color: Colors.black.withValues(alpha: 0.2),
                        child: const Center(
                          child: Card(
                            child: Padding(
                              padding: EdgeInsets.all(32.0),
                              child: Text("App Launcher Overlay"),
                            ),
                          ),
                        ),
                      ),
                    ),
                  ),
              ],
            ],
          ),
        );
      },
    );
  }
}
