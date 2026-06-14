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
}
