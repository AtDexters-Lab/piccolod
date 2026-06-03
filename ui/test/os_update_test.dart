import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/core/models/os_update.dart';

void main() {
  Map<String, dynamic> statusJson(Map<String, dynamic> meta) => {
    'current_version': 'v0.2.25',
    'available_version': 'v0.2.25',
    'pending': false,
    'requires_reboot': false,
    'last_checked': '2026-06-03T10:00:00Z',
    'meta': meta,
  };

  test('stale refreshing status is treated as uncertain', () {
    final update = OSUpdate.fromJson(
      statusJson({
        'stale': true,
        'refreshing': true,
        'rpm_updates_available': 2,
      }),
    );

    expect(update.stale, isTrue);
    expect(update.refreshing, isTrue);
    expect(update.degraded, isFalse);
    expect(update.cacheEmpty, isFalse);
    expect(update.rpmUpdatesAvailable, 2);
    expect(update.isUncertain, isTrue);
  });

  test('cold degraded status is treated as uncertain', () {
    final update = OSUpdate.fromJson(
      statusJson({
        'degraded': true,
        'refreshing': true,
        'cache_empty': true,
      }),
    );

    expect(update.stale, isFalse);
    expect(update.refreshing, isTrue);
    expect(update.degraded, isTrue);
    expect(update.cacheEmpty, isTrue);
    expect(update.isUncertain, isTrue);
  });

  test('fresh status is not treated as uncertain', () {
    final update = OSUpdate.fromJson(statusJson({}));

    expect(update.isUncertain, isFalse);
  });
}
