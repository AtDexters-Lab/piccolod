import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/features/apps/manifest_update_wizard.dart';

void main() {
  ManifestUpdateInputField field({bool required = false}) =>
      ManifestUpdateInputField(
        name: 'field',
        type: 'string',
        provenance: 'New manifest default',
        required: required,
        generate: false,
        locked: false,
      );

  test('required defaulted manifest fields are submitted when untouched', () {
    expect(
      manifestUpdateShouldSubmitField(
        field(required: true),
        touched: false,
        hasDefault: true,
      ),
      isTrue,
    );
    expect(
      manifestUpdateShouldSubmitField(
        field(),
        touched: false,
        hasDefault: true,
      ),
      isFalse,
    );
    expect(
      manifestUpdateShouldSubmitField(
        field(),
        touched: true,
        hasDefault: true,
      ),
      isTrue,
    );
  });
}
