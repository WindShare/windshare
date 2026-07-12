export function combineSinkCleanupFailure(
  reason: unknown,
  cleanupError: unknown,
  message: string,
): AggregateError {
  return new AggregateError([reason, cleanupError], message, { cause: reason })
}
