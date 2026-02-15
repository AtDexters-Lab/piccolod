import 'package:flutter/material.dart';
import 'package:flutter/rendering.dart';
import 'package:flutter/scheduler.dart';

/// A generic error boundary that intercepts paint and layout errors from a
/// child subtree, displaying a fallback UI instead of Flutter's red error screen.
///
/// On retry, cycles a [UniqueKey] on a [KeyedSubtree] to force a full subtree
/// rebuild — creating fresh Elements, States, and RenderObjects for the child.
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
  /// Maximum number of retry attempts. 0 means unlimited retries.
  final int maxRetries;

  /// Builds the fallback UI shown when an error occurs.
  /// [retry] is null when [maxRetries] has been exceeded.
  final Widget Function(Object error, VoidCallback? retry) fallbackBuilder;

  /// Called on every error. Use for logging/reporting.
  final void Function(Object error)? onError;

  /// Called when the user triggers a retry, before the child is rebuilt.
  final VoidCallback? onRetry;

  /// The child subtree to protect.
  final Widget child;

  const RenderErrorBoundary({
    super.key,
    this.maxRetries = 3,
    required this.fallbackBuilder,
    this.onError,
    this.onRetry,
    required this.child,
  });

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
  final void Function(Object error) onError;

  const _PaintErrorCatcher({
    required this.onError,
    required super.child,
  });

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
  final String label;
  final VoidCallback? retry;

  const RenderErrorFallback({
    super.key,
    required this.label,
    this.retry,
  });

  @override
  Widget build(BuildContext context) {
    return Container(
      color: const Color(0xFF1E1E1E),
      child: Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            const Icon(Icons.warning_amber_rounded, color: Colors.amber, size: 48),
            const SizedBox(height: 16),
            Text(
              retry != null
                  ? '$label encountered a rendering error'
                  : '$label unavailable',
              style: const TextStyle(color: Colors.white70, fontSize: 14),
            ),
            const SizedBox(height: 16),
            if (retry != null)
              FilledButton.icon(
                onPressed: retry,
                icon: const Icon(Icons.refresh),
                label: const Text('Reconnect'),
              ),
          ],
        ),
      ),
    );
  }
}

class _RenderPaintErrorCatcher extends RenderProxyBox {
  void Function(Object error) onError;
  bool _errorNotified = false;

  _RenderPaintErrorCatcher({required this.onError});

  // _errorNotified is intentionally never reset — key-cycling in
  // _RenderErrorBoundaryState creates a fresh render object on retry.
  void _scheduleErrorNotification(Object error) {
    if (_errorNotified) return;
    _errorNotified = true;
    // Read onError live at dispatch time rather than capturing early —
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
    } catch (e) {
      // Use constrain(Size.zero) to get minimum size — always finite,
      // even if parent provides unbounded constraints (e.g., inside Column).
      size = constraints.constrain(Size.zero);
      _scheduleErrorNotification(e);
    }
  }

  @override
  void paint(PaintingContext context, Offset offset) {
    try {
      super.paint(context, offset);
    } catch (e) {
      // Paint a dark fallback rect so the area isn't blank
      context.canvas.drawRect(
        offset & size,
        Paint()..color = const Color(0xFF1E1E1E),
      );
      _scheduleErrorNotification(e);
    }
  }
}
