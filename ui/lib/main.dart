import 'dart:js_interop';
import 'dart:ui';

import 'package:flutter/material.dart';
import 'package:flutter/scheduler.dart';
import 'package:piccolo_os/core/services/error_reporter.dart';
import 'package:piccolo_os/core/utils/https_probe.dart';
import 'package:piccolo_os/shells/desktop/desktop_shell.dart';
import 'package:piccolo_os/shells/gateway/gateway_shell.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:web/web.dart' as web;

void main() async {
  WidgetsFlutterBinding.ensureInitialized();

  // Attempt HTTPS upgrade before anything else (no-op if already HTTPS or dev)
  if (await _tryUpgradeToHttps()) return;

  // Determine if this is a gateway access (piccolo.local)
  final isGateway = _isGatewayAccess();

  final reporter = ErrorReporter()
    ..initialize(route: isGateway ? 'gateway' : 'desktop');

  // 1. Flutter framework errors (layout, rendering, build)
  FlutterError.onError = (details) {
    reporter.report(
      type: 'flutter_error',
      message: details.exceptionAsString(),
      stack: details.stack?.toString(),
    );
    FlutterError.presentError(details);
  };

  // 2. Unhandled async exceptions
  PlatformDispatcher.instance.onError = (error, stack) {
    reporter.report(
      type: 'async_error',
      message: error.toString(),
      stack: stack.toString(),
    );
    return true;
  };

  // 3. Web/WASM errors
  _registerWebErrorListeners(reporter);

  // 4. Jank detection (frames > 100ms)
  _registerJankDetector(reporter);

  if (isGateway) {
    runApp(const GatewayShell());
  } else {
    runApp(const PiccoloApp());
  }
}

void _registerWebErrorListeners(ErrorReporter reporter) {
  web.window.addEventListener(
    'error',
    ((web.ErrorEvent event) {
      reporter.report(
        type: 'web_error',
        message: event.message,
      );
    }).toJS,
  );

  web.window.addEventListener(
    'unhandledrejection',
    ((web.PromiseRejectionEvent event) {
      reporter.report(
        type: 'js_rejection',
        message: event.reason?.toString() ?? 'unknown rejection',
      );
    }).toJS,
  );
}

void _registerJankDetector(ErrorReporter reporter) {
  SchedulerBinding.instance.addTimingsCallback((timings) {
    for (final timing in timings) {
      final ms = timing.totalSpan.inMilliseconds;
      if (ms > 100) {
        // Bucket durations for dedup: 100-200, 200-500, >500ms
        final bucket = ms > 500
            ? '>500ms'
            : ms > 200
                ? '200-500ms'
                : '100-200ms';
        reporter.report(type: 'jank', message: 'frame jank $bucket');
      }
    }
  });
}

/// Probes HTTPS on the current host and redirects if the CA is trusted.
/// Returns true if a redirect was initiated (caller should bail out).
Future<bool> _tryUpgradeToHttps() async {
  if (await probeHttpsAvailable()) {
    final httpsUrl = Uri.base.replace(scheme: 'https').toString();
    web.window.location.replace(httpsUrl);
    return true;
  }
  return false;
}

/// Checks if the current access is via the gateway domain (piccolo.local).
/// Returns true if the hostname is exactly "piccolo.local".
bool _isGatewayAccess() {
  final host = web.window.location.hostname.toLowerCase();
  return host == 'piccolo.local';
}

class PiccoloApp extends StatelessWidget {
  const PiccoloApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Piccolo',
      debugShowCheckedModeBanner: false,
      theme: PiccoloTheme.lightTheme,
      // In the future, we can detect platform/screen size here to choose shell.
      home: const DesktopShell(),
    );
  }
}
