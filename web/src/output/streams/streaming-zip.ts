import {
  validateSealedZipLayoutPlan,
  type SealedZipLayoutPlanV1,
} from '../zip-layout/layout'
import {
  ZipCrc32,
  ZIP_ENCODING_POLICY_V1,
  checkedZipAdd,
  type ZipEntryPlanV1,
} from '../zip-layout/policy'
import type {
  ZipArchiveLayoutAuthority,
  ZipArchiveMember,
  ZipArchiveWriter,
} from './zip-archive'
import type { ZipCentralDirectorySpool } from './zip-spool'
import { ZipOutputSink } from './zip-output-sink'

export type ZipArchiveTraceEvent =
  | Readonly<{
      kind: 'zip-close-proof-accepted'
      receiveIntentDigest: string
      layoutDigest: string
      entryCount: bigint
    }>
  | Readonly<{
      kind: 'zip-archive-committed'
      receiveIntentDigest: string
      layoutDigest: string
      exactArchiveBytes: bigint
    }>
  | Readonly<{
      kind: 'zip-archive-aborted'
      receiveIntentDigest: string
      layoutDigest?: string
    }>

export type ZipArchiveObserver = (event: ZipArchiveTraceEvent) => void

type WriterState = 'open' | 'closing' | 'committed' | 'aborting' | 'aborted' | 'failed'

/** Store-mode writer whose only length and encoding inputs are immutable layout plans. */
export class StreamingZipArchiveWriter implements ZipArchiveWriter {
  readonly #spool: ZipCentralDirectorySpool
  readonly #output: ZipOutputSink
  readonly #authority: ZipArchiveLayoutAuthority
  readonly #observe: ZipArchiveObserver | undefined
  readonly #centralDirectoryCrc32 = new ZipCrc32()
  #active: StreamingZipMember | undefined
  #offset = 0n
  #entryIndex = 0
  #state: WriterState = 'open'
  #settlementPromise: Promise<void> | undefined
  #abortPromise: Promise<void> | undefined
  #closeController: AbortController | undefined
  #settlementCleanupFailure: unknown
  #spoolCleanupFailure: unknown
  #spoolCleanupPromise: Promise<void> | undefined
  #closingLayoutDigest: string | undefined

  constructor(
    output: WritableStream<Uint8Array>,
    spool: ZipCentralDirectorySpool,
    authority: ZipArchiveLayoutAuthority,
    observe?: ZipArchiveObserver,
  ) {
    if (authority.kind !== 'sealed' && authority.kind !== 'progressive') {
      throw new TypeError('ZIP layout authority is invalid')
    }
    this.#output = new ZipOutputSink(output)
    this.#spool = spool
    this.#authority = authority.kind === 'sealed'
      ? Object.freeze({ kind: 'sealed', plan: authority.plan })
      : Object.freeze({ kind: 'progressive', ledger: authority.ledger })
    this.#observe = observe
  }

  get cleanupPending(): boolean {
    return this.#spoolCleanupFailure !== undefined
  }

  get cleanupFailure(): unknown {
    return this.#spoolCleanupFailure
  }

  async addDirectory(entry: ZipEntryPlanV1): Promise<void> {
    this.#requireIdle()
    try {
      const plan = this.#claimEntry(entry, 'directory')
      await this.#writeMetadata(ZIP_ENCODING_POLICY_V1.encodeLocalHeader(plan))
      await this.#writeMetadata(ZIP_ENCODING_POLICY_V1.encodeDataDescriptor(plan, 0))
      this.#assertEntryStreamComplete(plan)
      await this.#appendCentralRecord(plan, 0)
      this.#entryIndex += 1
    } catch (error) {
      await this.#failEntry(error, 'ZIP directory output and abort failed')
      throw error
    }
  }

  async beginFile(entry: ZipEntryPlanV1): Promise<ZipArchiveMember> {
    this.#requireIdle()
    let plan: ZipEntryPlanV1
    try {
      plan = this.#claimEntry(entry, 'file')
      await this.#writeMetadata(ZIP_ENCODING_POLICY_V1.encodeLocalHeader(plan))
    } catch (error) {
      await this.#failEntry(error, 'ZIP member start and abort failed')
      throw error
    }
    const member = new StreamingZipMember(
      plan,
      (chunk) => this.#writePayload(chunk),
      (crc32) => this.#commitMember(plan, crc32),
      (reason) => this.#memberFailed(reason),
      () => {
        if (this.#active === member) this.#active = undefined
      },
    )
    this.#active = member
    return member
  }

  close(proof: SealedZipLayoutPlanV1, signal: AbortSignal): Promise<void> {
    this.#requireIdle()
    signal.throwIfAborted()
    this.#state = 'closing'
    const controller = new AbortController()
    this.#closeController = controller
    const detach = forwardAbort(signal, controller)
    const operation = this.#performClose(proof, controller.signal).then(
      () => { this.#state = 'committed' },
      (error: unknown) => {
        this.#state = this.#settlementCleanupFailure === undefined ? 'aborted' : 'failed'
        throw error
      },
    ).finally(() => {
      detach()
      this.#closeController = undefined
    })
    this.#settlementPromise = operation
    return operation
  }

  abort(reason: unknown): Promise<void> {
    if (this.#abortPromise !== undefined) return this.#abortPromise
    let operation: Promise<void>
    if (this.#state === 'closing') {
      this.#closeController?.abort(reason)
      operation = this.#normalizedCloseAbort()
    } else if (this.#state === 'committed' || this.#state === 'aborted') {
      operation = Promise.resolve()
    } else if (this.#state === 'failed') {
      operation = Promise.reject(this.#settlementCleanupFailure)
    } else {
      this.#state = 'aborting'
      operation = this.#performAbort(reason).then(
        () => { this.#state = 'aborted' },
        (error: unknown) => {
          this.#state = 'failed'
          throw error
        },
      )
      this.#settlementPromise = operation
    }
    this.#abortPromise = operation
    operation.catch(() => undefined)
    return operation
  }

  retryCleanup(): Promise<void> {
    if (this.#spoolCleanupFailure === undefined) return Promise.resolve()
    if (this.#spoolCleanupPromise !== undefined) return this.#spoolCleanupPromise
    const operation = this.#spool.clear().then(
      () => { this.#spoolCleanupFailure = undefined },
      (error: unknown) => {
        this.#spoolCleanupFailure = error
        throw error
      },
    ).finally(() => { this.#spoolCleanupPromise = undefined })
    this.#spoolCleanupPromise = operation
    return operation
  }

  async #performClose(candidate: SealedZipLayoutPlanV1, signal: AbortSignal): Promise<void> {
    let failure: unknown
    let published = false
    let outputAbort: Promise<void> | undefined
    const abortOutput = () => {
      outputAbort ??= outputAbortPromise(this.#output, abortReason(signal))
      outputAbort.catch(() => undefined)
    }
    signal.addEventListener('abort', abortOutput, { once: true })
    try {
      const proof = await this.#resolveAuthorityProof(candidate)
      signal.throwIfAborted()
      if (this.#entryIndex !== proof.entries.length ||
          BigInt(this.#entryIndex) !== proof.entryCount ||
          this.#offset !== proof.centralDirectoryOffset) {
        throw new Error('ZIP writer observations do not match the close proof')
      }
      this.#closingLayoutDigest = proof.digest
      this.#emit({
        kind: 'zip-close-proof-accepted',
        receiveIntentDigest: proof.receiveIntentDigest,
        layoutDigest: proof.digest,
        entryCount: proof.entryCount,
      })
      await this.#writeCentralDirectory(proof, signal)
      await this.#output.close()
      published = true
      await outputAbort?.catch(() => undefined)
      this.#emit({
        kind: 'zip-archive-committed',
        receiveIntentDigest: proof.receiveIntentDigest,
        layoutDigest: proof.digest,
        exactArchiveBytes: proof.exactArchiveBytes,
      })
    } catch (error) {
      failure = await this.#settleFailedClose(error, outputAbort)
    } finally {
      signal.removeEventListener('abort', abortOutput)
      this.#output.releaseLock()
      failure = await this.#clearSpoolAfterSettlement(published, failure)
    }
    if (failure !== undefined) throw failure
  }

  async #resolveAuthorityProof(candidate: SealedZipLayoutPlanV1): Promise<SealedZipLayoutPlanV1> {
    if (this.#authority.kind === 'progressive') {
      if (!this.#authority.ledger.acceptsSealedPlan(candidate)) {
        throw new Error('ZIP progressive ledger did not issue the close proof')
      }
      // The ledger accepts only its immutable issued object, so rebuilding a
      // million-entry proof here would duplicate the sealing authority without
      // adding an independent validation boundary.
      return candidate
    }
    const expected = await validateSealedZipLayoutPlan(this.#authority.plan)
    if (candidate === this.#authority.plan) return expected
    const proof = await validateSealedZipLayoutPlan(candidate)
    if (expected.digest !== proof.digest ||
        expected.receiveIntentDigest !== proof.receiveIntentDigest ||
        expected.artifactDigest !== proof.artifactDigest) {
      throw new Error('ZIP prepared close proof changed')
    }
    return proof
  }

  async #writeCentralDirectory(
    proof: SealedZipLayoutPlanV1,
    signal: AbortSignal,
  ): Promise<void> {
    const manifest = await this.#spool.seal()
    const replayCrc32 = new ZipCrc32()
    signal.throwIfAborted()
    if (manifest.recordCount !== proof.entryCount ||
        manifest.byteLength !== proof.centralDirectoryBytes) {
      throw new Error('ZIP central-directory spool disagrees with the layout proof')
    }
    for (let index = 0; index < manifest.chunkCount; index += 1) {
      signal.throwIfAborted()
      const chunk = await this.#spool.readChunk(index)
      if (chunk === undefined) throw new Error('ZIP central-directory spool ended early')
      replayCrc32.update(chunk)
      await this.#writePayload(chunk)
    }
    if (this.#offset !== checkedZipAdd(
      proof.centralDirectoryOffset,
      proof.centralDirectoryBytes,
    )) {
      throw new Error('ZIP central-directory replay length changed')
    }
    if (replayCrc32.digest() !== this.#centralDirectoryCrc32.digest()) {
      throw new Error('ZIP central-directory spool content changed')
    }
    const ends = ZIP_ENCODING_POLICY_V1.encodeEndRecords(proof)
    signal.throwIfAborted()
    if (ends.zip64End !== undefined) await this.#writeMetadata(ends.zip64End)
    if (ends.zip64Locator !== undefined) await this.#writeMetadata(ends.zip64Locator)
    await this.#writeMetadata(ends.classicEnd)
    signal.throwIfAborted()
    if (this.#offset !== proof.exactArchiveBytes) {
      throw new Error('ZIP actual archive length disagrees with the layout proof')
    }
  }

  async #settleFailedClose(
    closeFailure: unknown,
    requestedAbort: Promise<void> | undefined,
  ): Promise<unknown> {
    try {
      await (requestedAbort ?? outputAbortPromise(this.#output, closeFailure))
      this.#emitAbort()
      return closeFailure
    } catch (abortFailure) {
      this.#settlementCleanupFailure = abortFailure
      return new AggregateError(
        [closeFailure, abortFailure],
        'ZIP close and output abort failed',
      )
    }
  }

  async #clearSpoolAfterSettlement(published: boolean, failure: unknown): Promise<unknown> {
    try {
      await this.#spool.clear()
      return failure
    } catch (clearFailure) {
      if (published) {
        this.#spoolCleanupFailure = clearFailure
        return failure
      }
      this.#settlementCleanupFailure = this.#settlementCleanupFailure === undefined
        ? clearFailure
        : new AggregateError(
            [this.#settlementCleanupFailure, clearFailure],
            'ZIP abort cleanup failed',
          )
      return failure === undefined
        ? clearFailure
        : new AggregateError(
            [failure, clearFailure],
            'ZIP output and metadata cleanup failed',
          )
    }
  }

  async #performAbort(reason: unknown): Promise<void> {
    const failures: unknown[] = []
    if (this.#authority.kind === 'progressive') {
      this.#authority.ledger.recordSelectedMemberFailure()
    }
    this.#active?.abandon()
    this.#active = undefined
    try {
      await this.#output.abort(reason)
    } catch (error) {
      failures.push(error)
    } finally {
      this.#output.releaseLock()
    }
    try {
      await this.#spool.clear()
    } catch (error) {
      failures.push(error)
    }
    this.#emitAbort()
    if (failures.length > 0) {
      this.#settlementCleanupFailure = new AggregateError(failures, 'ZIP stream abort failed')
      throw this.#settlementCleanupFailure
    }
  }

  async #normalizedCloseAbort(): Promise<void> {
    const settlement = this.#settlementPromise
    if (settlement === undefined) throw new Error('ZIP close settlement is missing')
    try {
      await settlement
    } catch (error) {
      if (this.#state === 'aborted') return
      throw this.#settlementCleanupFailure ?? error
    }
  }

  #claimEntry(entry: ZipEntryPlanV1, kind: 'directory' | 'file'): ZipEntryPlanV1 {
    if (entry.kind !== kind) throw new TypeError(`ZIP ${kind} plan has the wrong entry kind`)
    const plan = ZIP_ENCODING_POLICY_V1.validateEntryPlan(entry, this.#offset)
    const expected = this.#authority.kind === 'sealed'
      ? this.#authority.plan.entries[this.#entryIndex]
      : this.#authority.ledger.entryAt(this.#entryIndex)
    if (expected === undefined || !ZIP_ENCODING_POLICY_V1.sameEntryPlan(plan, expected)) {
      throw new Error('ZIP writer entry order changed from the layout authority')
    }
    return plan
  }

  async #commitMember(plan: ZipEntryPlanV1, crc32: number): Promise<void> {
    if (this.#active === undefined) throw new Error('ZIP member is not active')
    await this.#writeMetadata(ZIP_ENCODING_POLICY_V1.encodeDataDescriptor(plan, crc32))
    this.#assertEntryStreamComplete(plan)
    await this.#appendCentralRecord(plan, crc32)
    this.#entryIndex += 1
  }

  async #appendCentralRecord(plan: ZipEntryPlanV1, crc32: number): Promise<void> {
    const record = ZIP_ENCODING_POLICY_V1.encodeCentralRecord(plan, crc32)
    this.#centralDirectoryCrc32.update(record)
    await this.#spool.append(record)
  }

  async #memberFailed(reason: unknown): Promise<void> {
    if (this.#authority.kind === 'progressive') {
      this.#authority.ledger.recordSelectedMemberFailure()
    }
    await this.abort(reason)
  }

  async #failEntry(reason: unknown, message: string): Promise<void> {
    try {
      await this.#memberFailed(reason)
    } catch (abortError) {
      throw new AggregateError([reason, abortError], message, { cause: abortError })
    }
  }

  async #writeMetadata(chunk: Uint8Array): Promise<void> {
    await this.#output.appendMetadata(chunk)
    this.#offset = checkedZipAdd(this.#offset, BigInt(chunk.byteLength))
  }

  async #writePayload(chunk: Uint8Array): Promise<void> {
    await this.#output.writePayload(chunk)
    this.#offset = checkedZipAdd(this.#offset, BigInt(chunk.byteLength))
  }

  #assertEntryStreamComplete(plan: ZipEntryPlanV1): void {
    const expected = checkedZipAdd(plan.localHeaderOffset, plan.entryStreamBytes)
    if (this.#offset !== expected) throw new Error('ZIP entry bytes disagree with the entry plan')
  }

  #requireIdle(): void {
    if (this.#state !== 'open') throw new Error('ZIP archive is settled')
    if (this.#active !== undefined) throw new Error('ZIP archive already has an active member')
  }

  #emit(event: ZipArchiveTraceEvent): void {
    try {
      this.#observe?.(event)
    } catch {
      // Observers are diagnostic-only and cannot change publication semantics.
    }
  }

  #emitAbort(): void {
    const receiveIntentDigest = this.#authority.kind === 'sealed'
      ? this.#authority.plan.receiveIntentDigest
      : this.#authority.ledger.receiveIntentDigest
    this.#emit(this.#closingLayoutDigest === undefined
      ? { kind: 'zip-archive-aborted', receiveIntentDigest }
      : { kind: 'zip-archive-aborted', receiveIntentDigest, layoutDigest: this.#closingLayoutDigest })
  }
}

class StreamingZipMember implements ZipArchiveMember {
  readonly #plan: ZipEntryPlanV1
  readonly #writeOutput: (chunk: Uint8Array) => Promise<void>
  readonly #commit: (crc32: number) => Promise<void>
  readonly #failed: (reason: unknown) => Promise<void>
  readonly #settled: () => void
  readonly #crc32 = new ZipCrc32()
  #written = 0n
  #closed = false
  #failureReported = false

  constructor(
    plan: ZipEntryPlanV1,
    writeOutput: (chunk: Uint8Array) => Promise<void>,
    commit: (crc32: number) => Promise<void>,
    failed: (reason: unknown) => Promise<void>,
    settled: () => void,
  ) {
    this.#plan = plan
    this.#writeOutput = writeOutput
    this.#commit = commit
    this.#failed = failed
    this.#settled = once(settled)
  }

  async write(data: Uint8Array): Promise<void> {
    this.#requireOpen()
    try {
      if (!(data instanceof Uint8Array)) throw new TypeError('ZIP member data must be bytes')
      if (checkedZipAdd(this.#written, BigInt(data.byteLength)) > this.#plan.exactSize) {
        throw new RangeError('ZIP member received more bytes than declared')
      }
      if (data.byteLength === 0) return
      const snapshot = data.slice()
      const nextWritten = checkedZipAdd(this.#written, BigInt(snapshot.byteLength))
      this.#crc32.update(snapshot)
      await this.#writeOutput(snapshot)
      this.#written = nextWritten
    } catch (error) {
      await this.#fail(error)
      throw error
    }
  }

  async close(): Promise<void> {
    this.#requireOpen()
    try {
      if (this.#written !== this.#plan.exactSize) {
        throw new Error('ZIP member size does not match its declaration')
      }
      this.#closed = true
      await this.#commit(this.#crc32.digest())
      this.#settled()
    } catch (error) {
      await this.#fail(error)
      throw error
    }
  }

  async abort(reason: unknown): Promise<void> {
    if (this.#closed) return
    await this.#fail(reason)
  }

  abandon(): void {
    if (this.#closed) return
    this.#closed = true
    this.#settled()
  }

  async #fail(reason: unknown): Promise<void> {
    if (this.#failureReported) return
    this.#failureReported = true
    this.#closed = true
    this.#settled()
    await this.#failed(reason)
  }

  #requireOpen(): void {
    if (this.#closed) throw new Error('ZIP member is settled')
  }
}

function abortReason(signal: AbortSignal): unknown {
  return signal.reason ?? new DOMException('ZIP output aborted', 'AbortError')
}

function outputAbortPromise(
  output: ZipOutputSink,
  reason: unknown,
): Promise<void> {
  try {
    return output.abort(reason)
  } catch (error) {
    return Promise.reject(error)
  }
}

function forwardAbort(source: AbortSignal, target: AbortController): () => void {
  const abort = () => target.abort(abortReason(source))
  if (source.aborted) {
    abort()
    return () => {}
  }
  source.addEventListener('abort', abort, { once: true })
  return () => source.removeEventListener('abort', abort)
}

function once(action: () => void): () => void {
  let called = false
  return () => {
    if (called) return
    called = true
    action()
  }
}
