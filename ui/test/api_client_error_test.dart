import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/core/services/api_client.dart';

void main() {
  test('ApiException keeps top-level structured error fields in rawBody', () {
    const body =
        '{"error":"setup already in progress","code":"setup_in_progress"}';
    final error = ApiException(409, body);
    final decoded = jsonDecode(error.rawBody) as Map<String, dynamic>;

    expect(error.message, 'setup already in progress');
    expect(error.code, 'setup_in_progress');
    expect(error.retryable, isNull);
    expect(decoded['code'], 'setup_in_progress');
  });

  test('ApiException exposes nested error key and readable message', () {
    const body =
        '{"error":{"message":"Another update is still finishing","key":"transition_in_progress"}}';
    final error = ApiException(409, body);

    expect(error.message, 'Another update is still finishing');
    expect(error.key, 'transition_in_progress');
  });

  test('ApiException preserves typed retryable task pressure fields', () {
    const body =
        '{"error":"Piccolo is temporarily limiting new process work","code":"task_pressure","retryable":true}';
    final error = ApiException(503, body);

    expect(error.code, 'task_pressure');
    expect(error.retryable, isTrue);
    expect(error.isRetryableTaskPressure, isTrue);
  });

  test('task-pressure marker without retryability is not retryable', () {
    const body =
        '{"error":"busy","code":"task_pressure","retryable":false}';
    final error = ApiException(503, body);

    expect(error.code, 'task_pressure');
    expect(error.retryable, isFalse);
    expect(error.isRetryableTaskPressure, isFalse);
  });

  test('ApiException retains top-level metadata around a nested error', () {
    const body =
        '{"error":{"message":"Try later","key":"busy"},"code":"task_pressure","retryable":true}';
    final error = ApiException(503, body);

    expect(error.message, 'Try later');
    expect(error.key, 'busy');
    expect(error.code, 'task_pressure');
    expect(error.retryable, isTrue);
  });
}
