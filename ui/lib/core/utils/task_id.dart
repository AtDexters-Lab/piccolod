import 'dart:math';

final _taskIdRng = Random.secure();

String generateTaskId() {
  const chars = 'abcdefghijklmnopqrstuvwxyz0123456789';
  final buf = StringBuffer();
  for (var i = 0; i < 24; i++) {
    buf.write(chars[_taskIdRng.nextInt(chars.length)]);
  }
  return buf.toString();
}

