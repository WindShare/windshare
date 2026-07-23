import type { JobOutcome } from '../../transfer/outcome'
import type {
  BeginOutputFileResult,
  FileAbortDisposition,
  OutputCapabilities,
  OutputDirectory,
  OutputFile,
  OutputFileTransaction,
  OutputSession,
  OutputSessionIdentity,
  VerifiedDurableRanges,
} from '../../transfer/output-session'
import { outputSessionIdentity, snapshotOutputFile } from '../../transfer/output-session'
import { BrowserFileSystemTree } from '../browser/filesystem-tree'
import { IndexedDbOutputRepository } from '../browser/indexeddb-repository'
import {
  acquireBrowserOutputSessionLease,
  type BrowserOutputSessionLease,
} from '../browser/session-lease'
import type { CheckpointCrashHook } from '../persistent-tree/contracts'
import {
  PersistentTreeOutputSession,
  type StagedOutputCatalog,
} from '../persistent-tree/session'
import {
  OriginPrivateStagingAdmission,
  type OriginPrivateQuotaOptions,
} from './admission'

const DEFAULT_DATABASE_NAME = 'windshare-output-checkpoints'
const STAGING_ROOT_NAME = '.windshare-receive-staging'
export const ORIGIN_PRIVATE_BACKEND = 'origin-private-staging'

export interface OriginPrivateStorage {
  getDirectory(): Promise<FileSystemDirectoryHandle>
  estimate?(): Promise<{ readonly usage?: number; readonly quota?: number }>
}

export interface OriginPrivateOutputExporter {
  export(
    catalog: StagedOutputCatalog,
    outcome: JobOutcome,
    signal: AbortSignal,
  ): Promise<OriginPrivateExportResult>
  abort?(reason: unknown): Promise<void>
  retryCleanup?(): Promise<OriginPrivateExportResult>
}

export interface OriginPrivateExportResult {
  readonly cleanupPending: boolean
  readonly cleanupFailure?: unknown
}

export interface OriginPrivateFinalization {
  readonly committed: true
  readonly outcome: JobOutcome
  readonly cleanupPending: boolean
  readonly cleanupFailure?: unknown
}

export const ORIGIN_PRIVATE_EXPORT_COMPLETE: OriginPrivateExportResult = Object.freeze({
  cleanupPending: false,
})

export interface OriginPrivateOutputOptions {
  readonly outputSessionId: string
  readonly exporter: OriginPrivateOutputExporter
  readonly storage?: OriginPrivateStorage
  readonly quota?: OriginPrivateQuotaOptions
  readonly databaseName?: string
  readonly crashHook?: CheckpointCrashHook
  readonly retainAfterExport?: boolean
}

type OriginPrivateState =
  | 'open'
  | 'finishing'
  | 'committed'
  | 'cleanup-pending'
  | 'finished'
  | 'finish-failed'
  | 'aborting'
  | 'aborted'
  | 'suspended'

export class OriginPrivateOutputSession implements OutputSession {
  readonly identity: OutputSessionIdentity
  readonly capabilities: OutputCapabilities

  readonly #inner: PersistentTreeOutputSession
  readonly #exporter: OriginPrivateOutputExporter
  readonly #admission: OriginPrivateStagingAdmission
  readonly #settleResources: (removeStaging: boolean) => Promise<void>
  readonly #retainAfterExport: boolean
  #state: OriginPrivateState = 'open'
  #finishController: AbortController | undefined
  #finishPromise: Promise<void> | undefined
  #abortPromise: Promise<void> | undefined
  #cleanupPromise: Promise<OriginPrivateFinalization> | undefined
  #committedOutcome: JobOutcome | undefined
  #exportCleanupPending = false
  #exportCleanupFailure: unknown
  #stagingCleanupPending = false
  #stagingCleanupFailure: unknown

  constructor(
    inner: PersistentTreeOutputSession,
    exporter: OriginPrivateOutputExporter,
    admission: OriginPrivateStagingAdmission,
    settleResources: (removeStaging: boolean) => Promise<void>,
    retainAfterExport: boolean,
  ) {
    this.#inner = inner
    this.#exporter = exporter
    this.#admission = admission
    this.#settleResources = settleResources
    this.#retainAfterExport = retainAfterExport
    this.identity = inner.identity
    this.capabilities = inner.capabilities
  }

  ensureDirectory(directory: OutputDirectory): Promise<void> {
    return this.#inner.ensureDirectory(directory)
  }

  finalizeDirectory(directory: OutputDirectory, signal: AbortSignal): Promise<void> {
    return this.#inner.finalizeDirectory(directory, signal)
  }

  async beginFile(file: OutputFile): Promise<BeginOutputFileResult> {
    const stagedFile = snapshotOutputFile(file)
    const previousFootprint = await this.#inner.stagedFileFootprint(stagedFile.path)
    const rollbackReservation = await this.#admission.reserve(
      stagedFile.path,
      stagedFile.exactSize,
      previousFootprint,
    )
    let begun: BeginOutputFileResult
    try {
      begun = await this.#inner.beginFile(stagedFile)
    } catch (error) {
      await rollbackReservation()
      throw error
    }
    return Object.freeze({
      durableRanges: begun.durableRanges,
      transaction: new QuotaTrackedTransaction(
        begun.transaction,
        stagedFile,
        this.#admission,
      ),
    })
  }

  get finalization(): OriginPrivateFinalization | undefined {
    if (this.#committedOutcome === undefined) return undefined
    const cleanupFailures = [this.#exportCleanupFailure, this.#stagingCleanupFailure]
      .filter((failure) => failure !== undefined)
    const cleanupFailure = cleanupFailures.length < 2
      ? cleanupFailures[0]
      : new AggregateError(cleanupFailures, 'Published output cleanup remains pending')
    return Object.freeze({
      committed: true,
      outcome: this.#committedOutcome,
      cleanupPending: this.#exportCleanupPending || this.#stagingCleanupPending,
      ...(cleanupFailure === undefined ? {} : { cleanupFailure }),
    })
  }

  async finishJob(outcome: JobOutcome, signal: AbortSignal): Promise<void> {
    if (this.#state === 'finished') return
    if (this.#state !== 'open') throw new Error('Origin-private output cannot start finalization')
    signal.throwIfAborted()
    this.#state = 'finishing'
    const controller = new AbortController()
    this.#finishController = controller
    const detach = forwardAbort(signal, controller)
    const operation = this.#finish(outcome, controller.signal).catch((error: unknown) => {
      this.#recordFinishFailure()
      throw error
    })
    this.#finishPromise = operation
    try {
      await operation
    } finally {
      detach()
      this.#finishController = undefined
    }
  }

  async abortJob(reason: unknown = new DOMException('Output job aborted', 'AbortError')): Promise<void> {
    if (this.#state === 'finished') return
    if (this.#state === 'committed') {
      await this.#finishPromise
      return
    }
    if (this.#state === 'cleanup-pending') {
      await this.retryCleanup()
      return
    }
    if (this.#state === 'aborted') return
    if (this.#abortPromise !== undefined) return this.#abortPromise
    this.#state = 'aborting'
    this.#finishController?.abort(reason)
    const operation = this.#abort(reason)
    this.#abortPromise = operation
    try {
      await operation
      this.#markAbortedUnlessFinished()
    } finally {
      this.#abortPromise = undefined
    }
  }

  async #abort(reason: unknown): Promise<void> {
    const failures: unknown[] = []
    let exporterAbortFailure: unknown
    try {
      await this.#exporter.abort?.(reason)
    } catch (error) {
      exporterAbortFailure = error
    }
    let finalizationWon = false
    try {
      await this.#finishPromise
      finalizationWon = this.#finishPromise !== undefined
    } catch {
      // The abort path owns the terminal error once it interrupts finalization.
    }
    if (finalizationWon || this.#state === 'finished') return
    if (exporterAbortFailure !== undefined) failures.push(exporterAbortFailure)
    try {
      await this.#inner.abortJob()
    } catch (error) {
      failures.push(error)
    }
    try {
      await this.#settleResources(true)
    } catch (error) {
      failures.push(error)
    }
    if (failures.length > 0) {
      throw new AggregateError(failures, 'Origin-private output cleanup failed')
    }
  }

  async suspendJob(reason: unknown): Promise<void> {
    if (this.#state !== 'open') return
    this.#state = 'suspended'
    const failures: unknown[] = []
    try {
      await this.#inner.suspendJob()
    } catch (error) {
      failures.push(error)
    }
    try {
      await this.#exporter.abort?.(reason)
    } catch (error) {
      failures.push(error)
    }
    try {
      await this.#settleResources(false)
    } catch (error) {
      failures.push(error)
    }
    if (failures.length > 0) {
      throw new AggregateError(failures, 'Origin-private output suspension failed')
    }
  }

  retryCleanup(): Promise<OriginPrivateFinalization> {
    if (this.#committedOutcome === undefined) {
      return Promise.reject(new Error('Origin-private output has not crossed its publication boundary'))
    }
    if (this.#cleanupPromise !== undefined) return this.#cleanupPromise
    // Deferring the operation installs the shared promise before an exporter or
    // resource callback can reenter cleanup authority.
    const operation = Promise.resolve().then(() => this.#performCleanup()).finally(() => {
      if (this.#cleanupPromise === operation) this.#cleanupPromise = undefined
    })
    this.#cleanupPromise = operation
    return operation
  }

  async #performCleanup(): Promise<OriginPrivateFinalization> {
    if (this.#exportCleanupPending && this.#exporter.retryCleanup !== undefined) {
      try {
        const result = await this.#exporter.retryCleanup()
        this.#exportCleanupPending = result.cleanupPending
        this.#exportCleanupFailure = result.cleanupFailure
      } catch (error) {
        this.#exportCleanupPending = true
        this.#exportCleanupFailure = error
      }
    }
    if (this.#stagingCleanupPending) {
      try {
        await this.#settleResources(!this.#retainAfterExport)
        this.#stagingCleanupPending = false
        this.#stagingCleanupFailure = undefined
      } catch (error) {
        this.#stagingCleanupFailure = error
      }
    }
    this.#state = this.#exportCleanupPending || this.#stagingCleanupPending
      ? 'cleanup-pending'
      : 'finished'
    return this.finalization as OriginPrivateFinalization
  }

  async #finish(outcome: JobOutcome, signal: AbortSignal): Promise<void> {
    await this.#inner.finishJob(outcome, signal)
    signal.throwIfAborted()
    const exportResult = await this.#exporter.export(this.#inner.stagedCatalog(), outcome, signal)
    // Export close is the irreversible publication boundary. Cancellation after
    // this point may delay cleanup but cannot truthfully change the job outcome.
    this.#state = 'committed'
    this.#committedOutcome = outcome
    this.#exportCleanupPending = exportResult.cleanupPending
    this.#exportCleanupFailure = exportResult.cleanupFailure
    this.#stagingCleanupPending = true
    await this.retryCleanup()
  }

  #recordFinishFailure(): void {
    if (this.#state === 'finishing') this.#state = 'finish-failed'
  }

  #markAbortedUnlessFinished(): void {
    if (this.#state !== 'finished' && this.#state !== 'cleanup-pending') {
      this.#state = 'aborted'
    }
  }
}

function forwardAbort(source: AbortSignal, target: AbortController): () => void {
  const abort = () => target.abort(abortError(source))
  if (source.aborted) {
    abort()
    return () => {}
  }
  source.addEventListener('abort', abort, { once: true })
  return () => source.removeEventListener('abort', abort)
}

function abortError(signal: AbortSignal): unknown {
  return signal.reason ?? new DOMException('Output finalization aborted', 'AbortError')
}

class QuotaTrackedTransaction implements OutputFileTransaction {
  readonly #inner: OutputFileTransaction
  readonly #file: OutputFile
  readonly #admission: OriginPrivateStagingAdmission

  constructor(
    inner: OutputFileTransaction,
    file: OutputFile,
    admission: OriginPrivateStagingAdmission,
  ) {
    this.#inner = inner
    this.#file = file
    this.#admission = admission
  }

  writeRange(offset: bigint, data: Uint8Array): Promise<void> {
    return this.#inner.writeRange(offset, data)
  }

  async checkpoint(): Promise<VerifiedDurableRanges> {
    const ranges = await this.#inner.checkpoint()
    await this.#admission.updateFile(this.#file.path, coveredBytes(ranges))
    return ranges
  }

  async commit(): Promise<void> {
    const ranges = await this.#inner.checkpoint()
    // Admission can fail closed on quota or cross-tab authority changes. Resolve
    // that fallible boundary before the inner commit becomes irreversible.
    await this.#admission.updateFile(this.#file.path, coveredBytes(ranges))
    const admission = await this.#admission.prepareFileCommit(this.#file.path)
    try {
      await this.#inner.commit()
    } catch (error) {
      try {
        await admission.rollback()
      } catch (rollbackError) {
        throw new AggregateError(
          [error, rollbackError],
          'Output commit and admission rollback failed',
          { cause: rollbackError },
        )
      }
      throw error
    }
    admission.publish()
  }

  async abort(reason: unknown): Promise<FileAbortDisposition> {
    const disposition = await this.#inner.abort(reason)
    await this.#admission.releaseFile(this.#file.path)
    return disposition
  }
}

export async function openOriginPrivateOutputSession(
  options: OriginPrivateOutputOptions,
): Promise<OriginPrivateOutputSession> {
  const identity = outputSessionIdentity({
    backend: ORIGIN_PRIVATE_BACKEND,
    outputSessionId: options.outputSessionId,
  })
  const lease = await acquireBrowserOutputSessionLease(identity)
  let repository: IndexedDbOutputRepository | undefined
  let admission: OriginPrivateStagingAdmission | undefined
  try {
    const storage = options.storage ?? defaultStorage()
    const originRoot = await storage.getDirectory()
    const stagingRoot = await originRoot.getDirectoryHandle(STAGING_ROOT_NAME, { create: true })
    const sessionName = await privateSessionName(options.outputSessionId)
    repository = await IndexedDbOutputRepository.open(
      options.databaseName ?? DEFAULT_DATABASE_NAME,
      ORIGIN_PRIVATE_BACKEND,
      options.outputSessionId,
    )
    await convergeCleanup(stagingRoot, sessionName, repository)
    const sessionRoot = await stagingRoot.getDirectoryHandle(sessionName, { create: true })
    const tree = new BrowserFileSystemTree({ root: sessionRoot, handles: repository })
    const inner = await PersistentTreeOutputSession.open({
      identity,
      tree,
      journal: repository,
      durability: 'ProcessRestart',
      ...(options.crashHook === undefined ? {} : { crashHook: options.crashHook }),
    })
    admission = await OriginPrivateStagingAdmission.open(
      `${identity.backend}\0${identity.outputSessionId}`,
      await inner.stagedOutputTotals(),
      options.quota ?? quotaFor(storage),
    )
    const heldRepository = repository
    const heldAdmission = admission
    const settleResources = resourceSettlement(
      stagingRoot,
      sessionName,
      heldRepository,
      heldAdmission,
      lease,
    )
    return new OriginPrivateOutputSession(
      inner,
      options.exporter,
      heldAdmission,
      settleResources,
      options.retainAfterExport ?? false,
    )
  } catch (error) {
    await admission?.release()
    repository?.close()
    await lease.release().catch(() => undefined)
    throw error
  }
}

function resourceSettlement(
  stagingRoot: FileSystemDirectoryHandle,
  sessionName: string,
  repository: IndexedDbOutputRepository,
  admission: OriginPrivateStagingAdmission,
  lease: BrowserOutputSessionLease,
): (removeStaging: boolean) => Promise<void> {
  let tail: Promise<void> = Promise.resolve()
  let released = false
  return (removeStaging) => {
    const operation = tail.then(async () => {
      if (released) return
      if (removeStaging) {
        await repository.markCleanup(sessionName)
        await removeStagingTree(stagingRoot, sessionName)
        await repository.deleteSessionData()
        await repository.clearCleanup()
      }
      await admission.release()
      await lease.release()
      repository.close()
      released = true
    })
    tail = operation.catch(() => undefined)
    return operation
  }
}

async function convergeCleanup(
  stagingRoot: FileSystemDirectoryHandle,
  sessionName: string,
  repository: IndexedDbOutputRepository,
): Promise<void> {
  const target = await repository.cleanupTarget()
  if (target === undefined) return
  if (target !== sessionName) throw new Error('OPFS cleanup marker targets another staging root')
  await removeStagingTree(stagingRoot, target)
  await repository.deleteSessionData()
  await repository.clearCleanup()
}

async function removeStagingTree(
  stagingRoot: FileSystemDirectoryHandle,
  sessionName: string,
): Promise<void> {
  try {
    await stagingRoot.removeEntry(sessionName, { recursive: true })
  } catch (error) {
    if (!errorNamed(error, 'NotFoundError')) throw error
  }
}

function defaultStorage(): OriginPrivateStorage {
  const storage = navigator.storage as StorageManager & Partial<OriginPrivateStorage>
  if (storage.getDirectory === undefined) {
    throw new DOMException('Origin-private file storage is unavailable', 'NotSupportedError')
  }
  return {
    getDirectory: () => storage.getDirectory?.() as Promise<FileSystemDirectoryHandle>,
    estimate: () => storage.estimate(),
  }
}

function quotaFor(storage: OriginPrivateStorage): OriginPrivateQuotaOptions {
  if (storage.estimate === undefined) {
    throw new DOMException('Origin-private quota information is unavailable', 'NotSupportedError')
  }
  return { estimate: () => storage.estimate?.() as ReturnType<NonNullable<typeof storage.estimate>> }
}

async function privateSessionName(outputSessionId: string): Promise<string> {
  const digest = await crypto.subtle.digest(
    'SHA-256',
    new TextEncoder().encode(outputSessionId),
  )
  return `session-${[...new Uint8Array(digest)]
    .map((value) => value.toString(16).padStart(2, '0'))
    .join('')}`
}

function errorNamed(error: unknown, name: string): boolean {
  return typeof error === 'object' && error !== null &&
    'name' in error && (error as { readonly name?: unknown }).name === name
}

function coveredBytes(ranges: VerifiedDurableRanges): bigint {
  return ranges.ranges.reduce((total, range) => total + range.end - range.start, 0n)
}
