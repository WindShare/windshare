import {
  COMPLETED_JOB_SETTLEMENT,
  type DurabilityLevel,
  type JobSettlement,
  needsAttentionJobSettlement,
  pausedJobSettlement,
} from '../../transfer/output-session'
import {
  BoundaryFaultError,
  FaultScope,
  OutputFaultCode,
  faultRequiresAttention,
  outputFault,
} from '../../transfer/fault'
import { PersistentOutputError } from './errors'

interface Deferred {
  readonly promise: Promise<void>
  readonly resolve: () => void
}

interface PersistentLifecycleFile {
  pauseForClose(reason: unknown): Promise<void>
}

export class PersistentOperationLease {
  readonly closing: AbortSignal
  readonly #releaseOperation: () => void
  #released = false

  constructor(closing: AbortSignal, releaseOperation: () => void) {
    this.closing = closing
    this.#releaseOperation = releaseOperation
  }

  release(): void {
    if (this.#released) return
    this.#released = true
    this.#releaseOperation()
  }
}

/**
 * Owns the stable lifecycle cut without serializing ordinary I/O. Acquisition
 * is synchronous so every accepted operation is inside the drain before its
 * caller can initiate Pause or Complete.
 */
export class PersistentOutputLifecycle {
  readonly #closing = new AbortController()
  readonly #drained = deferred()
  readonly #durability: DurabilityLevel
  readonly #activeFiles: ReadonlyMap<string, PersistentLifecycleFile>
  readonly #openingFiles: ReadonlySet<string>

  #gateState: 'open' | 'closing' | 'closed' = 'open'
  #sessionState: 'open' | 'finished' | 'paused' = 'open'
  #activeOperations = 0
  #closePromise: Promise<JobSettlement> | undefined

  constructor(
    durability: DurabilityLevel,
    activeFiles: ReadonlyMap<string, PersistentLifecycleFile>,
    openingFiles: ReadonlySet<string>,
  ) {
    this.#durability = durability
    this.#activeFiles = activeFiles
    this.#openingFiles = openingFiles
  }

  complete(signal: AbortSignal): Promise<JobSettlement> {
    const settled = this.#settledJob()
    if (settled !== undefined) return Promise.resolve(settled)
    if (this.#closePromise !== undefined) return this.#closePromise
    try {
      signal.throwIfAborted()
    } catch (error) {
      return Promise.reject(error)
    }

    const operation = this.#completeAfterDrain(this.#requestClose(), signal)
    this.#closePromise = operation
    return operation
  }

  pause(reason: unknown): Promise<JobSettlement> {
    const settled = this.#settledJob()
    if (settled !== undefined) return Promise.resolve(settled)
    if (this.#closePromise !== undefined) return this.#closePromise

    const operation = this.#pauseAfterDrain(this.#requestClose(), reason)
    this.#closePromise = operation
    return operation
  }

  run<T>(operation: (lease: PersistentOperationLease) => T | Promise<T>): Promise<T> {
    let lease: PersistentOperationLease
    try {
      lease = this.acquire()
    } catch (error) {
      return Promise.reject(error)
    }

    let result: T | Promise<T>
    try {
      result = operation(lease)
    } catch (error) {
      lease.release()
      return Promise.reject(error)
    }
    return Promise.resolve(result).finally(() => lease.release())
  }

  acquire(): PersistentOperationLease {
    if (this.#gateState !== 'open') throw lifecycleClosedError()
    this.#activeOperations += 1
    return new PersistentOperationLease(this.#closing.signal, () => this.#releaseOperation())
  }

  requireSessionOpen(): void {
    if (this.#sessionState !== 'open') {
      throw new PersistentOutputError('output-state', 'Persistent output session is not open')
    }
  }

  async #completeAfterDrain(
    drained: Promise<void>,
    signal: AbortSignal,
  ): Promise<JobSettlement> {
    await drained
    const activeFiles = [...this.#activeFiles.values()]
    if (signal.aborted) {
      await this.#pauseActiveFiles(activeFiles, signal.reason)
      this.#finishClose('paused')
      throw signal.reason ?? new DOMException('Output completion aborted', 'AbortError')
    }
    if (activeFiles.length !== 0 || this.#openingFiles.size !== 0) {
      const reason = new PersistentOutputError(
        'output-state',
        'Cannot complete output while file transactions are active',
      )
      await this.#pauseActiveFiles(activeFiles, reason)
      this.#finishClose('paused')
      throw reason
    }

    this.#finishClose('finished')
    return COMPLETED_JOB_SETTLEMENT
  }

  async #pauseAfterDrain(drained: Promise<void>, reason: unknown): Promise<JobSettlement> {
    await drained
    const settlement = await this.#pauseActiveFiles([...this.#activeFiles.values()], reason)
    this.#finishClose('paused')
    if (reason instanceof BoundaryFaultError && faultRequiresAttention(reason.fault)) {
      return needsAttentionJobSettlement(reason.fault)
    }
    return settlement
  }

  async #pauseActiveFiles(
    activeFiles: readonly PersistentLifecycleFile[],
    reason: unknown,
  ): Promise<JobSettlement> {
    const failures: unknown[] = []
    for (const transaction of activeFiles) {
      try {
        await transaction.pauseForClose(reason)
      } catch (error) {
        failures.push(error)
      }
    }
    if (failures.length > 0) {
      return needsAttentionJobSettlement(outputFault(
        FaultScope.OutputPause,
        OutputFaultCode.MutationAmbiguous,
      ))
    }
    return pausedJobSettlement(this.#durability)
  }

  #requestClose(): Promise<void> {
    if (this.#gateState !== 'open') return this.#drained.promise

    this.#gateState = 'closing'
    this.#closing.abort(lifecycleClosedError())
    if (this.#activeOperations === 0) this.#drained.resolve()
    return this.#drained.promise
  }

  #finishClose(state: 'finished' | 'paused'): void {
    if (this.#gateState !== 'closing' || this.#activeOperations !== 0) {
      throw new Error('Persistent output lifecycle reached an invalid close transition')
    }
    this.#sessionState = state
    this.#gateState = 'closed'
  }

  #settledJob(): JobSettlement | undefined {
    if (this.#sessionState === 'finished') return COMPLETED_JOB_SETTLEMENT
    if (this.#sessionState === 'paused') return pausedJobSettlement(this.#durability)
    return undefined
  }

  #releaseOperation(): void {
    if (this.#activeOperations <= 0) {
      throw new Error('Persistent output lifecycle released an unknown operation')
    }
    this.#activeOperations -= 1
    if (this.#gateState === 'closing' && this.#activeOperations === 0) this.#drained.resolve()
  }
}

function deferred(): Deferred {
  let resolve = (): void => undefined
  const promise = new Promise<void>((complete) => { resolve = complete })
  return { promise, resolve }
}

function lifecycleClosedError(): PersistentOutputError {
  return new PersistentOutputError(
    'output-state',
    'Persistent output lifecycle is closing or closed',
  )
}
