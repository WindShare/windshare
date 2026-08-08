import {
  type BeginOutputFileResult,
  type DirectoryAdmission,
  type DirectoryAdmissionScope,
  type DirectorySettlement,
  JobSettlementKind,
  type JobSettlement,
  type OutputCapabilities,
  type OutputDirectoryAdmission,
  type OutputFile,
  type OutputSession,
  type OutputSessionIdentity,
  COMPLETED_JOB_SETTLEMENT,
  needsAttentionJobSettlement,
  outputSessionIdentity,
  pausedJobSettlement,
} from '../../transfer/output-session'
import type { JobOutcome } from '../../transfer/outcome'
import { FaultScope, OutputFaultCode, outputFault } from '../../transfer/fault'
import { BrowserFileSystemTree } from '../browser/filesystem-tree'
import { ensureOneShotIndexedDbLegacyCleanup } from '../browser/indexeddb-legacy-cleaner'
import { IndexedDbOutputRepository } from '../browser/indexeddb-repository'
import {
  acquireBrowserFileSystemAccessSessionLease,
  type BrowserFileSystemAccessSessionLease,
} from '../browser/session-lease'
import type { CheckpointCrashHook } from '../persistent-tree/contracts'
import { PersistentTreeOutputSession } from '../persistent-tree/session'

const DEFAULT_DATABASE_NAME = 'windshare-output-checkpoints'
const ROOT_HANDLE_ID = 'output-root'
export const FILE_SYSTEM_ACCESS_BACKEND = 'file-system-access'

export interface FileSystemAccessOutputOptions {
  readonly outputSessionId: string
  readonly directoryAdmissionScope: DirectoryAdmissionScope
  /** Stable final intent digest; never use outputSessionId as durable namespace. */
  readonly transferIntentDigest: string
  /** Stable picker-root identity used to bind checkpoints to this capability. */
  readonly rootIdentity: string
  readonly databaseName?: string
  readonly crashHook?: CheckpointCrashHook
}

export type FileSystemAccessInnerSession = OutputSession

export interface FileSystemAccessSessionRepository {
  deleteSessionData(): Promise<void>
  close(): void
}

export interface FileSystemAccessSessionLease {
  release(): Promise<void>
}

type ManagedState =
  | 'open'
  | 'completing'
  | 'completed'
  | 'pausing'
  | 'paused'
  | 'needs-attention'

export class FileSystemAccessOutputSession implements OutputSession {
  readonly identity: OutputSessionIdentity
  readonly format = 'directory' as const
  readonly capabilities: OutputCapabilities

  readonly #inner: FileSystemAccessInnerSession
  readonly #repository: FileSystemAccessSessionRepository
  readonly #lease: FileSystemAccessSessionLease
  #state: ManagedState = 'open'
  #completePromise: Promise<JobSettlement> | undefined
  #pausePromise: Promise<JobSettlement> | undefined
  #resourcePromise: Promise<unknown> | undefined

  constructor(
    inner: FileSystemAccessInnerSession,
    repository: FileSystemAccessSessionRepository,
    lease: FileSystemAccessSessionLease,
  ) {
    this.#inner = inner
    this.#repository = repository
    this.#lease = lease
    this.identity = inner.identity
    this.capabilities = inner.capabilities
  }

  admitDirectory(directory: OutputDirectoryAdmission, signal: AbortSignal): Promise<DirectoryAdmission> {
    this.#requireOpen()
    return this.#inner.admitDirectory(directory, signal)
  }

  finalizeDirectory(
    admission: DirectoryAdmission,
    signal: AbortSignal,
  ): Promise<DirectorySettlement> {
    this.#requireOpen()
    return this.#inner.finalizeDirectory(admission, signal)
  }

  beginFile(file: OutputFile, signal: AbortSignal): Promise<BeginOutputFileResult> {
    this.#requireOpen()
    return this.#inner.beginFile(file, signal)
  }

  completeJob(outcome: JobOutcome, signal: AbortSignal): Promise<JobSettlement> {
    if (this.#state === 'completed') return Promise.resolve(COMPLETED_JOB_SETTLEMENT)
    if (this.#completePromise !== undefined) return this.#completePromise
    if (this.#state !== 'open') {
      return Promise.reject(new Error('File System Access output cannot start completion'))
    }
    this.#state = 'completing'
    const operation = this.#complete(outcome, signal).catch((error: unknown) => {
      if (this.#state === 'completing') this.#state = 'open'
      throw error
    })
    this.#completePromise = operation
    return operation
  }

  pauseJob(reason: unknown): Promise<JobSettlement> {
    if (this.#state === 'completed') return Promise.resolve(COMPLETED_JOB_SETTLEMENT)
    if (this.#pausePromise !== undefined) return this.#pausePromise
    if (this.#state !== 'open') {
      return Promise.resolve(needsAttentionJobSettlement(outputFault(
        FaultScope.OutputPause,
        OutputFaultCode.MutationAmbiguous,
      )))
    }
    this.#state = 'pausing'
    const operation = this.#pause(reason)
    this.#pausePromise = operation
    return operation
  }

  async #complete(outcome: JobOutcome, signal: AbortSignal): Promise<JobSettlement> {
    const inner = await this.#inner.completeJob(outcome, signal)
    if (inner.kind !== JobSettlementKind.Completed) {
      const releaseFailure = await this.#releaseResources()
      this.#state = 'needs-attention'
      return inner.kind === JobSettlementKind.NeedsAttention && releaseFailure === undefined
        ? inner
        : needsAttentionJobSettlement(outputFault(
            FaultScope.OutputPause,
            OutputFaultCode.MutationAmbiguous,
          ))
    }
    let failure: unknown
    try {
      await this.#repository.deleteSessionData()
    } catch (error) {
      failure = error
    }
    const releaseFailure = await this.#releaseResources()
    if (releaseFailure !== undefined) {
      failure = combinedFailure(failure, releaseFailure, 'Output completion and resource release failed')
    }
    this.#state = failure === undefined ? 'completed' : 'needs-attention'
    return failure === undefined
      ? COMPLETED_JOB_SETTLEMENT
      : needsAttentionJobSettlement(outputFault(
          FaultScope.OutputPause,
          OutputFaultCode.MutationAmbiguous,
        ))
  }

  async #pause(reason: unknown): Promise<JobSettlement> {
    let settlement: JobSettlement
    try {
      settlement = await this.#inner.pauseJob(reason)
    } catch {
      settlement = needsAttentionJobSettlement(outputFault(
        FaultScope.OutputPause,
        OutputFaultCode.MutationAmbiguous,
      ))
    }
    const releaseFailure = await this.#releaseResources()
    const stable = settlement.kind === JobSettlementKind.Paused && releaseFailure === undefined
    this.#state = stable ? 'paused' : 'needs-attention'
    return stable
      ? pausedJobSettlement(this.capabilities.durability)
      : needsAttentionJobSettlement(outputFault(
          FaultScope.OutputPause,
          OutputFaultCode.MutationAmbiguous,
        ))
  }

  async #releaseResources(): Promise<unknown> {
    if (this.#resourcePromise !== undefined) return this.#resourcePromise
    const operation = this.#performResourceRelease()
    this.#resourcePromise = operation
    return operation
  }

  async #performResourceRelease(): Promise<unknown> {
    let failure: unknown
    try {
      this.#repository.close()
    } catch (closeError) {
      failure = combinedFailure(failure, closeError, 'Output cleanup and repository close failed')
    }
    try {
      await this.#lease.release()
    } catch (releaseError) {
      failure = combinedFailure(failure, releaseError, 'Output cleanup and lease release failed')
    }
    return failure
  }

  #requireOpen(): void {
    if (this.#state !== 'open') throw new Error('File System Access output session is not open')
  }
}

function combinedFailure(current: unknown, next: unknown, message: string): unknown {
  return current === undefined ? next : new AggregateError([current, next], message)
}

export async function acquireFileSystemAccessOutputSession(
  root: FileSystemDirectoryHandle,
  options: FileSystemAccessOutputOptions,
): Promise<FileSystemAccessOutputSession> {
  const repository = await repositoryFor(options)
  let lease: BrowserFileSystemAccessSessionLease | undefined
  try {
    lease = await acquireBrowserFileSystemAccessSessionLease(repository.binding)
    await bindRootHandle(repository, root)
    return await openWithRoot(root, repository, lease, options)
  } catch (error) {
    repository.close()
    await lease?.release().catch(() => undefined)
    throw error
  }
}

async function openWithRoot(
  root: FileSystemDirectoryHandle,
  repository: IndexedDbOutputRepository,
  lease: BrowserFileSystemAccessSessionLease,
  options: FileSystemAccessOutputOptions,
): Promise<FileSystemAccessOutputSession> {
  const identity = sessionIdentity(options.outputSessionId)
  const tree = BrowserFileSystemTree.forSharedRoot({
    root,
    handles: repository,
    mutations: lease.mutations,
  })
  const inner = await PersistentTreeOutputSession.open({
    identity,
    directoryAdmissionScope: options.directoryAdmissionScope,
    tree,
    journal: repository,
    durability: 'ProcessRestart',
    ...(options.crashHook === undefined ? {} : { crashHook: options.crashHook }),
  })
  return new FileSystemAccessOutputSession(inner, repository, lease)
}

async function bindRootHandle(
  repository: IndexedDbOutputRepository,
  root: FileSystemDirectoryHandle,
): Promise<void> {
  const existing = await repository.getHandle(ROOT_HANDLE_ID)
  if (existing !== undefined) {
    if (existing.kind !== 'directory' || !await root.isSameEntry(existing)) {
      throw new DOMException(
        'The output session identity is already bound to another directory',
        'InvalidModificationError',
      )
    }
  } else {
    await repository.putHandle(ROOT_HANDLE_ID, root)
  }
  const reopened = await repository.getHandle(ROOT_HANDLE_ID)
  if (reopened?.kind !== 'directory' || !await root.isSameEntry(reopened)) {
    throw new DOMException('The output root handle did not persist safely', 'DataError')
  }
}

async function repositoryFor(
  options: FileSystemAccessOutputOptions,
): Promise<IndexedDbOutputRepository> {
  const databaseName = options.databaseName ?? DEFAULT_DATABASE_NAME
  await ensureOneShotIndexedDbLegacyCleanup(databaseName)
  return IndexedDbOutputRepository.open(
    databaseName,
    {
      backend: FILE_SYSTEM_ACCESS_BACKEND,
      transferIntentDigest: options.transferIntentDigest,
      rootIdentity: options.rootIdentity,
    },
  )
}

function sessionIdentity(outputSessionId: string): OutputSessionIdentity {
  return outputSessionIdentity({
    backend: FILE_SYSTEM_ACCESS_BACKEND,
    outputSessionId,
  })
}
