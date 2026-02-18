import 'package:flutter/material.dart';
import 'package:flutter/rendering.dart';
import 'package:flutter/scheduler.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// A generic error boundary that intercepts paint and layout errors from a
/// child subtree, displaying a fallback UI instead of Flutter's red error screen.
///
/// On retry, cycles a [UniqueKey] on a [KeyedSubtree] to force a full subtree
/// rebuild -- creating fresh Elements, States, and RenderObjects for the child.
///
/// Example:
/// ```dart
/// RenderErrorBoundary(
///   maxRetries: 3,
///   fallbackBuilder: (error, retry) => Text(retry != null ? 'Retry' : 'Failed'),
///   onError: (error) => print('Caught: $error'),
///   child: SomeWidget(),
/// )
/// ```
class RenderErrorBoundary extends StatefulWidget {

  const RenderErrorBoundary({
    required this.fallbackBuilder, required this.child, super.key,
    this.maxRetries = 3,
    this.onError,
    this.onRetry,
  });
  /// Maximum number of retry attempts. 0 means unlimited retries.
  final int maxRetries;

  /// Builds the fallback UI shown when an error occurs.
  /// `retry` is null when [maxRetries] has been exceeded.
  final Widget Function(Object error, VoidCallback? retry) fallbackBuilder;

  /// Called on every error. Use for logging/reporting.
  final void Function(Object error)? onError;

  /// Called when the user triggers a retry, before the child is rebuilt.
  final VoidCallback? onRetry;

  /// The child subtree to protect.
  final Widget child;

  @override
  State<RenderErrorBoundary> createState() => _RenderErrorBoundaryState();
}

class _RenderErrorBoundaryState extends State<RenderErrorBoundary> {
  Object? _error;
  int _attempts = 0;
  Key _childKey = UniqueKey();

  void _onRenderError(Object error) {
    if (_error != null) return; // Already in error state
    widget.onError?.call(error);
    if (!mounted) return;
    setState(() {
      _error = error;
    });
  }

  void _retry() {
    widget.onRetry?.call();
    setState(() {
      _attempts++;
      _error = null;
      _childKey = UniqueKey();
    });
  }

  @override
  Widget build(BuildContext context) {
    if (_error != null) {
      final canRetry =
          widget.maxRetries == 0 || _attempts < widget.maxRetries;
      return widget.fallbackBuilder(_error!, canRetry ? _retry : null);
    }

    return _PaintErrorCatcher(
      onError: _onRenderError,
      child: KeyedSubtree(
        key: _childKey,
        child: widget.child,
      ),
    );
  }
}

/// SingleChildRenderObjectWidget that wraps paint/layout in try/catch.
class _PaintErrorCatcher extends SingleChildRenderObjectWidget {

  const _PaintErrorCatcher({
    required this.onError,
    required super.child,
  });
  final void Function(Object error) onError;

  @override
  RenderObject createRenderObject(BuildContext context) {
    return _RenderPaintErrorCatcher(onError: onError);
  }

  @override
  void updateRenderObject(
      BuildContext context, _RenderPaintErrorCatcher renderObject) {
    renderObject.onError = onError;
  }
}

/// Default fallback UI for render error boundaries.
/// Shows a warning icon, label, and optional reconnect button on a dark background.
class RenderErrorFallback extends StatelessWidget {

  const RenderErrorFallback({
    required this.label, super.key,
    this.retry,
  });
  final String label;
  final VoidCallback? retry;

  @override
  Widget build(BuildContext context) {
    return ColoredBox(
      color: PiccoloTheme.terminalBg,
      child: Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            const Icon(PiccoloIcons.warning, color: PiccoloTheme.warning, size: 48),
            const SizedBox(height: Spacing.base),
            Text(
              retry != null
                  ? '$label encountered a rendering error'
                  : '$label unavailable',
              style: const TextStyle(color: Colors.white70, fontSize: 14),
            ),
            const SizedBox(height: Spacing.base),
            if (retry != null)
              FilledButton.icon(
                onPressed: retry,
                icon: const Icon(PiccoloIcons.refresh),
                label: const Text('Reconnect'),
              ),
          ],
        ),
      ),
    );
  }
}

class _RenderPaintErrorCatcher extends RenderProxyBox {

  _RenderPaintErrorCatcher({required this.onError});
  void Function(Object error) onError;
  bool _errorNotified = false;

  // _errorNotified is intentionally never reset -- key-cycling in
  // _RenderErrorBoundaryState creates a fresh render object on retry.
  void _scheduleErrorNotification(Object error) {
    if (_errorNotified) return;
    _errorNotified = true;
    // Read onError live at dispatch time rather than capturing early --
    // attached check guards liveness, and live read stays current if
    // updateRenderObject swaps the callback between scheduling and dispatch.
    SchedulerBinding.instance.addPostFrameCallback((_) {
      if (attached) {
        onError(error);
      }
    });
  }

  @override
  void performLayout() {
    try {
      super.performLayout();
    } on Object catch (e) {
      // Use constrain(Size.zero) to get minimum size -- always finite,
      // even if parent provides unbounded constraints (e.g., inside Column).
      size = constraints.constrain(Size.zero);
      _scheduleErrorNotification(e);
    }
  }

  @override
  void paint(PaintingContext context, Offset offset) {
    try {
      super.paint(context, offset);
    } on Object catch (e) {
      // Paint a dark fallback rect so the area isn't blank
      context.canvas.drawRect(
        offset & size,
        Paint()..color = PiccoloTheme.terminalBg,
      );
      _scheduleErrorNotification(e);
    }
  }
}
