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
    expect(decoded['code'], 'setup_in_progress');
  });

  test('ApiException exposes nested error key and readable message', () {
    const body =
        '{"error":{"message":"Another update is still finishing","key":"transition_in_progress"}}';
    final error = ApiException(409, body);

    expect(error.message, 'Another update is still finishing');
    expect(error.key, 'transition_in_progress');
  });
}
