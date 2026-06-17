import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/core/models/app_models.dart';

void main() {
  group('App catalog pending flow', () {
    test('parses config pending flow', () {
      final app = App.fromJson(const {
        'instance_id': 'piclu',
        'status': 'running',
        'catalog_update_pending': true,
        'catalog_update_pending_flow': 'config',
        'definition': {
          'x-piccolo': {'mode': 'service'},
        },
      });

      expect(app.catalogUpdatePending, isTrue);
      expect(app.catalogUpdatePendingFlow, 'config');
      expect(app.hasConfigReviewCatalogUpdate, isTrue);
      expect(app.hasManifestReviewCatalogUpdate, isFalse);
    });

    test('parses manifest-review pending flow', () {
      final app = App.fromJson(const {
        'instance_id': 'piclu',
        'status': 'running',
        'catalog_update_pending': true,
        'catalog_update_pending_flow': 'manifest_review',
        'definition': {
          'x-piccolo': {'mode': 'service'},
        },
      });

      expect(app.catalogUpdatePending, isTrue);
      expect(app.catalogUpdatePendingFlow, 'manifest_review');
      expect(app.hasManifestReviewCatalogUpdate, isTrue);
      expect(app.hasConfigReviewCatalogUpdate, isFalse);
    });
  });

  test('manifest update parses kept value review items', () {
    final result = ManifestUpdateResult.fromJson(const <String, dynamic>{
      'instance_id': 'piclu',
      'base_manifest_hash': 'base',
      'runtime_fingerprint': 'runtime',
      'rendered_app_id': 'piclu',
      'diff_kind': 'structural',
      'applicable': true,
      'metadata_only': false,
      'summary': <String, dynamic>{},
      'kept_value_review': <Map<String, dynamic>>[
        <String, dynamic>{
          'field': 'gemini_api_key',
          'risk_kind': 'kept_secret_semantic_changed',
          'old_semantic': ['label=Gemini API key'],
          'new_semantic': ['label=OpenAI compatible API key'],
          'semantic_delta': ['label changed', 'template usage changed'],
          'old_usage': ['line 20: PICLU_API_KEY'],
          'new_usage': ['line 20: OPENAI_COMPATIBLE_API_KEY'],
          'confirmation': 'kept_value_review:gemini_api_key',
          'blocking_reason': 'confirm current stored value reuse',
        },
      ],
      'required_confirmations': ['kept_value_review:gemini_api_key'],
    });

    expect(result.keptValueReview, hasLength(1));
    final item = result.keptValueReview.single;
    expect(item.field, 'gemini_api_key');
    expect(item.riskKind, 'kept_secret_semantic_changed');
    expect(item.semanticDelta, contains('template usage changed'));
    expect(item.confirmation, 'kept_value_review:gemini_api_key');
    expect(result.requiredConfirmations, contains(item.confirmation));
  });

  test('manifest update input fields parse current-value metadata', () {
    final configure = ManifestUpdateConfigureResult.fromJson(const {
      'eligible': true,
      'inputs': <String, dynamic>{},
      'fields': <Map<String, dynamic>>[
        <String, dynamic>{
          'name': 'session',
          'type': 'string',
          'provenance': 'Current stored value will be kept',
          'required': true,
          'generate': false,
          'locked': false,
          'sensitive': true,
          'has_current_value': true,
          'current_value_sensitive': true,
          'current_value_display': '',
        },
      ],
    });

    expect(configure.fields, hasLength(1));
    expect(configure.fields.single.sensitive, isTrue);
    expect(configure.fields.single.hasCurrentValue, isTrue);
    expect(configure.fields.single.currentValueSensitive, isTrue);
    expect(configure.fields.single.currentValueDisplay, isEmpty);
  });
}
