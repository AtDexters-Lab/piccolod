import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/core/models/capability_models.dart';

void main() {
  test('parses capability inventory and resolves an app provider', () {
    final status = CapabilityStatus.fromJson(const {
      'capability': 'ai.inference.openai.v1',
      'default': 'provider-one',
      'provider_change_disclosure': 'Requests may be interrupted.',
      'providers': [
        {'app_instance': 'provider-one', 'enabled': true},
        {'app_instance': 'provider-two', 'enabled': false},
      ],
    });

    expect(status.capability, 'ai.inference.openai.v1');
    expect(status.defaultProvider, 'provider-one');
    expect(status.providerChangeDisclosure, 'Requests may be interrupted.');
    expect(status.providerFor('provider-one')?.enabled, isTrue);
    expect(status.providerFor('provider-two')?.enabled, isFalse);
    expect(status.providerFor('missing'), isNull);
    expect(status.isDefault('provider-one'), isTrue);
    expect(status.isDefault('provider-two'), isFalse);
  });

  test('maps capability-selection response statuses', () {
    expect(
      CapabilityProviderSelectionOutcome.fromHttpStatus(202),
      CapabilityProviderSelectionOutcome.repairPending,
    );
    expect(
      CapabilityProviderSelectionOutcome.fromHttpStatus(204),
      CapabilityProviderSelectionOutcome.reconciled,
    );
  });
}
