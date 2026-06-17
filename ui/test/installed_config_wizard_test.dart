import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/features/apps/installed_config_wizard.dart';

void main() {
  InstalledConfigField field(
    String type, {
    bool required = false,
    bool present = true,
    bool sensitive = false,
    bool generate = false,
    Object? display,
    List<String> actions = const [],
  }) => InstalledConfigField(
    name: type,
    type: type,
    required: required,
    generate: generate,
    sensitive: sensitive,
    present: present,
    display: display,
    provenance: 'operator',
    editable: true,
    actions: actions,
  );

  test('installed config numeric edits preserve invalid text', () {
    expect(installedConfigInputValueForField(field('int'), '42'), 42);
    expect(installedConfigInputValueForField(field('int'), 'abc'), 'abc');
    expect(installedConfigInputValueForField(field('number'), '2.5'), 2.5);
    expect(installedConfigInputValueForField(field('number'), 'abc'), 'abc');
  });

  test('required absent boolean is submitted as an explicit false', () {
    expect(
      installedConfigShouldSubmitPlainField(
        field('boolean', required: true, present: false),
        changed: false,
      ),
      isTrue,
    );
    expect(
      installedConfigShouldSubmitPlainField(field('boolean'), changed: false),
      isFalse,
    );
    expect(
      installedConfigShouldSubmitPlainField(field('string'), changed: true),
      isTrue,
    );
  });

  test('required absent defaulted field is submitted unchanged', () {
    expect(
      installedConfigShouldSubmitPlainField(
        field('string', required: true, present: false, display: 'local'),
        changed: false,
      ),
      isTrue,
    );
    expect(
      installedConfigShouldSubmitPlainField(
        field('string', required: true, present: false),
        changed: false,
      ),
      isFalse,
    );
  });

  test('absent sensitive config actions do not imply hidden current value', () {
    final absentSecret = field('string', present: false, sensitive: true);
    expect(installedConfigActionLabel('replace', absentSecret), 'Set value');
    expect(installedConfigActionLabel('keep', absentSecret), 'Leave unset');

    final presentSecret = field('string', sensitive: true);
    expect(
      installedConfigActionLabel('replace', presentSecret),
      'Replace value',
    );
    expect(installedConfigActionLabel('clear', presentSecret), 'Clear value');
  });

  test(
    'absent optional sensitive config action defaults to leave unset',
    () {
      final absentSecret = field(
        'string',
        present: false,
        sensitive: true,
        actions: const ['keep', 'replace'],
      );
      expect(installedConfigEffectiveSecretAction(absentSecret, null), 'keep');
      expect(
        installedConfigEffectiveSecretAction(absentSecret, 'replace'),
        'replace',
      );

      final requiredAbsentSecret = field(
        'string',
        required: true,
        present: false,
        sensitive: true,
        actions: const ['replace'],
      );
      expect(
        installedConfigEffectiveSecretAction(requiredAbsentSecret, null),
        'replace',
      );

      final presentSecret = field(
        'string',
        sensitive: true,
        actions: const ['keep', 'replace', 'clear'],
      );
      expect(installedConfigEffectiveSecretAction(presentSecret, null), 'keep');
      expect(
        installedConfigEffectiveSecretAction(presentSecret, 'replace'),
        'replace',
      );
    },
  );
}
