import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/shared/widgets/log_stream_viewer.dart';

void main() {
  test('buildAppLogStreamPath includes service when set', () {
    expect(
      buildAppLogStreamPath(appId: 'demo', tail: 200, serviceName: 'db'),
      '/api/v1/apps/demo/logs/stream?tail=200&timestamps=1&service=db',
    );
  });

  test('buildAppLogStreamPath omits service when blank', () {
    expect(
      buildAppLogStreamPath(appId: 'demo', tail: 200, serviceName: ''),
      '/api/v1/apps/demo/logs/stream?tail=200&timestamps=1',
    );
  });
}
