import 'dart:io';

import 'package:flutter_test/flutter_test.dart';

void main() {
  test('passkey login failures never trigger browser credential deletion', () {
    final signalService = File(
      'lib/core/services/webauthn_service.dart',
    ).readAsStringSync();
    expect(signalService, isNot(contains('maybeSignalFromApiError')));
    expect(signalService, isNot(contains('signal_unknown_credential')));

    for (final path in [
      'lib/shells/desktop/features/setup/controllers/auth_controller.dart',
      'lib/shared/widgets/reauth_overlay.dart',
    ]) {
      final source = File(path).readAsStringSync();
      expect(source, isNot(contains('maybeSignalFromApiError')));
      expect(source, isNot(contains('signalUnknownCredential')));
      expect(source, isNot(contains('signal_unknown_credential')));
    }
  });

  test(
    'browser credential deletion remains scoped to explicit passkey delete',
    () {
      final service = File(
        'lib/core/services/webauthn_service.dart',
      ).readAsStringSync();
      final passkeysController = File(
        'lib/shells/desktop/features/settings/tabs/security/'
        'passkeys_controller.dart',
      ).readAsStringSync();

      expect(service, contains('signalUnknownCredential('));
      expect(
        passkeysController,
        contains('WebAuthnService.signalUnknownCredential'),
      );
      expect(
        passkeysController,
        contains('Future<SignalDeliveryState> deletePasskey'),
      );
    },
  );
}
