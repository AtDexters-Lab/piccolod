import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/shells/desktop/features/settings/settings_controller.dart';

void main() {
  Map<String, dynamic> statusJson(Map<String, dynamic> meta) => {
    'current_version': 'v0.2.40',
    'available_version': 'v0.2.40',
    'pending': false,
    'requires_reboot': false,
    'last_checked': '2026-07-24T10:00:00Z',
    'meta': meta,
  };

  test(
    'uncertain 200 status is not treated as backend operation busy',
    () async {
      final controller = SettingsController(
        osUpdateStatusFetcher: () async => statusJson({
          'stale': true,
          'refreshing': true,
          'degraded': true,
        }),
      );
      addTearDown(controller.dispose);

      final result = await controller.fetchOSUpdate();

      expect(result, OSUpdateStatusFetchResult.success);
      expect(controller.osUpdate?.isUncertain, isTrue);
      expect(controller.isBackendBusy, isFalse);
      expect(controller.isUpdateInProgress, isFalse);
      expect(controller.isOSUpdateLoading, isFalse);
      expect(controller.osUpdateError, isNull);
    },
  );

  test(
    'transport failure is distinct from a successful retained status',
    () async {
      var requestCount = 0;
      final controller = SettingsController(
        osUpdateStatusFetcher: () async {
          requestCount++;
          if (requestCount == 1) {
            return {
              ...statusJson(const {}),
              'available_version': 'v0.2.41',
              'pending': true,
              'requires_reboot': true,
            };
          }
          throw StateError('network unavailable');
        },
      );
      addTearDown(controller.dispose);

      expect(
        await controller.fetchOSUpdate(),
        OSUpdateStatusFetchResult.success,
      );
      expect(controller.osUpdate?.pending, isTrue);

      expect(
        await controller.fetchOSUpdate(silent: true),
        OSUpdateStatusFetchResult.failed,
      );
      expect(controller.osUpdate?.pending, isTrue);
      expect(controller.isBackendBusy, isFalse);
      expect(controller.isLoading, isFalse);
      expect(controller.osUpdateError, 'Unable to refresh update status.');
      expect(controller.error, isNull);
    },
  );
}
