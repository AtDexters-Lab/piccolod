import 'dart:async';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:piccolo_os/core/models/task_progress.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/features/apps/apply_settlement.dart';

void main() {
  test('only transport loss or timeout leaves an unknown apply outcome', () {
    expect(
      isAmbiguousApplyResponseError(http.ClientException('closed')),
      isTrue,
    );
    expect(isAmbiguousApplyResponseError(TimeoutException('timeout')), isTrue);
    expect(
      isAmbiguousApplyResponseError(ApiException(500, 'rejected')),
      isFalse,
    );
    expect(isAmbiguousApplyResponseError(StateError('unexpected')), isFalse);
  });

  test('repair is structured and cannot be mistaken for ordinary success', () {
    TaskProgressEvent event(Map<String, dynamic>? metadata) =>
        TaskProgressEvent(
          taskId: 'original',
          taskType: 'update_config',
          phase: 'complete',
          progress: 100,
          message: 'complete',
          isComplete: true,
          metadata: metadata,
        );
    expect(applyAccessRepairMessage(event(null), 'repair needed'), isNull);
    expect(
      applyAccessRepairMessage(
        event({'access_repair_pending': true}),
        'repair needed',
      ),
      'repair needed',
    );
    expect(
      applyAccessRepairMessage(
        event({
          'access_repair_pending': true,
          'access_repair_message': 'Publication pending',
        }),
        'repair needed',
      ),
      'Publication pending',
    );
  });
}
