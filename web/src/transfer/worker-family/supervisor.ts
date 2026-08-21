export interface WorkerFamilyQueue {
  close(): void
  abort(reason: unknown): void
}

export type WorkerFamilyFailureSource =
  | Readonly<{ kind: 'producer' }>
  | Readonly<{ kind: 'worker'; workerIndex: number }>
  | Readonly<{ kind: 'abort' }>
  | Readonly<{ kind: 'queue-close'; queueIndex: number }>
  | Readonly<{ kind: 'queue-abort'; queueIndex: number }>

export interface WorkerFamilyConsequenceFailure {
  readonly initiatingFailure: unknown
  readonly failure: unknown
  readonly source: WorkerFamilyFailureSource
}

export interface WorkerFamilySupervision {
  readonly producer: Promise<void>
  readonly workers: readonly Promise<void>[]
  readonly queues: readonly WorkerFamilyQueue[]
  readonly abort: (failure: unknown) => void
  readonly observeConsequenceFailure?: (failure: WorkerFamilyConsequenceFailure) => void
}

/**
 * Owns the terminal boundary for a producer and its workers. Promise aggregates
 * are intentionally excluded because their early rejection does not prove that
 * sibling workers have stopped mutating output.
 */
export async function superviseWorkerFamily(input: WorkerFamilySupervision): Promise<void> {
  const queues = [...new Set(input.queues)]
  let failureLatched = false
  let initiatingFailure: unknown
  let abortStarted = false

  const reportConsequence = (failure: unknown, source: WorkerFamilyFailureSource): void => {
    try {
      input.observeConsequenceFailure?.(Object.freeze({
        initiatingFailure,
        failure,
        source,
      }))
    } catch {
      // Diagnostic observers cannot alter failure precedence or prevent drainage.
    }
  }

  const invokeAbort = (
    action: () => void,
    source: WorkerFamilyFailureSource,
  ): void => {
    try {
      action()
    } catch (failure) {
      reportConsequence(failure, source)
    }
  }

  const abortOnce = (failure: unknown): void => {
    if (abortStarted) return
    abortStarted = true
    invokeAbort(() => input.abort(failure), Object.freeze({ kind: 'abort' }))
    queues.forEach((queue, queueIndex) => {
      invokeAbort(
        () => queue.abort(failure),
        Object.freeze({ kind: 'queue-abort', queueIndex }),
      )
    })
  }

  const observeFailure = (failure: unknown, source: WorkerFamilyFailureSource): void => {
    if (failureLatched) {
      reportConsequence(failure, source)
      return
    }
    failureLatched = true
    initiatingFailure = failure
    abortOnce(failure)
  }

  const closeQueues = (): void => {
    if (failureLatched) return
    for (const [queueIndex, queue] of queues.entries()) {
      try {
        queue.close()
      } catch (failure) {
        observeFailure(failure, Object.freeze({ kind: 'queue-close', queueIndex }))
        return
      }
    }
  }

  // Attach rejection observers before installing the allSettled drain so the
  // first terminal object is latched before any consequence can replace it.
  const producerObservation = input.producer.then(
    closeQueues,
    failure => observeFailure(failure, Object.freeze({ kind: 'producer' })),
  )
  const workerObservations = input.workers.map((worker, workerIndex) =>
    worker.catch(failure => {
      observeFailure(failure, Object.freeze({ kind: 'worker', workerIndex }))
    }),
  )

  await Promise.allSettled([input.producer, ...input.workers])
  await Promise.allSettled([producerObservation, ...workerObservations])
  if (failureLatched) throw initiatingFailure
}
