import type { FileCheckpointV2 } from '../persistence/checkpoint'
import type {
  FileCheckpointJournal,
  FileCheckpointScan,
} from '../persistence/journal'
import type {
  PersistentOutputCheckpointRecordFact,
  PersistentOutputFactTruncation,
  PersistentOutputFailureFactContext,
  PersistentOutputFailureFacts,
  PersistentOutputFailureObservation,
  PersistentOutputObservedFact,
  PersistentOutputRawException,
} from './stage-diagnostic-model'

export const PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS = Object.freeze({
  deadlineMilliseconds: 100,
  checkpointPages: 2,
  checkpointRecords: 16,
  stringBytes: 512,
  totalBytes: 16 * 1024,
})

const CHECKPOINT_RECORD_FIXED_BYTES = 64

export class PersistentOutputFailureObservationBudget
implements PersistentOutputFailureFactContext {
  readonly #controller = new AbortController()
  readonly #truncation = new Set<PersistentOutputFactTruncation>()
  #checkpointPagesRead = 0
  #checkpointRecordsRetained = 0
  #retainedBytes = 0
  #timedOut = false

  get signal(): AbortSignal {
    return this.#controller.signal
  }

  exception(error: unknown): PersistentOutputRawException {
    return snapshotException(error, value => this.#boundedString(value))
  }

  claimCheckpointPage(): boolean {
    if (this.#checkpointPagesRead >= PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.checkpointPages) {
      this.markTruncated('checkpoint-pages')
      return false
    }
    this.#checkpointPagesRead += 1
    return true
  }

  remainingCheckpointRecords(): number {
    return Math.max(
      0,
      PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.checkpointRecords -
        this.#checkpointRecordsRetained,
    )
  }

  checkpointRecord(record: FileCheckpointV2): PersistentOutputCheckpointRecordFact | undefined {
    if (this.remainingCheckpointRecords() === 0) {
      this.markTruncated('checkpoint-records')
      return undefined
    }
    if (!this.#reserveBytes(CHECKPOINT_RECORD_FIXED_BYTES)) return undefined
    this.#checkpointRecordsRetained += 1
    return Object.freeze({
      recordId: this.#boundedString(record.recordId),
      checkpointGeneration: record.checkpointGeneration,
      commitState: record.commitState,
      checksum: this.#boundedString(record.checksum),
      verifiedEnd: record.verifiedRanges.at(-1)?.end ?? 0n,
    })
  }

  markTruncated(reason: PersistentOutputFactTruncation): void {
    this.#truncation.add(reason)
  }

  timeout(): void {
    this.#timedOut = true
    this.#controller.abort(new DOMException(
      'Persistent output diagnostic fact deadline elapsed',
      'TimeoutError',
    ))
  }

  observation(
    providerCount: number,
    completedProviderCount: number,
    activeFileEvidenceCount: number,
    unavailableProviders: PersistentOutputFailureObservation['unavailableProviders'],
  ): PersistentOutputFailureObservation {
    return Object.freeze({
      deadlineMilliseconds: PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.deadlineMilliseconds,
      timedOut: this.#timedOut,
      providerCount,
      completedProviderCount,
      activeFileEvidenceCount,
      checkpointPagesRead: this.#checkpointPagesRead,
      checkpointRecordsRetained: this.#checkpointRecordsRetained,
      retainedBytes: this.#retainedBytes,
      truncation: Object.freeze([...this.#truncation].sort()),
      unavailableProviders: Object.freeze([...unavailableProviders]),
    })
  }

  #boundedString(value: string): string {
    const perString = truncateUTF8(value, PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.stringBytes)
    if (perString.truncated) this.markTruncated('string-bytes')
    const remaining = PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.totalBytes - this.#retainedBytes
    const total = truncateUTF8(perString.value, remaining)
    if (total.truncated) this.markTruncated('total-bytes')
    this.#retainedBytes += total.bytes
    return total.value
  }

  #reserveBytes(bytes: number): boolean {
    if (this.#retainedBytes + bytes > PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.totalBytes) {
      this.markTruncated('total-bytes')
      return false
    }
    this.#retainedBytes += bytes
    return true
  }
}

export async function persistentOutputObservedFact<Value>(
  observe: () => Promise<Value>,
  context?: PersistentOutputFailureFactContext,
): Promise<PersistentOutputObservedFact<Value>> {
  try {
    return Object.freeze({ status: 'observed', value: await observe() })
  } catch (error) {
    return Object.freeze({
      status: 'unavailable',
      exception: context?.exception(error) ?? persistentOutputRawException(error),
    })
  }
}

export function persistentOutputRawException(error: unknown): PersistentOutputRawException {
  return snapshotException(error, value => value)
}

export async function captureCheckpointFailureFacts(
  checkpoints: FileCheckpointJournal,
  fileId: string,
  context: PersistentOutputFailureFactContext,
): Promise<Omit<PersistentOutputFailureFacts, 'observation'>> {
  const candidates = await persistentOutputObservedFact(
    () => scanCheckpointFacts(scan => checkpoints.scanCandidates(scan), fileId, context),
    context,
  )
  const committed = await persistentOutputObservedFact(
    () => scanCheckpointFacts(scan => checkpoints.scanCommitted(scan), fileId, context),
    context,
  )
  return Object.freeze({
    checkpoint: Object.freeze({ candidates, committed }),
  })
}

export function mergePersistentOutputFailureFacts(
  left: Omit<PersistentOutputFailureFacts, 'observation'>,
  right: Omit<PersistentOutputFailureFacts, 'observation'>,
): Omit<PersistentOutputFailureFacts, 'observation'> {
  return Object.freeze({
    ...(left.fsa === undefined && right.fsa === undefined
      ? {}
      : { fsa: left.fsa ?? right.fsa }),
    ...(left.checkpoint === undefined && right.checkpoint === undefined
      ? {}
      : { checkpoint: left.checkpoint ?? right.checkpoint }),
    ...((left.probeFailures?.length ?? 0) + (right.probeFailures?.length ?? 0) === 0
      ? {}
      : { probeFailures: Object.freeze([
          ...(left.probeFailures ?? []),
          ...(right.probeFailures ?? []),
        ]) }),
  })
}

async function scanCheckpointFacts(
  scan: (request: FileCheckpointScan) => Promise<{
    readonly records: readonly FileCheckpointV2[]
    readonly nextCursor?: string
  }>,
  fileId: string,
  context: PersistentOutputFailureFactContext,
): Promise<readonly PersistentOutputCheckpointRecordFact[]> {
  const records: PersistentOutputCheckpointRecordFact[] = []
  let cursor: string | undefined
  do {
    context.signal.throwIfAborted()
    if (!context.claimCheckpointPage()) break
    const remaining = context.remainingCheckpointRecords()
    if (remaining === 0) {
      context.markTruncated('checkpoint-records')
      break
    }
    const page = await scan({
      direction: 'ascending',
      fileId,
      limit: remaining,
      ...(cursor === undefined ? {} : { cursor }),
    })
    const retainedFromPage = retainCheckpointRecords(page.records, records, context)
    if (retainedFromPage < page.records.length) context.markTruncated('checkpoint-records')
    cursor = page.nextCursor
    if (cursor !== undefined && context.remainingCheckpointRecords() === 0) {
      context.markTruncated('checkpoint-records')
      break
    }
  } while (cursor !== undefined)
  return Object.freeze(records)
}

function retainCheckpointRecords(
  source: readonly FileCheckpointV2[],
  destination: PersistentOutputCheckpointRecordFact[],
  context: PersistentOutputFailureFactContext,
): number {
  let retained = 0
  for (const record of source) {
    const captured = context.checkpointRecord(record)
    if (captured === undefined) break
    destination.push(captured)
    retained += 1
  }
  return retained
}

function snapshotException(
  error: unknown,
  bound: (value: string) => string,
): PersistentOutputRawException {
  return Object.freeze({
    raw: error,
    valueType: error === null ? 'null' : typeof error,
    ...exceptionObjectFields(error, bound),
  })
}

function exceptionObjectFields(
  error: unknown,
  bound: (value: string) => string,
): Partial<Pick<PersistentOutputRawException, 'constructorName' | 'name' | 'message' | 'stack'>> {
  if (typeof error !== 'object' || error === null) return Object.freeze({})
  try {
    const candidate = error as {
      readonly constructor?: { readonly name?: unknown }
      readonly name?: unknown
      readonly message?: unknown
      readonly stack?: unknown
    }
    return Object.freeze({
      ...(typeof candidate.constructor?.name === 'string'
        ? { constructorName: bound(candidate.constructor.name) }
        : {}),
      ...(typeof candidate.name === 'string' ? { name: bound(candidate.name) } : {}),
      ...(typeof candidate.message === 'string' ? { message: bound(candidate.message) } : {}),
      ...(typeof candidate.stack === 'string' ? { stack: bound(candidate.stack) } : {}),
    })
  } catch {
    return Object.freeze({})
  }
}

function truncateUTF8(
  value: string,
  maximumBytes: number,
): Readonly<{ value: string; bytes: number; truncated: boolean }> {
  if (maximumBytes <= 0) {
    return Object.freeze({ value: '', bytes: 0, truncated: value.length !== 0 })
  }
  let retained = ''
  let bytes = 0
  for (const scalar of value) {
    const scalarBytes = new TextEncoder().encode(scalar).byteLength
    if (bytes + scalarBytes > maximumBytes) {
      return Object.freeze({ value: retained, bytes, truncated: true })
    }
    retained += scalar
    bytes += scalarBytes
  }
  return Object.freeze({ value: retained, bytes, truncated: false })
}
