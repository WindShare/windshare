import {
  V2OutputPausedError,
  type PendingFile,
} from './contract'
import {
  OutputBudgetExceededError,
  type OutputFileTransaction,
  type OutputExecutionProfile,
  type OutputSession,
} from '../output-file-contract'
import {
  bindTransferExecutionLimits,
  type TransferExecutionLimits,
  type TransferJobLimits,
} from './limits'
import {
  observePerformance,
  performanceElapsedMilliseconds,
  performanceNowMilliseconds,
  type PerformanceSummaryObservations,
} from '../../output/diagnostics/performance-summary'

const PENDING_FILE_STRUCTURAL_METADATA_BYTES = 128n
const PENDING_FILE_EXACT_SIZE_BYTES = 8n
const PENDING_FILE_TIMESTAMP_METADATA_BYTES = 24n
const PENDING_PATH_SEGMENT_METADATA_BYTES = 8n
const UTF8_ENCODER = new TextEncoder()

export interface BoundOutputScheduling {
  readonly limits: TransferExecutionLimits
  readonly resources: OutputResourceBudget
  readonly output: OutputSession
}

export function bindOutputScheduling(input: Readonly<{
  limits: TransferJobLimits
  profile: OutputExecutionProfile
  output: OutputSession
  performance?: PerformanceSummaryObservations
}>): BoundOutputScheduling {
  const limits = bindTransferExecutionLimits(input.limits, input.profile)
  const resources = new OutputResourceBudget(limits, input.performance)
  return Object.freeze({ limits, resources, output: resources.bind(input.output) })
}

export function pendingFileMetadataBytes(file: PendingFile): bigint {
  const admission = file.parent.kind === 'materialized' ? file.parent.admission : undefined
  const strings = [
    file.entry.idText,
    file.entry.name,
    ...file.sourceAuthenticationPath,
    ...file.logicalArtifactPath,
    ...file.materializationRelativePath,
    file.parent.directoryId,
    file.parent.generation,
    ...file.parent.sourceAuthenticationPath,
    ...file.parent.logicalArtifactPath,
    ...(file.parent.kind === 'materialized' ? file.parent.materializationRelativePath : []),
    ...(admission === undefined ? [] : [
      admission.token,
      ...(admission.parentToken === undefined ? [] : [admission.parentToken]),
    ]),
  ]
  let bytes = PENDING_FILE_STRUCTURAL_METADATA_BYTES +
    PENDING_FILE_EXACT_SIZE_BYTES +
    BigInt(file.entry.id.byteLength) +
    BigInt(strings.length) * PENDING_PATH_SEGMENT_METADATA_BYTES
  for (const value of strings) bytes += BigInt(UTF8_ENCODER.encode(value).byteLength)
  if (file.modifiedTime !== undefined) bytes += PENDING_FILE_TIMESTAMP_METADATA_BYTES
  if (file.parent.modifiedTime !== undefined) bytes += PENDING_FILE_TIMESTAMP_METADATA_BYTES
  return bytes
}

interface WeightedQueueItem<T> {
  readonly value: T
  readonly weight: bigint
}

/** Bounded async ownership queue shared by catalog and file schedulers. */
export class AsyncBoundedQueue<T> {
  readonly #maximumItems: number
  readonly #maximumBytes: bigint
  readonly #weight: (item: T) => bigint
  readonly #closeWhenIdle: boolean
  readonly #items: WeightedQueueItem<T>[] = []
  #bytes = 0n
  #closed = false
  #failure: unknown
  #unfinished = 0
  readonly #waiters = new Set<() => void>()

  constructor(
    maximumItems: number,
    maximumBytes: bigint,
    weight: (item: T) => bigint = () => 1n,
    closeWhenIdle = false,
  ) {
    this.#maximumItems = maximumItems
    this.#maximumBytes = maximumBytes
    this.#weight = weight
    this.#closeWhenIdle = closeWhenIdle
  }

  async push(item: T, signal: AbortSignal): Promise<void> {
    const weight = this.#weight(item)
    this.#validateWeight(weight)
    while (!this.#closed && !this.#canAdmit(weight)) await this.#wait(signal)
    signal.throwIfAborted()
    if (this.#closed) throw this.#failure ?? new V2OutputPausedError('Transfer queue is closed')
    this.#admit(item, weight)
  }

  tryPush(item: T, signal: AbortSignal): boolean {
    signal.throwIfAborted()
    if (this.#closed) throw this.#failure ?? new V2OutputPausedError('Transfer queue is closed')
    const weight = this.#weight(item)
    this.#validateWeight(weight)
    if (!this.#canAdmit(weight)) return false
    this.#admit(item, weight)
    return true
  }

  async pop(signal: AbortSignal): Promise<T | undefined> {
    while (this.#items.length === 0 && !this.#closed) await this.#wait(signal)
    signal.throwIfAborted()
    const item = this.#items.shift()
    if (item !== undefined) {
      this.#bytes -= item.weight
      this.#wake()
    }
    return item?.value
  }

  close(): void {
    this.#closed = true
    this.#wake()
  }

  taskDone(): void {
    if (!this.#closeWhenIdle || this.#unfinished === 0) return
    this.#unfinished -= 1
    if (this.#unfinished === 0 && this.#items.length === 0) this.close()
  }

  abort(reason: unknown): void {
    this.#failure = reason
    this.#closed = true
    this.#wake()
  }

  #admit(item: T, weight: bigint): void {
    this.#items.push({ value: item, weight })
    this.#bytes += weight
    if (this.#closeWhenIdle) this.#unfinished += 1
    this.#wake()
  }

  #canAdmit(weight: bigint): boolean {
    return this.#items.length < this.#maximumItems && this.#bytes + weight <= this.#maximumBytes
  }

  #validateWeight(weight: bigint): void {
    if (weight < 0n || weight > this.#maximumBytes) {
      throw new V2OutputPausedError('Transfer queue item exceeds its byte admission')
    }
  }

  async #wait(signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    await new Promise<void>((resolve, reject) => {
      const wake = () => {
        signal.removeEventListener('abort', abort)
        this.#waiters.delete(wake)
        resolve()
      }
      const abort = () => {
        signal.removeEventListener('abort', abort)
        this.#waiters.delete(wake)
        reject(signal.reason ?? new DOMException('Transfer queue aborted', 'AbortError'))
      }
      this.#waiters.add(wake)
      signal.addEventListener('abort', abort, { once: true })
      if (signal.aborted) abort()
    })
  }

  #wake(): void {
    for (const waiter of [...this.#waiters]) waiter()
  }
}

export interface OutputResourceBudgetSnapshot {
  readonly activeFiles: number
  readonly outstandingWriteBytes: bigint
  readonly bufferedBytes: bigint
  readonly peakActiveFiles: number
  readonly peakOutstandingWriteBytes: bigint
  readonly peakBufferedBytes: bigint
  readonly queuedAdmissions: number
}

interface OutputResourceRequest {
  readonly activeFiles: number
  readonly outstandingWriteBytes: bigint
  readonly bufferedBytes: bigint
  readonly signal: AbortSignal
  readonly queuedAtMilliseconds?: number
  readonly resolve: (lease: OutputResourceLease) => void
  readonly reject: (reason: unknown) => void
  readonly abort: () => void
  pending: boolean
}

interface OutputResourceLease {
  release(): void
}

/** One operation-local admission authority accounts independently bounded output resources. */
export class OutputResourceBudget {
  readonly #limits: TransferExecutionLimits
  readonly #performance: PerformanceSummaryObservations | undefined
  readonly #requests: OutputResourceRequest[] = []
  #activeFiles = 0
  #outstandingWriteBytes = 0n
  #bufferedBytes = 0n
  #peakActiveFiles = 0
  #peakOutstandingWriteBytes = 0n
  #peakBufferedBytes = 0n

  constructor(
    limits: TransferExecutionLimits,
    performance?: PerformanceSummaryObservations,
  ) {
    this.#limits = limits
    this.#performance = performance
  }

  acquireFile(signal: AbortSignal): Promise<OutputResourceLease> {
    return this.#acquire(1, 0n, 0n, signal)
  }

  runWrite<T>(bytes: bigint, signal: AbortSignal, operation: () => Promise<T>): Promise<T> {
    return this.#run(bytes, signal, operation)
  }

  bind(output: OutputSession): OutputSession {
    const scheduled: OutputSession = {
      identity: output.identity,
      capabilities: output.capabilities,
      executionProfile: output.executionProfile,
      beginFile: async (file, signal) => {
        const begun = await output.beginFile(file, signal)
        return Object.freeze({
          ...begun,
          transaction: budgetedOutputTransaction(begun.transaction, this),
        })
      },
    }
    return Object.freeze(scheduled)
  }

  snapshot(): OutputResourceBudgetSnapshot {
    return Object.freeze({
      activeFiles: this.#activeFiles,
      outstandingWriteBytes: this.#outstandingWriteBytes,
      bufferedBytes: this.#bufferedBytes,
      peakActiveFiles: this.#peakActiveFiles,
      peakOutstandingWriteBytes: this.#peakOutstandingWriteBytes,
      peakBufferedBytes: this.#peakBufferedBytes,
      queuedAdmissions: this.#requests.length,
    })
  }

  async #run<T>(bytes: bigint, signal: AbortSignal, operation: () => Promise<T>): Promise<T> {
    const lease = await this.#acquire(0, bytes, bytes, signal)
    try {
      return await operation()
    } finally {
      lease.release()
    }
  }

  #acquire(
    activeFiles: number,
    outstandingWriteBytes: bigint,
    bufferedBytes: bigint,
    signal: AbortSignal,
  ): Promise<OutputResourceLease> {
    signal.throwIfAborted()
    this.#validateRequest(activeFiles, outstandingWriteBytes, bufferedBytes)
    const queuedAtMilliseconds = performanceNowMilliseconds(this.#performance)
    return new Promise<OutputResourceLease>((resolve, reject) => {
      const request: OutputResourceRequest = {
        activeFiles,
        outstandingWriteBytes,
        bufferedBytes,
        signal,
        ...(queuedAtMilliseconds === undefined ? {} : { queuedAtMilliseconds }),
        resolve,
        reject,
        abort: () => {
          if (!request.pending) return
          request.pending = false
          const index = this.#requests.indexOf(request)
          if (index >= 0) this.#requests.splice(index, 1)
          reject(signal.reason ?? new DOMException('Output resource admission aborted', 'AbortError'))
          this.#pump()
        },
        pending: true,
      }
      this.#requests.push(request)
      signal.addEventListener('abort', request.abort, { once: true })
      if (signal.aborted) request.abort()
      else this.#pump()
    })
  }

  #validateRequest(activeFiles: number, outstandingWriteBytes: bigint, bufferedBytes: bigint): void {
    if (activeFiles > this.#limits.concurrentFiles) {
      throw new OutputBudgetExceededError(
        'active files',
        BigInt(this.#limits.concurrentFiles),
        BigInt(activeFiles),
      )
    }
    if (outstandingWriteBytes < 0n ||
        outstandingWriteBytes > this.#limits.maximumOutstandingWriteBytes) {
      throw new OutputBudgetExceededError(
        'outstanding write bytes',
        this.#limits.maximumOutstandingWriteBytes,
        outstandingWriteBytes,
      )
    }
    if (bufferedBytes < 0n || bufferedBytes > this.#limits.maximumBufferedBytes) {
      throw new OutputBudgetExceededError(
        'buffered bytes',
        this.#limits.maximumBufferedBytes,
        bufferedBytes,
      )
    }
  }

  #pump(): void {
    while (true) {
      const index = this.#requests.findIndex(request => this.#canAdmit(request))
      if (index < 0) return
      const [request] = this.#requests.splice(index, 1)
      if (request === undefined) throw new Error('output resource admission queue was corrupted')
      request.pending = false
      request.signal.removeEventListener('abort', request.abort)
      this.#activeFiles += request.activeFiles
      this.#outstandingWriteBytes += request.outstandingWriteBytes
      this.#bufferedBytes += request.bufferedBytes
      this.#peakActiveFiles = Math.max(this.#peakActiveFiles, this.#activeFiles)
      this.#peakOutstandingWriteBytes = maxBigInt(
        this.#peakOutstandingWriteBytes,
        this.#outstandingWriteBytes,
      )
      this.#peakBufferedBytes = maxBigInt(this.#peakBufferedBytes, this.#bufferedBytes)
      this.#observeAdmission(request)
      request.resolve(this.#lease(request))
    }
  }

  #canAdmit(request: OutputResourceRequest): boolean {
    return this.#activeFiles + request.activeFiles <= this.#limits.concurrentFiles &&
      this.#outstandingWriteBytes + request.outstandingWriteBytes <=
        this.#limits.maximumOutstandingWriteBytes &&
      this.#bufferedBytes + request.bufferedBytes <= this.#limits.maximumBufferedBytes
  }

  #lease(request: OutputResourceRequest): OutputResourceLease {
    let released = false
    return Object.freeze({
      release: () => {
        if (released) return
        released = true
        this.#activeFiles -= request.activeFiles
        this.#outstandingWriteBytes -= request.outstandingWriteBytes
        this.#bufferedBytes -= request.bufferedBytes
        if (this.#activeFiles < 0 || this.#outstandingWriteBytes < 0n || this.#bufferedBytes < 0n) {
          throw new Error('output resource accounting underflowed')
        }
        this.#pump()
      },
    })
  }

  #observeAdmission(request: OutputResourceRequest): void {
    const waitMilliseconds = performanceElapsedMilliseconds(
      request.queuedAtMilliseconds,
      performanceNowMilliseconds(this.#performance),
    )
    if (waitMilliseconds === undefined) return
    observePerformance(this.#performance, summary => {
      if (request.activeFiles > 0) {
        summary.observeOutputResource({
          resource: 'active_files',
          waitMilliseconds,
          peak: this.#peakActiveFiles,
        })
      }
      if (request.outstandingWriteBytes > 0n) {
        summary.observeOutputResource({
          resource: 'write_bytes',
          waitMilliseconds,
          peak: this.#peakOutstandingWriteBytes,
        })
      }
      if (request.bufferedBytes > 0n) {
        summary.observeOutputResource({
          resource: 'buffered_bytes',
          waitMilliseconds,
          peak: this.#peakBufferedBytes,
        })
      }
    })
  }
}

function budgetedOutputTransaction(
  transaction: OutputFileTransaction,
  budget: OutputResourceBudget,
): OutputFileTransaction {
  const budgeted: OutputFileTransaction = {
    writeRange: (offset, data, signal) => budget.runWrite(
      BigInt(data.byteLength),
      signal,
      () => transaction.writeRange(offset, data, signal),
    ),
    automaticCheckpoint: (trigger, costBudget, signal) =>
      transaction.automaticCheckpoint(trigger, costBudget, signal),
    commit: signal => transaction.commit(signal),
    retire: reason => transaction.retire(reason),
    pause: reason => transaction.pause(reason),
  }
  return Object.freeze(budgeted)
}

function maxBigInt(left: bigint, right: bigint): bigint {
  return left >= right ? left : right
}
