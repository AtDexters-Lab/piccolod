import 'package:flutter/material.dart';
import 'package:pointer_interceptor/pointer_interceptor.dart';
import '../../../theme/piccolo_theme.dart';
import '../models/desktop_window.dart';
import 'window_activity.dart';

class WindowFrame extends StatefulWidget {
  final DesktopWindow window;
  final bool isClosing; // Primitive for accurate didUpdateWidget comparison
  final bool isActive;
  final VoidCallback onClose;
  final VoidCallback onMinimize;
  final VoidCallback onMaximize;
  final VoidCallback onTap;
  final Function(Offset) onDrag;
  final Function(Size) onResize;
  final VoidCallback onAnimationComplete;

  const WindowFrame({
    super.key,
    required this.window,
    required this.isClosing,
    required this.isActive,
    required this.onClose,
    required this.onMinimize,
    required this.onMaximize,
    required this.onTap,
    required this.onDrag,
    required this.onResize,
    required this.onAnimationComplete,
  });

  @override
  State<WindowFrame> createState() => _WindowFrameState();
}

class _WindowFrameState extends State<WindowFrame>
    with SingleTickerProviderStateMixin {
  bool _isHoveringHeader = false;
  late AnimationController _entryController;
  late Animation<double> _scaleAnimation;
  late Animation<double> _opacityAnimation;

  @override
  void initState() {
    super.initState();
    _entryController = AnimationController(
      vsync: this,
      duration: const Duration(milliseconds: 200),
    );

    _entryController.addStatusListener((status) {
      // Use widget.isClosing here to confirm intent
      if (status == AnimationStatus.dismissed && widget.isClosing) {
        widget.onAnimationComplete();
      }
    });

    _scaleAnimation = Tween<double>(
      begin: 0.9,
      end: 1.0,
    ).animate(CurvedAnimation(parent: _entryController, curve: Curves.easeOut));
    _opacityAnimation = Tween<double>(
      begin: 0.0,
      end: 1.0,
    ).animate(CurvedAnimation(parent: _entryController, curve: Curves.easeOut));

    // Start open animation
    _entryController.forward();
  }

  @override
  void didUpdateWidget(WindowFrame oldWidget) {
    super.didUpdateWidget(oldWidget);
    // Compare primitives to detect state change correctly
    if (widget.isClosing && !oldWidget.isClosing) {
      _entryController.reverse();
    }
  }

  @override
  void dispose() {
    _entryController.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    // If minimized, we shouldn't be here (filtered by Shell), but safe to return empty.
    if (widget.window.isMinimized && !widget.isClosing) {
      return const SizedBox.shrink();
    }

    return Positioned(
      left: widget.window.position.dx,
      top: widget.window.position.dy,
      child: PointerInterceptor(
        // Intercept if it's a native window.
        // WebViews now self-manage their interactivity via WindowActivity.
        intercepting: widget.window.requiresInterceptor,
        child: WindowActivity(
          isActive: widget.isActive,
          child: AnimatedBuilder(
            animation: _entryController,
            builder: (context, child) {
              return Transform.scale(
                scale: _scaleAnimation.value,
                child: Opacity(opacity: _opacityAnimation.value, child: child),
              );
            },
            child: Listener(
              onPointerDown: (_) => widget.onTap(), // Focus immediately on touch/click down
              child: Stack(
              children: [
                // Main Window Content
              AnimatedContainer(
                duration: const Duration(milliseconds: 200),
                curve: Curves.easeOutCubic,
                width: widget.window.size.width,
                height: widget.window.size.height,
                decoration: BoxDecoration(
                  color: PiccoloTheme.porcelain,
                  borderRadius: BorderRadius.circular(
                    widget.window.isMaximized ? 0 : 14,
                  ),
                  boxShadow: [
                    BoxShadow(
                      color: const Color(0xFF0E1322).withValues(alpha: 0.12),
                      blurRadius: 20,
                      offset: const Offset(0, 8),
                    ),
                    BoxShadow(
                      color: const Color(0xFF0E1322).withValues(alpha: 0.08),
                      blurRadius: 6,
                      offset: const Offset(0, 2),
                    ),
                  ],
                  border: Border.all(
                    color: PiccoloTheme.ink.withValues(alpha: 0.08),
                    width: 1,
                  ),
                ),
                child: Column(
                  children: [
                    // -- Window Header (Draggable) --
                    MouseRegion(
                      onEnter: (_) => setState(() => _isHoveringHeader = true),
                      onExit: (_) => setState(() => _isHoveringHeader = false),
                      child: GestureDetector(
                        onPanUpdate: (details) {
                          widget.onDrag(widget.window.position + details.delta);
                        },
                        // Removed onDoubleTap here to fix button lag
                        child: Container(
                          height: 40,
                          decoration: BoxDecoration(
                            color: Colors.transparent,
                            borderRadius: BorderRadius.vertical(
                              top: Radius.circular(
                                widget.window.isMaximized ? 0 : 14,
                              ),
                            ),
                          ),
                          padding: const EdgeInsets.symmetric(horizontal: 12),
                          child: Row(
                            children: [
                              // Traffic Lights
                              _WindowCaptionButton(
                                color: PiccoloTheme.critical,
                                icon: Icons.close,
                                isHoveringWindow: _isHoveringHeader,
                                onTap: widget.onClose,
                              ),
                              const SizedBox(width: 8),
                              _WindowCaptionButton(
                                color: PiccoloTheme.warning,
                                icon: Icons.remove,
                                isHoveringWindow: _isHoveringHeader,
                                onTap: widget.onMinimize,
                              ),
                              const SizedBox(width: 8),
                              _WindowCaptionButton(
                                color: PiccoloTheme.success,
                                icon: widget.window.isMaximized
                                    ? Icons.filter_none
                                    : Icons.crop_square,
                                isHoveringWindow: _isHoveringHeader,
                                onTap: widget.onMaximize,
                              ),

                              const SizedBox(width: 16),

                              // Title Area - Double Tap to Maximize lives here
                              Expanded(
                                child: GestureDetector(
                                  behavior: HitTestBehavior.translucent,
                                  onDoubleTap: widget.onMaximize,
                                  child: SizedBox(
                                    height: double.infinity,
                                    child: Align(
                                      alignment: Alignment.centerLeft,
                                      child: Text(
                                        widget.window.title,
                                        style: PiccoloTheme.textTheme.labelSmall
                                            ?.copyWith(
                                              fontWeight: FontWeight.w600,
                                            ),
                                      ),
                                    ),
                                  ),
                                ),
                              ),
                              
                              if (widget.window.actions != null) ...[
                                const SizedBox(width: 8),
                                ...widget.window.actions!,
                              ],
                            ],
                          ),
                        ),
                      ),
                    ),

                    // -- App Content --
                    Expanded(
                      child: ClipRRect(
                        borderRadius: BorderRadius.vertical(
                          bottom: Radius.circular(
                            widget.window.isMaximized ? 0 : 14,
                          ),
                        ),
                        child: widget.window.child,
                      ),
                    ),
                  ],
                ),
              ),

              // -- Resize Handle (Bottom Right) --
              if (!widget.window.isMaximized)
                Positioned(
                  right: 0,
                  bottom: 0,
                  child: PointerInterceptor(
                    child: GestureDetector(
                    onPanUpdate: (details) {
                      widget.onResize(
                        Size(
                          widget.window.size.width + details.delta.dx,
                          widget.window.size.height + details.delta.dy,
                        ),
                      );
                    },
                    child: MouseRegion(
                      cursor: SystemMouseCursors.resizeDownRight,
                      child: Container(
                        width: 20,
                        height: 20,
                        color: Colors.transparent, // Hitbox
                        child: Align(
                          alignment: Alignment.bottomRight,
                          child: Container(
                            width: 6,
                            height: 6,
                            margin: const EdgeInsets.all(4),
                            decoration: BoxDecoration(
                              color: PiccoloTheme.ink.withValues(alpha: 0.2),
                              borderRadius: const BorderRadius.only(
                                topLeft: Radius.circular(4),
                              ),
                            ),
                          ),
                        ),
                      ),
                    ),
                  ),
                  ),
                ),
            ],
          ),
        ),
        ),
        ),
      ),
    );
  }
}

class _WindowCaptionButton extends StatefulWidget {
  final Color color;
  final IconData icon;
  final bool isHoveringWindow;
  final VoidCallback? onTap;

  const _WindowCaptionButton({
    required this.color,
    required this.icon,
    required this.isHoveringWindow,
    this.onTap,
  });

  @override
  State<_WindowCaptionButton> createState() => _WindowCaptionButtonState();
}

class _WindowCaptionButtonState extends State<_WindowCaptionButton> {
  bool _isHovering = false;

  @override
  Widget build(BuildContext context) {
    // Show icon if hovering THIS button OR hovering the header area (macOS style)
    final showIcon = _isHovering || widget.isHoveringWindow;

    return GestureDetector(
      onTap: widget.onTap,
      child: MouseRegion(
        onEnter: (_) => setState(() => _isHovering = true),
        onExit: (_) => setState(() => _isHovering = false),
        // Hit target is 24x24, visual circle is 12x12
        child: Container(
          width: 24,
          height: 24,
          color: Colors.transparent,
          alignment: Alignment.center,
          child: Container(
            width: 12,
            height: 12,
            decoration: BoxDecoration(
              color: widget.color,
              shape: BoxShape.circle,
            ),
            child: Center(
              child: AnimatedOpacity(
                duration: const Duration(milliseconds: 100),
                opacity: showIcon ? 1.0 : 0.0,
                child: Icon(
                  widget.icon,
                  size: 10,
                  color: Colors.black.withValues(alpha: 0.8),
                ),
              ),
            ),
          ),
        ),
      ),
    );
  }
}
