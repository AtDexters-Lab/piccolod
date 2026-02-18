import 'dart:async';

import 'package:piccolo_os/core/services/api_client.dart';
import 'package:web/web.dart' as web;

/// Captures UI errors, deduplicates them in-memory, and batch-posts
/// to the backend telemetry endpoint for systemd journal logging.
class ErrorReporter {
  factory ErrorReporter() => _instance;
  ErrorReporter._internal();
  static final ErrorReporter _instance = ErrorReporter._internal();

  static const _maxBufferSize = 50;
  static const _maxFingerprintLen = 128;
  static const _flushThreshold = 10;
  static const _normalFlushInterval = Duration(seconds: 30);
  static const _backoffFlushInterval = Duration(seconds: 120);
  static const _backoffThreshold = 3;

  final Map<String, _ErrorRecord> _buffer = {};
  Timer? _flushTimer;
  String _route = 'unknown';
  String _version = 'unknown';
  bool _flushing = false;
  bool _initialized = false;
  int _consecutiveFailures = 0;

  /// Initialize the error reporter with route context.
  /// Should be called once at app startup.
  void initialize({required String route}) {
    if (_initialized) return;
    _initialized = true;
    _route = route;
    _startFlushTimer();
    unawaited(_fetchVersion());
  }

  /// Report an error or event for telemetry.
  void report({
    required String type,
    required String message,
    String? stack,
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

      if (_buffer.length >= _flushThreshold) {
        unawaited(_flush());
      }
    } on Object catch (_) {
      // Reporter must never throw
    }
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

      // Snapshot and clear buffer
      final entries = Map<String, _ErrorRecord>.from(_buffer);
      _buffer.clear();

      final payload = {
        'entries': entries.values.map((e) => e.toJson()).toList(),
        'session': {
          'piccolo_version': _version,
          'screen': '${web.window.screen.width}x${web.window.screen.height}',
        },
      };

      try {
        await ApiClient().post(
          '/api/v1/telemetry/log',
          body: payload,
        );
        if (_consecutiveFailures >= _backoffThreshold) {
          _consecutiveFailures = 0;
          _startFlushTimer(); // Restore normal flush interval
        } else {
          _consecutiveFailures = 0;
        }
      } on Object catch (_) {
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
