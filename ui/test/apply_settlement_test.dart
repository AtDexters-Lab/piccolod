import 'package:flutter_test/flutter_test.dart';
import 'package:piccolo_os/features/apps/apply_settlement.dart';

void main() {
  test('terminal task success settles ambiguous apply POST failure', () {
    expect(
      shouldFinishApplyFromTaskSuccess(
        taskSucceeded: true,
        alreadyApplied: false,
      ),
      isTrue,
    );
    expect(
      shouldFinishApplyFromTaskSuccess(
        taskSucceeded: false,
        alreadyApplied: false,
      ),
      isFalse,
    );
    expect(
      shouldFinishApplyFromTaskSuccess(
        taskSucceeded: true,
        alreadyApplied: true,
      ),
      isFalse,
    );
  });
}
