import type {
  BeginOutputFileResult,
  OutputCapabilities,
  OutputDirectory,
  OutputFile,
  OutputSession,
  OutputSessionIdentity,
} from '../../transfer/output-session'
import type { JobOutcome } from '../../transfer/outcome'
import { outputSessionIdentity } from '../../transfer/output-session'
import { BrowserFileSystemTree } from '../browser/filesystem-tree'
import { IndexedDbOutputRepository } from '../browser/indexeddb-repository'
import {
  acquireBrowserOutputSessionLease,
  type BrowserOutputSessionLease,
} from '../browser/session-lease'
import type { CheckpointCrashHook } from '../persistent-tree/contracts'
import { PersistentTreeOutputSession } from '../persistent-tree/session'

const DEFAULT_DATABASE_NAME = 'windshare-output-checkpoints'
const ROOT_HANDLE_ID = 'output-root'
export const FILE_SYSTEM_ACCESS_BACKEND = 'file-system-access'

export interface FileSystemAccessOutputOptions {
  readonly outputSessionId: string
  readonly databaseName?: string
  readonly crashHook?: CheckpointCrashHook
}

export interface PreparedFileSystemAccessReauthorization {
  /** Invoke directly from the user's click handler; the permission request is synchronous. */
  authorize(): Promise<FileSystemAccessOutputSession>
  /** Releases the prepared session lease when the user cancels the reauthorization UI. */
  discard(): Promise<void>
}

export interface FileSystemAccessInnerSession extends OutputSession {
  suspendJob(reason: unknown): Promise<void>
}

export interface FileSystemAccessSessionRepository {
  deleteSessionData(): Promise<void>
  close(): void
}

export interface FileSystemAccessSessionLease {
  release(): Promise<void>
}

type ManagedState =
  | 'open'
  | 'finishing'
  | 'committed'
  | 'finished'
  | 'finish-failed'
  | 'aborting'
  | 'aborted'
  | 'suspending'
  | 'suspended'

export class FileSystemAccessOutputSession implements OutputSession {
  readonly identity: OutputSessionIdentity
  readonly capabilities: OutputCapabilities

  readonly #inner: FileSystemAccessInnerSession
  readonly #repository: FileSystemAccessSessionRepository
  readonly #lease: FileSystemAccessSessionLease
  #state: ManagedState = 'open'
  #finishController: AbortController | undefined
  #finishPromise: Promise<void> | undefined
  #abortPromise: Promise<void> | undefined
  #suspendPromise: Promise<void> | undefined
  #resourcePromise: Promise<void> | undefined

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

  ensureDirectory(directory: OutputDirectory): Promise<void> {
    this.#requireOpen()
    return this.#inner.ensureDirectory(directory)
  }

  finalizeDirectory(directory: OutputDirectory, signal: AbortSignal): Promise<void> {
    this.#requireOpen()
    return this.#inner.finalizeDirectory(directory, signal)
  }

  beginFile(file: OutputFile): Promise<BeginOutputFileResult> {
    this.#requireOpen()
    return this.#inner.beginFile(file)
  }

  async finishJob(outcome: JobOutcome, signal: AbortSignal): Promise<void> {
    if (this.#state === 'finished') return
    if (this.#state === 'committed') return this.#finishPromise
    if (this.#state !== 'open') throw new Error('File System Access output cannot start finalization')
    signal.throwIfAborted()
    this.#state = 'finishing'
    const controller = new AbortController()
    this.#finishController = controller
    const detach = forwardAbort(signal, controller)
    const operation = this.#finish(outcome, controller.signal).catch((error: unknown) => {
      if (this.#state === 'finishing') this.#state = 'finish-failed'
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

  abortJob(
    reason: unknown = new DOMException('File System Access output aborted', 'AbortError'),
  ): Promise<void> {
    if (this.#state === 'finished' || this.#state === 'aborted') return Promise.resolve()
    if (this.#state === 'committed') return this.#finishPromise ?? Promise.resolve()
    if (this.#state === 'suspending' || this.#state === 'suspended') {
      return this.#suspendPromise ?? Promise.resolve()
    }
    if (this.#abortPromise !== undefined) return this.#abortPromise
    this.#state = 'aborting'
    this.#finishController?.abort(reason)
    const operation = this.#abort(reason).then(() => {
      if (this.#state !== 'committed' && this.#state !== 'finished') this.#state = 'aborted'
    })
    this.#abortPromise = operation
    return operation
  }

  suspendJob(reason: unknown): Promise<void> {
    if (this.#state === 'suspending' || this.#state === 'suspended') {
      return this.#suspendPromise ?? Promise.resolve()
    }
    if (this.#state !== 'open') return Promise.resolve()
    this.#state = 'suspending'
    const operation = this.#suspend(reason).then(
      () => { this.#state = 'suspended' },
      (error: unknown) => {
        this.#state = 'suspended'
        throw error
      },
    )
    this.#suspendPromise = operation
    return operation
  }

  async #finish(outcome: JobOutcome, signal: AbortSignal): Promise<void> {
    await this.#inner.finishJob(outcome, signal)
    // Inner completion is the publication boundary. Cleanup can fail afterward,
    // but cancellation can no longer truthfully delete or relabel committed files.
    this.#state = 'committed'
    let failure: unknown
    try {
      // Once every output is committed, recovery metadata is no longer useful and
      // retaining FSA handles would create an unbounded permission/object leak.
      await this.#repository.deleteSessionData()
    } catch (error) {
      failure = error
    }
    await this.#releaseResources(failure)
    this.#state = 'finished'
  }

  async #abort(reason: unknown): Promise<void> {
    let finishFailure: unknown
    if (this.#finishPromise !== undefined) {
      try {
        await this.#finishPromise
      } catch (error) {
        finishFailure = error
      }
      if (this.#state === 'committed' || this.#state === 'finished') {
        if (finishFailure !== undefined) throw finishFailure
        return
      }
    }
    const failures: unknown[] = []
    try {
      await this.#inner.abortJob(reason)
    } catch (error) {
      failures.push(error)
    }
    if (failures.length === 0) {
      try {
        await this.#repository.deleteSessionData()
      } catch (error) {
        failures.push(error)
      }
    }
    await this.#releaseResources(
      failures.length === 0
        ? undefined
        : new AggregateError(failures, 'File System Access output cleanup failed'),
    )
  }

  async #suspend(reason: unknown): Promise<void> {
    let failure: unknown
    try {
      await this.#inner.suspendJob(reason)
    } catch (error) {
      failure = error
    }
    await this.#releaseResources(failure)
  }

  async #releaseResources(failure: unknown): Promise<void> {
    if (this.#resourcePromise !== undefined) return this.#resourcePromise
    const operation = this.#performResourceRelease(failure)
    this.#resourcePromise = operation
    return operation
  }

  async #performResourceRelease(failure: unknown): Promise<void> {
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
    if (failure !== undefined) throw failure
  }

  #requireOpen(): void {
    if (this.#state !== 'open') throw new Error('File System Access output session is not open')
  }
}

function forwardAbort(source: AbortSignal, target: AbortController): () => void {
  const abort = () => target.abort(
    source.reason ?? new DOMException('File System Access output aborted', 'AbortError'),
  )
  if (source.aborted) {
    abort()
    return () => {}
  }
  source.addEventListener('abort', abort, { once: true })
  return () => source.removeEventListener('abort', abort)
}

function combinedFailure(current: unknown, next: unknown, message: string): unknown {
  return current === undefined ? next : new AggregateError([current, next], message)
}

export async function acquireFileSystemAccessOutputSession(
  root: FileSystemDirectoryHandle,
  options: FileSystemAccessOutputOptions,
): Promise<FileSystemAccessOutputSession> {
  const repository = await repositoryFor(options)
  let lease: BrowserOutputSessionLease | undefined
  try {
    await bindRootHandle(repository, root)
    lease = await acquireBrowserOutputSessionLease(sessionIdentity(options.outputSessionId))
    return await openWithRoot(root, repository, lease, options)
  } catch (error) {
    repository.close()
    await lease?.release().catch(() => undefined)
    throw error
  }
}

export async function prepareFileSystemAccessReauthorization(
  options: FileSystemAccessOutputOptions,
): Promise<PreparedFileSystemAccessReauthorization> {
  const repository = await repositoryFor(options)
  let lease: BrowserOutputSessionLease | undefined
  try {
    const root = await repository.getHandle(ROOT_HANDLE_ID)
    if (root?.kind !== 'directory') {
      throw new DOMException('The persisted output directory handle is unavailable', 'NotFoundError')
    }
    const directory = root as FileSystemDirectoryHandle
    lease = await acquireBrowserOutputSessionLease(sessionIdentity(options.outputSessionId))
    const heldLease = lease
    let consumed = false
    return Object.freeze({
      authorize: () => {
        if (consumed) return Promise.reject(new Error('Output reauthorization was already consumed'))
        consumed = true
        // Permission is requested before the first await to preserve transient activation.
        const permission = requestWritePermission(directory)
        return permission
          .then(() => openWithRoot(directory, repository, heldLease, options))
          .catch(async (error: unknown) => {
            repository.close()
            await heldLease.release().catch(() => undefined)
            throw error
          })
      },
      discard: async () => {
        if (consumed) return
        consumed = true
        repository.close()
        await heldLease.release()
      },
    })
  } catch (error) {
    repository.close()
    await lease?.release().catch(() => undefined)
    throw error
  }
}

export async function discardFileSystemAccessOutputSession(
  options: FileSystemAccessOutputOptions,
): Promise<void> {
  const identity = sessionIdentity(options.outputSessionId)
  const repository = await repositoryFor(options)
  let lease: BrowserOutputSessionLease | undefined
  try {
    lease = await acquireBrowserOutputSessionLease(identity)
    await repository.deleteSessionData()
  } finally {
    repository.close()
    await lease?.release()
  }
}

async function openWithRoot(
  root: FileSystemDirectoryHandle,
  repository: IndexedDbOutputRepository,
  lease: BrowserOutputSessionLease,
  options: FileSystemAccessOutputOptions,
): Promise<FileSystemAccessOutputSession> {
  const identity = sessionIdentity(options.outputSessionId)
  const tree = new BrowserFileSystemTree({
    root,
    handles: repository,
  })
  const inner = await PersistentTreeOutputSession.open({
    identity,
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

function repositoryFor(
  options: FileSystemAccessOutputOptions,
): Promise<IndexedDbOutputRepository> {
  return IndexedDbOutputRepository.open(
    options.databaseName ?? DEFAULT_DATABASE_NAME,
    FILE_SYSTEM_ACCESS_BACKEND,
    options.outputSessionId,
  )
}

function sessionIdentity(outputSessionId: string): OutputSessionIdentity {
  return outputSessionIdentity({
    backend: FILE_SYSTEM_ACCESS_BACKEND,
    outputSessionId,
  })
}

function requestWritePermission(root: FileSystemDirectoryHandle): Promise<void> {
  const permissionRoot = root as FileSystemDirectoryHandle & {
    requestPermission?: (
      descriptor?: { readonly mode?: 'read' | 'readwrite' },
    ) => Promise<PermissionState>
  }
  if (permissionRoot.requestPermission === undefined) return Promise.resolve()
  const requested = permissionRoot.requestPermission({ mode: 'readwrite' })
  return requested.then((permission) => {
    if (permission !== 'granted') {
      throw new DOMException('Output permission was not granted', 'NotAllowedError')
    }
  })
}
