import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../diagnostics'
import type { PreparationFileEntry } from '../workspace/preparation'
import {
  type BeginOutputFileResult,
  type AutomaticCheckpointResult,
  type AutomaticCheckpointTrigger,
  type FileRetirementDisposition,
  type OpenedOutputRevision,
  type OutputCapabilities,
  type OutputFileOwnership,
  type OutputFileRequest,
  type OutputFileTransaction,
  type OutputSession,
  type OutputSessionIdentity,
  OutputSessionBindingError,
  VerifiedFinalOutputFile,
  VerifiedDurableRanges,
  disabledOutputExecutionProfile,
  outputCapabilities,
  outputSessionIdentity,
  snapshotOpenedOutputRevision,
  snapshotOutputFileRequest,
} from '../../transfer/output-session'
import type { ReceiveIntent } from '../../transfer/intent'
import { SingleFileStreamOutputSession } from '../streams/single-file'
import { StreamingZipArchiveWriter } from '../streams/streaming-zip'
import type { ZipArchiveMember, ZipArchiveWriter } from '../streams/zip-archive'
import type { ZipCentralDirectorySpool } from '../streams/zip-spool'
import type { SealedZipLayoutPlanV1 } from '../zip-layout/layout'
import type { ZipEntryPlanV1 } from '../zip-layout/policy'
import {
  PortableHandoffError,
  type PortableHandoffSession,
} from './browser-download'

export const PORTABLE_ZIP_OUTPUT_BACKEND = 'portable-sealed-zip'

export interface PortablePreparedOutput extends OutputSession {
  readonly cleanupPending: boolean
  finalize(signal: AbortSignal): Promise<void>
  abort(reason: unknown): Promise<void>
  retryCleanup(): Promise<void>
}

export type PortableZipArchiveWriterFactory = (
  output: WritableStream<Uint8Array<ArrayBuffer>>,
  spool: ZipCentralDirectorySpool,
  layout: SealedZipLayoutPlanV1,
) => ZipArchiveWriter

export class PortableOriginalOutputSession implements PortablePreparedOutput {
  readonly identity: OutputSessionIdentity
  readonly capabilities: OutputCapabilities
  readonly executionProfile
  readonly #intent: ReceiveIntent
  readonly #entry: PreparationFileEntry
  readonly #handoff: PortableHandoffSession
  readonly #inner: SingleFileStreamOutputSession
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  #transaction: OutputFileTransaction | undefined
  #beginPending: Promise<BeginOutputFileResult> | undefined

  constructor(input: Readonly<{
    intent: ReceiveIntent
    entry: PreparationFileEntry
    handoff: PortableHandoffSession
    diagnostics?: OutputDiagnosticsPorts
  }>) {
    this.#intent = input.intent
    this.#entry = input.entry
    this.#handoff = input.handoff
    this.#diagnostics = input.diagnostics
    this.#inner = new SingleFileStreamOutputSession(
      `${input.intent.plan.kind === 'portable-handoff'
        ? input.intent.plan.portable.portablePlanId
        : input.intent.operationId}:original`,
      input.handoff.writable,
    )
    this.identity = this.#inner.identity
    this.capabilities = this.#inner.capabilities
    this.executionProfile = this.#inner.executionProfile
  }

  get cleanupPending(): boolean {
    return false
  }

  async beginFile(
    input: OutputFileRequest,
    signal: AbortSignal,
  ): Promise<BeginOutputFileResult> {
    if (this.#beginPending !== undefined) {
      throw new OutputSessionBindingError('portable original output accepts exactly one file')
    }
    const request = this.#snapshotRequest(input)
    const opening = this.#inner.beginFile(request, signal).then((result) => {
      this.#transaction = observePortableTransaction(
        result.transaction,
        this.#diagnostics,
      )
      return Object.freeze({
        ...result,
        transaction: this.#transaction,
      })
    }).catch((error: unknown) => {
      recordPortableFailure(this.#diagnostics, 'output_write', error)
      throw error
    })
    this.#beginPending = opening
    return opening
  }

  async finalize(signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    if (this.#transaction === undefined) {
      const error = new PortableHandoffError('preparation-invalidated')
      recordPortableFailure(this.#diagnostics, 'output_commit', error)
      throw error
    }
  }

  async abort(reason: unknown): Promise<void> {
    const opening = this.#beginPending
    if (opening !== undefined) {
      try {
        const result = await opening
        await result.transaction.pause(reason)
        return
      } catch {
        // A failed revision open leaves the stream unlocked for direct cleanup.
      }
    }
    if (!this.#handoff.writable.locked) {
      try {
        await this.#handoff.writable.abort(reason)
      } catch (error) {
        recordPortableFailure(this.#diagnostics, 'cleanup', error)
        throw error
      }
    }
  }

  retryCleanup(): Promise<void> {
    return Promise.resolve()
  }

  #snapshotRequest(input: OutputFileRequest): OutputFileRequest {
    try {
      const request = snapshotOutputFileRequest(input)
      assertPreparedFileRequest(this.#intent, this.#entry, request, this.capabilities)
      return request
    } catch (error) {
      recordPortableFailure(this.#diagnostics, 'output_write', error)
      throw error
    }
  }
}

type ZipSessionState = 'open' | 'closing' | 'closed' | 'failed'

export class PortableSealedZipOutputSession implements PortablePreparedOutput {
  readonly identity: OutputSessionIdentity
  readonly capabilities: OutputCapabilities = outputCapabilities({
    durability: 'None',
    randomWrite: false,
    fileFailureIsolation: false,
    modificationTime: false,
  })
  readonly executionProfile = disabledOutputExecutionProfile(1)

  readonly #intent: ReceiveIntent
  readonly #entriesByPath: ReadonlyMap<string, PreparationFileEntry>
  readonly #layout: SealedZipLayoutPlanV1
  readonly #handoff: PortableHandoffSession
  readonly #createSpool: () => ZipCentralDirectorySpool
  readonly #createWriter: PortableZipArchiveWriterFactory
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  #writer: ZipArchiveWriter | undefined
  #entryIndex = 0
  #tail: Promise<void> = Promise.resolve()
  #failure: unknown
  readonly #diagnosticFailureStages = new Set<PortableDiagnosticFailureStage>()
  #state: ZipSessionState = 'open'
  #abortPromise: Promise<void> | undefined

  constructor(input: Readonly<{
    intent: ReceiveIntent
    files: readonly PreparationFileEntry[]
    layout: SealedZipLayoutPlanV1
    handoff: PortableHandoffSession
    createSpool: () => ZipCentralDirectorySpool
    createWriter?: PortableZipArchiveWriterFactory
    diagnostics?: OutputDiagnosticsPorts
  }>) {
    this.#intent = input.intent
    this.#layout = input.layout
    this.#handoff = input.handoff
    this.#createSpool = input.createSpool
    this.#diagnostics = input.diagnostics
    this.#createWriter = input.createWriter ?? ((output, spool, layout) =>
      new StreamingZipArchiveWriter(output, spool, {
        kind: 'sealed',
        plan: layout,
      }))
    this.identity = outputSessionIdentity({
      backend: PORTABLE_ZIP_OUTPUT_BACKEND,
      outputSessionId: `${input.intent.plan.kind === 'portable-handoff'
        ? input.intent.plan.portable.portablePlanId
        : input.intent.operationId}:zip`,
    })
    this.#entriesByPath = preparedFilesByArtifactPath(input.files)
  }

  get cleanupPending(): boolean {
    return this.#writer?.cleanupPending ?? false
  }

  beginFile(
    input: OutputFileRequest,
    signal: AbortSignal,
  ): Promise<BeginOutputFileResult> {
    if (this.#state !== 'open') {
      return Promise.reject(new Error('portable ZIP output is not accepting files'))
    }
    const request = snapshotOutputFileRequest(input)
    const predecessor = this.#tail
    let resolveSettlement!: () => void
    let rejectSettlement!: (reason: unknown) => void
    let settled = false
    const settlement = new Promise<void>((resolve, reject) => {
      resolveSettlement = resolve
      rejectSettlement = reject
    })
    const finish = (failure?: unknown): void => {
      if (settled) return
      settled = true
      if (failure === undefined) resolveSettlement()
      else rejectSettlement(failure)
    }
    this.#tail = predecessor.then(() => settlement)
    this.#tail.catch(() => undefined)

    const operation = predecessor.then(async () => {
      signal.throwIfAborted()
      this.#requireHealthy()
      const entry = this.#nextFile(request)
      const revision = snapshotOpenedOutputRevision(await request.openRevision(signal))
      signal.throwIfAborted()
      assertMatchingRevision(request, revision)

      // ZIP directory records before this member are emitted only after the
      // member revision is authenticated, preserving OutputSession's no-placeholder boundary.
      await this.#writeDirectoriesBefore(entry)
      const member = await this.#archiveWriter().beginFile(entry)
      const ownership: OutputFileOwnership = Object.freeze({
        ...this.identity,
        canonicalPath: request.materializationRelativePath,
        ownedFileIdentity: `${this.identity.outputSessionId}:${this.#entryIndex}`,
      })
      const durableRanges = new VerifiedDurableRanges(
        ownership,
        revision,
        revision.exactSize,
        [],
      )
      const transaction = observePortableTransaction(
        new PortableZipMemberTransaction({
          member,
          revision,
          ownership,
          committed: () => {
            this.#entryIndex += 1
            finish()
          },
          failed: (error) => {
            this.#recordFailure(error)
            finish(error)
          },
        }),
        this.#diagnostics,
        (stage, error) => {
          this.#recordFailure(error)
          this.#recordDiagnosticFailure(stage, error)
        },
      )
      return Object.freeze({ revision, transaction, durableRanges })
    }).catch((error: unknown) => {
      this.#recordFailure(error)
      this.#recordDiagnosticFailure('output_write', error)
      finish(error)
      this.abort(error).catch(() => undefined)
      throw error
    })
    return operation
  }

  async finalize(signal: AbortSignal): Promise<void> {
    try {
      await this.#finalize(signal)
    } catch (error) {
      const cleanupFailed = this.#writer?.cleanupPending === true
      if (cleanupFailed || this.#failure === undefined) {
        this.#recordDiagnosticFailure(
          cleanupFailed ? 'cleanup' : 'output_commit',
          error,
        )
      }
      throw error
    }
  }

  async #finalize(signal: AbortSignal): Promise<void> {
    if (this.#state !== 'open') throw new Error('portable ZIP output is already settled')
    this.#state = 'closing'
    await this.#tail
    signal.throwIfAborted()
    this.#requireHealthy()

    const remaining = this.#layout.entries.slice(this.#entryIndex)
    if (remaining.some(entry => entry.kind === 'file')) {
      const error = new PortableHandoffError('preparation-invalidated')
      this.#recordFailure(error)
      await this.abort(error).catch(() => undefined)
      throw error
    }

    const writer = this.#archiveWriter()
    for (const entry of remaining) {
      await writer.addDirectory(entry)
      this.#entryIndex += 1
    }
    await writer.close(this.#layout, signal)
    if (writer.cleanupPending) {
      // Publication may already have crossed the browser boundary, but lingering
      // spool ownership is not silently called a clean DownloadStarted settlement.
      await writer.retryCleanup()
    }
    if (writer.cleanupPending) throw writer.cleanupFailure
    this.#state = 'closed'
  }

  abort(reason: unknown): Promise<void> {
    if (this.#abortPromise !== undefined) return this.#abortPromise
    const operation = this.#writer === undefined
      ? abortUnlockedStream(this.#handoff.writable, reason)
      : this.#writer.abort(reason)
    this.#abortPromise = operation.then(
      () => { this.#state = 'closed' },
      (error: unknown) => {
        this.#recordFailure(error)
        this.#recordDiagnosticFailure('cleanup', error)
        throw error
      },
    )
    this.#abortPromise.catch(() => undefined)
    return this.#abortPromise
  }

  async retryCleanup(): Promise<void> {
    try {
      await (this.#writer?.retryCleanup() ?? Promise.resolve())
    } catch (error) {
      this.#recordDiagnosticFailure('cleanup', error)
      throw error
    }
  }

  #nextFile(
    request: OutputFileRequest,
  ): ZipEntryPlanV1 {
    let index = this.#entryIndex
    while (this.#layout.entries[index]?.kind === 'directory') index += 1
    const entry = this.#layout.entries[index]
    if (entry === undefined || entry.kind !== 'file') {
      throw new OutputSessionBindingError('portable ZIP received an unexpected file')
    }
    const file = this.#entriesByPath.get(pathKey(entry.path))
    if (file === undefined) {
      throw new OutputSessionBindingError('sealed ZIP member lacks preparation evidence')
    }
    assertPreparedFileRequest(this.#intent, file, request, this.capabilities)
    return entry
  }

  async #writeDirectoriesBefore(file: ZipEntryPlanV1): Promise<void> {
    const writer = this.#archiveWriter()
    while (this.#layout.entries[this.#entryIndex] !== file) {
      const entry = this.#layout.entries[this.#entryIndex]
      if (entry === undefined || entry.kind !== 'directory') {
        throw new OutputSessionBindingError('portable ZIP member order changed from preparation')
      }
      await writer.addDirectory(entry)
      this.#entryIndex += 1
    }
  }

  #archiveWriter(): ZipArchiveWriter {
    this.#writer ??= this.#createWriter(
      this.#handoff.writable,
      this.#createSpool(),
      this.#layout,
    )
    return this.#writer
  }

  #recordDiagnosticFailure(
    stage: PortableDiagnosticFailureStage,
    error: unknown,
  ): void {
    if (this.#diagnosticFailureStages.has(stage)) return
    this.#diagnosticFailureStages.add(stage)
    recordPortableFailure(this.#diagnostics, stage, error)
  }

  #recordFailure(error: unknown): void {
    this.#failure ??= error
    if (this.#state !== 'closed') this.#state = 'failed'
  }

  #requireHealthy(): void {
    if (this.#failure !== undefined) {
      throw new Error('portable ZIP output is compromised', { cause: this.#failure })
    }
  }
}

interface PortableZipMemberTransactionInput {
  readonly member: ZipArchiveMember
  readonly revision: OpenedOutputRevision
  readonly ownership: OutputFileOwnership
  readonly committed: () => void
  readonly failed: (error: unknown) => void
}

class PortableZipMemberTransaction implements OutputFileTransaction {
  readonly #member: ZipArchiveMember
  readonly #revision: OpenedOutputRevision
  readonly #ownership: OutputFileOwnership
  readonly #committed: () => void
  readonly #failed: (error: unknown) => void
  #tail: Promise<unknown> = Promise.resolve()
  #nextOffset = 0n
  #settled = false

  constructor(input: PortableZipMemberTransactionInput) {
    this.#member = input.member
    this.#revision = input.revision
    this.#ownership = input.ownership
    this.#committed = once(input.committed)
    this.#failed = once(input.failed)
  }

  writeRange(offset: bigint, data: Uint8Array, signal: AbortSignal): Promise<void> {
    const snapshot = data.slice()
    return this.#enqueue(async () => {
      signal.throwIfAborted()
      this.#requireOpen()
      if (offset !== this.#nextOffset ||
          offset + BigInt(snapshot.byteLength) > this.#revision.exactSize) {
        throw new RangeError('portable ZIP member requires contiguous ascending ranges')
      }
      if (snapshot.byteLength === 0) return
      await this.#member.write(snapshot)
      signal.throwIfAborted()
      this.#nextOffset += BigInt(snapshot.byteLength)
    })
  }

  automaticCheckpoint(
    _trigger: AutomaticCheckpointTrigger,
    signal: AbortSignal,
  ): Promise<AutomaticCheckpointResult> {
    return this.#enqueue(async () => {
      signal.throwIfAborted()
      this.#requireOpen()
      return Object.freeze({
        kind: 'finished' as const,
        reason: 'cost-evidence-unavailable' as const,
      })
    })
  }

  commit(signal: AbortSignal): Promise<VerifiedFinalOutputFile> {
    return this.#enqueue(async () => {
      signal.throwIfAborted()
      this.#requireOpen()
      if (this.#nextOffset !== this.#revision.exactSize) {
        throw new Error('portable ZIP member is incomplete')
      }
      try {
        await this.#member.close()
        this.#settled = true
        this.#committed()
      } catch (error) {
        this.#settled = true
        this.#failed(error)
        throw error
      }
      return new VerifiedFinalOutputFile(
        this.#ownership,
        this.#revision,
        this.#revision.exactSize,
      )
    })
  }

  retire(reason: unknown): Promise<FileRetirementDisposition> {
    return this.#enqueue<FileRetirementDisposition>(async () => {
      if (!this.#settled) {
        try {
          await this.#member.abort(reason)
        } catch (error) {
          this.#failed(error)
          throw error
        } finally {
          this.#settled = true
        }
        this.#failed(reason)
      }
      return 'JobOutputCompromised'
    })
  }

  async pause(reason: unknown): Promise<VerifiedDurableRanges> {
    await this.retire(reason)
    return new VerifiedDurableRanges(
      this.#ownership,
      this.#revision,
      this.#revision.exactSize,
      [],
    )
  }

  #enqueue<T>(operation: () => Promise<T>): Promise<T> {
    const result = this.#tail.then(operation, operation)
    this.#tail = result
    return result
  }

  #requireOpen(): void {
    if (this.#settled) throw new Error('portable ZIP member is settled')
  }
}

type PortableDiagnosticFailureStage =
  | 'output_write'
  | 'output_commit'
  | 'cleanup'

function observePortableTransaction(
  transaction: OutputFileTransaction,
  diagnostics: OutputDiagnosticsPorts | undefined,
  onFailure?: (stage: PortableDiagnosticFailureStage, error: unknown) => void,
): OutputFileTransaction {
  const observedFailureStages = new Set<PortableDiagnosticFailureStage>()
  const observe = async <Result>(
    stage: PortableDiagnosticFailureStage,
    operation: () => Promise<Result>,
  ): Promise<Result> => {
    try {
      return await operation()
    } catch (error) {
      if (!observedFailureStages.has(stage)) {
        observedFailureStages.add(stage)
        if (onFailure === undefined) recordPortableFailure(diagnostics, stage, error)
        else onFailure(stage, error)
      }
      throw error
    }
  }
  const observed: OutputFileTransaction = {
    writeRange: (offset, data, signal) =>
      observe('output_write', () => transaction.writeRange(offset, data, signal)),
    automaticCheckpoint: (trigger, signal) =>
      observe('output_write', () => transaction.automaticCheckpoint(trigger, signal)),
    commit: async signal => {
      const proof = await observe('output_commit', () => transaction.commit(signal))
      emitOutputTrace(diagnostics?.trace, () =>
        outputTraceEvent('output_write', {
          backend: 'portable',
          transition: 'transaction_committed',
        }))
      return proof
    },
    retire: reason =>
      observe('cleanup', () => transaction.retire(reason)),
    pause: reason =>
      observe('cleanup', () => transaction.pause(reason)),
  }
  return Object.freeze(observed)
}

function recordPortableFailure(
  diagnostics: OutputDiagnosticsPorts | undefined,
  stage: PortableDiagnosticFailureStage,
  error: unknown,
): void {
  switch (stage) {
    case 'output_write':
      recordOutputException(diagnostics?.failures?.outputWrite, error)
      emitOutputTrace(diagnostics?.trace, () =>
        outputTraceEvent('output_write', {
          backend: 'portable',
          transition: 'transaction_failed',
        }))
      return
    case 'output_commit':
      recordOutputException(diagnostics?.failures?.outputCommit, error)
      emitOutputTrace(diagnostics?.trace, () =>
        outputTraceEvent('output_write', {
          backend: 'portable',
          transition: 'commit_failed',
        }))
      return
    case 'cleanup':
      recordOutputException(diagnostics?.failures?.cleanup, error)
      emitOutputTrace(diagnostics?.trace, () =>
        outputTraceEvent('cleanup', {
          backend: 'portable',
          transition: 'failed',
        }))
  }
}

function preparedFilesByArtifactPath(
  entries: readonly PreparationFileEntry[],
): ReadonlyMap<string, PreparationFileEntry> {
  const files = new Map<string, PreparationFileEntry>()
  for (const entry of entries) {
    const key = pathKey(entry.artifactPath)
    if (files.has(key)) {
      throw new OutputSessionBindingError('portable preparation duplicates a file path')
    }
    files.set(key, entry)
  }
  return files
}

function assertPreparedFileRequest(
  intent: ReceiveIntent,
  evidence: PreparationFileEntry,
  request: OutputFileRequest,
  capabilities: OutputCapabilities,
): void {
  if (request.source.shareInstance !== intent.shareInstance ||
      request.source.fileId !== evidence.fileId ||
      request.expectedSize !== evidence.exactSize ||
      !samePath(request.sourceAuthenticationPath, evidence.sourcePath) ||
      !samePath(request.logicalArtifactPath, evidence.artifactPath) ||
      !samePath(request.materializationRelativePath, evidence.artifactPath) ||
      request.parentAdmission !== undefined ||
      !matchesPreparedModifiedTime(capabilities, request.modifiedTime, evidence.modifiedTime)) {
    throw new OutputSessionBindingError(
      'output file request does not match exact portable preparation',
    )
  }
}

function assertMatchingRevision(
  request: OutputFileRequest,
  revision: OpenedOutputRevision,
): void {
  if (revision.shareInstance !== request.source.shareInstance ||
      revision.fileId !== request.source.fileId ||
      revision.exactSize !== request.expectedSize) {
    throw new OutputSessionBindingError(
      'authenticated revision does not match the prepared portable member',
    )
  }
}

function matchesPreparedModifiedTime(
  capabilities: OutputCapabilities,
  request: OutputFileRequest['modifiedTime'],
  evidence: PreparationFileEntry['modifiedTime'],
): boolean {
  // Transfer omits metadata that the target cannot apply. That exception is only
  // for absence: a supplied timestamp still claims identity and must match the seal.
  return request === undefined && !capabilities.modificationTime
    ? true
    : sameModifiedTime(request, evidence)
}

function sameModifiedTime(
  left: PreparationFileEntry['modifiedTime'],
  right: PreparationFileEntry['modifiedTime'],
): boolean {
  return left === undefined
    ? right === undefined
    : right !== undefined &&
      left.seconds === right.seconds &&
      left.nanoseconds === right.nanoseconds &&
      left.precision === right.precision
}

function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length &&
    left.every((segment, index) => segment === right[index])
}

function pathKey(path: readonly string[]): string {
  return path.join('/')
}

function abortUnlockedStream(
  stream: WritableStream<Uint8Array<ArrayBuffer>>,
  reason: unknown,
): Promise<void> {
  if (stream.locked) {
    return Promise.reject(new Error('portable output stream cleanup ownership is unknown'))
  }
  try {
    return stream.abort(reason)
  } catch (error) {
    return Promise.reject(error)
  }
}

function once<Args extends readonly unknown[]>(
  callback: (...args: Args) => void,
): (...args: Args) => void {
  let called = false
  return (...args: Args) => {
    if (called) return
    called = true
    callback(...args)
  }
}
