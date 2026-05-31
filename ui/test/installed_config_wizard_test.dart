import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/features/apps/installed_config_wizard.dart';

void main() {
  InstalledConfigField field(
    String type, {
    bool required = false,
    bool present = true,
  }) => InstalledConfigField(
    name: type,
    type: type,
    required: required,
    generate: false,
    sensitive: false,
    present: present,
    provenance: 'operator',
    editable: true,
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
}
