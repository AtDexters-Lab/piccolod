import 'dart:async';
import 'dart:convert';
import 'dart:js_interop';

import 'package:flutter/foundation.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:web/web.dart' as web;

/// Captures UI errors, deduplicates them in-memory, and batch-posts
/// to the backend telemetry endpoint for systemd journal logging.
class ErrorReporter {
  factory ErrorReporter() => _instance;
  ErrorReporter._internal();
  static final ErrorReporter _instance = ErrorReporter._internal();

  // Buffer cap is sized so the worst-case serialized batch fits the beacon /
  // keepalive-fetch per-request body limit (~64 KiB on Chromium). 20 entries
  // × ~3 KiB upper bound (msg 512 + stack 2048 + envelope) ≈ 60 KiB. The
  // backend MaxBytesReader is also 64 KiB; both clip in the same place so a
  // payload that fits the client always fits the server.
  static const _maxBufferSize = 20;
  static const _maxFingerprintLen = 128;
  static const _flushThreshold = 10;
  static const _normalFlushInterval = Duration(seconds: 30);
  static const _backoffFlushInterval = Duration(seconds: 120);
  static const _backoffThreshold = 3;
  static const _telemetryPath = '/api/v1/telemetry/log';

  final Map<String, _ErrorRecord> _buffer = {};
  // Snapshot of entries currently in the periodic POST. Visible to the
  // durable-flush path so a navigation that arrives mid-await doesn't lose
  // them: `_buffer` is empty during the await, but `_inflightEntries` holds
  // the in-flight set and gets bundled into the beacon payload.
  Map<String, _ErrorRecord>? _inflightEntries;
  Timer? _flushTimer;
  String _route = 'unknown';
  String _version = 'unknown';
  bool _flushing = false;
  bool _initialized = false;
  bool _beaconScheduled = false;
  DateTime? _lastDurableFlushAt;
  int _consecutiveFailures = 0;

  // Cooldown between durable (beacon/keepalive) flushes. The successful path
  // already self-deduplicates via `_buffer.isEmpty` after `sendBeacon` clears
  // — this guard exists for the rejected-beacon case where the buffer survives
  // and pagehide+visibilitychange would otherwise fire a duplicate keepalive.
  static const _durableFlushCooldown = Duration(milliseconds: 250);

  /// Initialize the error reporter with route context.
  /// Should be called once at app startup.
  void initialize({required String route}) {
    if (_initialized) return;
    _initialized = true;
    _route = route;
    _startFlushTimer();
    _registerPageLifecycleHooks();
    unawaited(_fetchVersion());
  }

  /// Report an error or event for telemetry.
  /// When [flushImmediate] is true, the buffer is flushed via
  /// `navigator.sendBeacon`, which the browser commits before tearing down
  /// the page — durable across navigation, reload, and tab close. Use for
  /// rare high-signal events (passkey_error, recovery-key telemetry) where
  /// losing the entry to a same-tick navigation would mask a real bug.
  void report({
    required String type,
    required String message,
    String? stack,
    bool flushImmediate = false,
  }) {
    try {
      if (!_initialized) return;

      final fingerprint = _computeFingerprint(type, message, stack);

      if (_buffer.containsKey(fingerprint)) {
        _buffer[fingerprint]!.count++;
      } else {
        // Evict oldest entry if buffer is full
        if (_buffer.length >= _maxBufferSize) {
          _buffer.remove(_buffer.keys.first);
        }
        _buffer[fingerprint] = _ErrorRecord(
          type: type,
          message: _truncate(message, 512),
          stack: stack != null ? _truncate(stack, 2048) : null,
          route: _route,
          count: 1,
          ts: DateTime.now().toUtc().toIso8601String(),
        );
      }

      if (flushImmediate) {
        // Coalesce same-tick bursts so we don't fire one beacon per report.
        // The microtask runs before the next event loop turn, which is before
        // any pending navigation, so durability is preserved.
        if (!_beaconScheduled) {
          _beaconScheduled = true;
          scheduleMicrotask(() {
            _beaconScheduled = false;
            _flushViaBeacon();
          });
        }
      } else if (_buffer.length >= _flushThreshold) {
        unawaited(_flush());
      }
    } on Object catch (_) {
      // Reporter must never throw
    }
  }

  /// Register pagehide/visibilitychange to drain the buffer at end-of-life.
  /// pagehide fires reliably on both bfcache navigation and tab close;
  /// visibilitychange catches the tab-becomes-hidden case (mobile, alt-tab)
  /// where pagehide may not fire. Both flush via sendBeacon so the in-flight
  /// commit survives whatever lifecycle event triggered it.
  void _registerPageLifecycleHooks() {
    web.window.addEventListener(
      'pagehide',
      ((web.Event _) => _flushViaBeacon()).toJS,
    );
    web.document.addEventListener(
      'visibilitychange',
      ((web.Event _) {
        if (web.document.visibilityState == 'hidden') {
          _flushViaBeacon();
        }
      }).toJS,
    );
  }

  void _startFlushTimer() {
    _flushTimer?.cancel();
    final interval = _consecutiveFailures >= _backoffThreshold
        ? _backoffFlushInterval
        : _normalFlushInterval;
    _flushTimer = Timer.periodic(interval, (_) => _flush());
  }

  Future<void> _flush() async {
    try {
      if (_flushing || _buffer.isEmpty) return;
      _flushing = true;

      final entries = Map<String, _ErrorRecord>.from(_buffer);
      _buffer.clear();
      _inflightEntries = entries;

      final payload = _composePayload(entries.values);

      try {
        await ApiClient().post(
          _telemetryPath,
          body: payload,
        );
        _inflightEntries = null;
        if (_consecutiveFailures >= _backoffThreshold) {
          _consecutiveFailures = 0;
          _startFlushTimer(); // Restore normal flush interval
        } else {
          _consecutiveFailures = 0;
        }
      } on Object catch (_) {
        _inflightEntries = null;
        // On failure, merge entries back into buffer
        for (final entry in entries.entries) {
          if (_buffer.containsKey(entry.key)) {
            _buffer[entry.key]!.count += entry.value.count;
          } else if (_buffer.length < _maxBufferSize) {
            _buffer[entry.key] = entry.value;
          }
        }
        _consecutiveFailures++;
        if (_consecutiveFailures == _backoffThreshold) {
          _startFlushTimer(); // Switch to backoff interval
        }
      }
    } on Object catch (_) {
      // Reporter must never throw
    } finally {
      _flushing = false;
    }
  }

  /// Best-effort durable flush. Survives page navigation/unload by handing
  /// the request to the browser before the document tears down — the failure
  /// mode that motivated this path. Tries `navigator.sendBeacon` first;
  /// on rejection (per-origin queue full) falls back to
  /// `fetch(..., keepalive: true)`. The two paths share the ~64 KiB body
  /// limit per spec; the buffer cap (`_maxBufferSize`) is sized so a worst-
  /// case payload fits before either gate. Periodic-flush in-flight entries
  /// (`_inflightEntries`) are bundled in too, so a navigation that arrives
  /// mid-await doesn't lose them.
  void _flushViaBeacon() {
    try {
      final inflight = _inflightEntries;
      if (_buffer.isEmpty && (inflight == null || inflight.isEmpty)) {
        return;
      }
      final now = DateTime.now();
      if (_lastDurableFlushAt != null &&
          now.difference(_lastDurableFlushAt!) < _durableFlushCooldown) {
        return;
      }
      _lastDurableFlushAt = now;

      final allEntries = <_ErrorRecord>[
        if (inflight != null) ...inflight.values,
        ..._buffer.values,
      ];
      final body = jsonEncode(_composePayload(allEntries));
      final blob = web.Blob(
        [body.toJS].toJS,
        web.BlobPropertyBag(type: 'application/json'),
      );
      // sendBeacon's `true` return means the browser accepted for transmission,
      // not that the server accepted. Buffer clears either way: once the
      // request leaves Dart-land we have no way to retry from a torn-down
      // page. Server-side observability (request-rate on /telemetry/log) is
      // the safety net for backend-side drops. We do NOT clear
      // `_inflightEntries` — the periodic _flush owns its lifecycle (its
      // own resolution path nulls it). Worst case is one duplicate set of
      // entries on the server when both the periodic POST and the beacon
      // succeed; backend dedup is downstream of this code.
      if (web.window.navigator.sendBeacon(_telemetryPath, blob)) {
        _buffer.clear();
        _consecutiveFailures = 0;
        return;
      }
      // Beacon rejected (per-origin queue full). Fall through to keepalive
      // fetch — same ~64 KiB body cap but a separate queue, so a queue-full
      // rejection is genuinely recoverable here.
      _flushViaKeepalive(body);
    } on Object catch (_) {
      // Reporter must never throw
    }
  }

  void _flushViaKeepalive(String body) {
    try {
      final init = web.RequestInit(
        method: 'POST',
        body: body.toJS,
        keepalive: true,
        headers: {'Content-Type': 'application/json'}.jsify()! as web.HeadersInit,
      );
      final promise = web.window.fetch(_telemetryPath.toJS, init);
      unawaited(promise.toDart.then((response) {
        // fetch resolves on any HTTP status — only treat 2xx as accepted.
        // 4xx/5xx leaves the buffer intact so the next periodic flush can
        // retry (assuming the page survived). Telemetry already accepts
        // some loss; we don't try to retry from a torn-down page.
        if (response.status >= 200 && response.status < 300) {
          _buffer.clear();
          _consecutiveFailures = 0;
        } else {
          debugPrint('error_reporter: keepalive returned ${response.status}');
        }
      }).catchError((Object _) {
        // Network refused outright. Buffer stays; if the page survives, the
        // periodic flush will retry.
        debugPrint('error_reporter: keepalive fetch rejected');
      }));
    } on Object catch (_) {
      // Reporter must never throw
    }
  }

  Map<String, dynamic> _composePayload(Iterable<_ErrorRecord> entries) {
    return {
      'entries': entries.map((e) => e.toJson()).toList(),
      'session': {
        'piccolo_version': _version,
        'screen': '${web.window.screen.width}x${web.window.screen.height}',
      },
    };
  }

  Future<void> _fetchVersion() async {
    try {
      final response = await ApiClient().get('/version');
      if (response is Map && response.containsKey('version')) {
        _version = (response['version'] as String?) ?? 'unknown';
      }
    } on Object catch (_) {
      // Keep default 'unknown'
    }
  }

  String _computeFingerprint(String type, String message, String? stack) {
    final msgPrefix = _truncate(message, 80);
    final firstLine = stack != null ? _firstLine(stack) : '';
    return _truncate('$type|$msgPrefix|$firstLine', _maxFingerprintLen);
  }

  static String _firstLine(String s) {
    final idx = s.indexOf('\n');
    return idx < 0 ? s : s.substring(0, idx);
  }

  static String _truncate(String s, int maxLen) {
    return s.length <= maxLen ? s : s.substring(0, maxLen);
  }
}

class _ErrorRecord {

  _ErrorRecord({
    required this.type,
    required this.message,
    required this.route, required this.count, required this.ts, this.stack,
  });
  final String type;
  final String message;
  final String? stack;
  final String route;
  final String ts;
  int count;

  Map<String, dynamic> toJson() => {
        'type': type,
        'message': message,
        if (stack != null) 'stack': stack,
        'route': route,
        'count': count,
        'ts': ts,
      };
}
