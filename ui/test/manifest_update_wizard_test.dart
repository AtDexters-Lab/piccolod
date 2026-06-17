import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/features/apps/manifest_update_wizard.dart';

void main() {
  ManifestUpdateInputField field({
    bool required = false,
    String type = 'string',
  }) => ManifestUpdateInputField(
    name: 'field',
    type: type,
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
        field(required: true),
        touched: false,
        hasDefault: false,
      ),
      isFalse,
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

  test('required manifest booleans are submitted when untouched', () {
    expect(
      manifestUpdateShouldSubmitField(
        field(required: true, type: 'boolean'),
        touched: false,
        hasDefault: false,
      ),
      isTrue,
    );
    expect(
      manifestUpdateShouldSubmitField(
        field(type: 'boolean'),
        touched: false,
        hasDefault: false,
      ),
      isFalse,
    );
  });

  test('kept value review copy distinguishes review from blocked action', () {
    final reviewText = manifestUpdateKeptValueReviewText(
      const ManifestUpdateKeptValueReviewItem(
        field: 'license',
        riskKind: 'kept_secret_semantic_changed',
        semanticDelta: ['label changed'],
        oldUsage: ['line 1'],
        newUsage: ['line 1'],
        confirmation: 'kept_value_review:license',
      ),
    );
    expect(
      reviewText,
      contains('current stored value will be kept after review'),
    );
    expect(
      reviewText,
      isNot(contains('replace or regenerate before applying')),
    );

    final blockedText = manifestUpdateKeptValueReviewText(
      const ManifestUpdateKeptValueReviewItem(
        field: 'license',
        riskKind: 'kept_secret_semantic_changed',
        semanticDelta: ['previous source usage unavailable'],
        confirmation: 'kept_value_review:license',
        blockingReason: 'template usage unavailable',
      ),
    );
    expect(blockedText, contains('replace or regenerate before applying'));
    expect(blockedText, contains('template usage unavailable'));
  });
}
