export async function withFailedRetainedClose(
  operation: { close(): Promise<void> },
  error: unknown,
): Promise<never> {
  return withRetainedOperationClose(operation, async () => {
    throw error
  })
}

export async function detachRuntimeAfterFailure(
  runtime: { detach(): void | PromiseLike<void> },
  error: unknown,
): Promise<never> {
  let cleanupFailure: unknown
  try {
    await Promise.resolve(runtime.detach())
  } catch (caughtCleanupFailure) {
    cleanupFailure = caughtCleanupFailure
  }
  if (cleanupFailure !== undefined) {
    throw new AggregateError(
      [error, cleanupFailure],
      'Receive continuation adoption failed and runtime cleanup also failed',
      { cause: error },
    )
  }
  throw error
}

export async function withRetainedOperationClose<Result>(
  operation: { close(): Promise<void> },
  execute: () => Promise<Result>,
): Promise<Result> {
  let failed = false
  let failure: unknown
  let result: Result | undefined
  try {
    result = await execute()
  } catch (error) {
    failed = true
    failure = error
  }
  let cleanupFailed = false
  let cleanupFailure: unknown
  try {
    await operation.close()
  } catch (error) {
    cleanupFailed = true
    cleanupFailure = error
  }
  if (failed && cleanupFailed) {
    throw new AggregateError(
      [failure, cleanupFailure],
      'Retained operation and output cleanup both failed',
      { cause: failure },
    )
  }
  if (failed) throw failure
  if (cleanupFailed) throw cleanupFailure
  return result!
}
