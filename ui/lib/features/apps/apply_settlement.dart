bool shouldFinishApplyFromTaskSuccess({
  required bool taskSucceeded,
  required bool alreadyApplied,
}) {
  return taskSucceeded && !alreadyApplied;
}
