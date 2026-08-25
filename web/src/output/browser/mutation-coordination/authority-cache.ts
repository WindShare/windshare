import type { MaterializationRootRelativePath } from '../../../transfer/job/coordinate/direct-tree'
import { snapshotMaterializationRootRelativePath } from '../../../transfer/job/coordinate/direct-tree'
import type { PersistedFSAOperationBinding } from '../indexeddb-root-binding'
import type { FSARootMutationAuthority } from '../namespace-mutation'
import type { FSAParentMutationIdentity } from './model'
import {
  observePerformance,
  type PerformanceSummaryObservations,
} from '../../diagnostics/performance-summary'

const ROOT_AUTHORITY_CACHES = new WeakMap<object, Map<string, FSAAuthorityCache>>()

export interface FSAVerifiedDirectoryAuthority {
  readonly operationId: string
  readonly handleId: string
  readonly ownedObjectId: string | undefined
  readonly canonicalPath: MaterializationRootRelativePath | undefined
  readonly physicalName: string
  readonly handle: FileSystemDirectoryHandle
  readonly schedulerIdentity: FSAParentMutationIdentity
}

export interface FSAVerifiedFileAuthority {
  readonly operationId: string
  readonly handleId: string
  readonly ownedObjectId: string
  readonly parent: FSAVerifiedDirectoryAuthority
  readonly canonicalPath: MaterializationRootRelativePath
  readonly physicalName: string
  readonly handle: FileSystemFileHandle
}

export interface FSAAuthorityCacheDiagnosticsSnapshot {
  readonly hits: number
  readonly misses: number
  readonly invalidations: number
  readonly walkedDepth: number
  readonly retainedPickedParent: boolean
  readonly retainedDirectories: number
  readonly retainedFiles: number
  readonly inFlightDirectoryResolutions: number
  readonly operationInvalidated: boolean
  readonly closed: boolean
}

export interface VerifiedDirectoryInstallation {
  readonly handleId: string
  readonly ownedObjectId: string
  readonly canonicalPath: readonly string[]
  readonly physicalName: string
  readonly handle: FileSystemDirectoryHandle
}

export interface VerifiedFileInstallation {
  readonly handleId: string
  readonly ownedObjectId: string
  readonly parent: FSAVerifiedDirectoryAuthority
  readonly canonicalPath: readonly string[]
  readonly physicalName: string
  readonly handle: FileSystemFileHandle
}

/**
 * Retained handles become scheduling authority only after the caller completes the durable
 * ownership check. Resolution callbacks therefore return verified installations, never raw handles.
 */
export class FSAAuthorityCache {
  readonly #operationId: string
  readonly #performance: PerformanceSummaryObservations | undefined
  #parent: FSAVerifiedDirectoryAuthority | undefined
  readonly #directories = new Map<string, FSAVerifiedDirectoryAuthority>()
  readonly #files = new Map<string, FSAVerifiedFileAuthority>()
  readonly #filePathOwners = new Map<string, string>()
  readonly #directoryIdentities = new Map<string, FSAParentMutationIdentity>()
  readonly #inFlightDirectories = new Map<string, Promise<FSAVerifiedDirectoryAuthority>>()
  #hits = 0
  #misses = 0
  #invalidations = 0
  #walkedDepth = 0
  #generation = 0
  #operationInvalidated = false
  #closed = false

  constructor(input: Readonly<{
    binding: PersistedFSAOperationBinding
    rootParentIdentity: FSAParentMutationIdentity
    performance?: PerformanceSummaryObservations
  }>) {
    this.#operationId = input.binding.intent.operationId
    this.#performance = input.performance
    this.#parent = Object.freeze({
      operationId: this.#operationId,
      handleId: input.binding.parentHandleId,
      ownedObjectId: undefined,
      canonicalPath: undefined,
      physicalName: input.binding.parent.name,
      handle: input.binding.parent,
      schedulerIdentity: input.rootParentIdentity,
    })
  }

  pickedParent(): FSAVerifiedDirectoryAuthority {
    this.#assertOpen()
    this.#hits += 1
    this.#observeLookup('directory', 'hit')
    const parent = this.#parent
    if (parent === undefined) throw new DOMException('FSA picked-parent authority was cleared', 'InvalidStateError')
    return parent
  }

  assertRootBinding(
    binding: PersistedFSAOperationBinding,
    rootParentIdentity: FSAParentMutationIdentity,
  ): void {
    const parent = this.#parent
    if (binding.intent.operationId !== this.#operationId ||
        parent === undefined ||
        binding.parentHandleId !== parent.handleId ||
        rootParentIdentity !== parent.schedulerIdentity) {
      throw new DOMException('FSA root authority cache binding changed', 'InvalidStateError')
    }
  }

  directory(pathInput: readonly string[]): FSAVerifiedDirectoryAuthority | undefined {
    this.#assertOpen()
    const path = snapshotMaterializationRootRelativePath(pathInput)
    const found = this.#directories.get(pathKey(path))
    if (found === undefined) this.#misses += 1
    else this.#hits += 1
    this.#observeLookup('directory', found === undefined ? 'miss' : 'hit')
    return found
  }

  async resolveDirectory(
    pathInput: readonly string[],
    walkedDepth: number,
    resolve: () => Promise<VerifiedDirectoryInstallation>,
  ): Promise<FSAVerifiedDirectoryAuthority> {
    this.#assertOpen()
    const path = snapshotMaterializationRootRelativePath(pathInput)
    const key = pathKey(path)
    const found = this.#directories.get(key)
    if (found !== undefined) {
      this.#hits += 1
      this.#observeLookup('directory', 'hit')
      return found
    }
    const inFlight = this.#inFlightDirectories.get(key)
    if (inFlight !== undefined) {
      this.#hits += 1
      this.#observeLookup('directory', 'hit')
      return inFlight
    }
    this.#misses += 1
    const validatedWalkedDepth = requireWalkedDepth(walkedDepth)
    this.#walkedDepth += validatedWalkedDepth
    this.#observeLookup('directory', 'miss', validatedWalkedDepth)
    const generation = this.#generation
    const pending = (async () => {
      const installation = await resolve()
      if (generation !== this.#generation) {
        throw new DOMException('FSA directory authority was invalidated while resolving', 'InvalidStateError')
      }
      return this.installDirectory(installation)
    })()
    this.#inFlightDirectories.set(key, pending)
    try {
      return await pending
    } finally {
      if (this.#inFlightDirectories.get(key) === pending) this.#inFlightDirectories.delete(key)
    }
  }

  installDirectory(input: VerifiedDirectoryInstallation): FSAVerifiedDirectoryAuthority {
    this.#assertOpen()
    const path = snapshotMaterializationRootRelativePath(input.canonicalPath)
    const key = pathKey(path)
    const existing = this.#directories.get(key)
    if (existing !== undefined) {
      if (existing.handleId !== input.handleId || existing.ownedObjectId !== input.ownedObjectId) {
        this.invalidateSubtree(path)
        throw new DOMException('FSA directory authority identity changed', 'InvalidStateError')
      } else {
        return existing
      }
    }
    const identityKey = `${input.handleId}\0${input.ownedObjectId}`
    let schedulerIdentity = this.#directoryIdentities.get(identityKey)
    if (schedulerIdentity === undefined) {
      schedulerIdentity = Symbol(identityKey) as FSAParentMutationIdentity
      this.#directoryIdentities.set(identityKey, schedulerIdentity)
    }
    const authority = Object.freeze({
      operationId: this.#operationId,
      handleId: requireIdentity(input.handleId, 'directory handle ID'),
      ownedObjectId: requireIdentity(input.ownedObjectId, 'directory owned object ID'),
      canonicalPath: path,
      physicalName: requireIdentity(input.physicalName, 'directory physical name'),
      handle: requireDirectoryHandle(input.handle),
      schedulerIdentity,
    })
    this.#directories.set(key, authority)
    return authority
  }

  file(
    pathInput: readonly string[],
    ownedObjectId: string,
  ): FSAVerifiedFileAuthority | undefined {
    this.#assertOpen()
    const path = snapshotMaterializationRootRelativePath(pathInput)
    const found = this.#files.get(fileKey(path, ownedObjectId))
    if (found === undefined) this.#misses += 1
    else this.#hits += 1
    this.#observeLookup('file', found === undefined ? 'miss' : 'hit')
    return found
  }

  installFile(input: VerifiedFileInstallation): FSAVerifiedFileAuthority {
    this.#assertOpen()
    const path = snapshotMaterializationRootRelativePath(input.canonicalPath)
    this.#requireOperation(input.parent)
    const key = fileKey(path, input.ownedObjectId)
    const pathOwner = this.#filePathOwners.get(pathKey(path))
    if (pathOwner !== undefined && pathOwner !== key) {
      this.invalidateSubtree(path)
      throw new DOMException('FSA file path authority changed ownership', 'InvalidStateError')
    }
    const existing = this.#files.get(key)
    if (existing !== undefined) {
      if (existing.handleId !== input.handleId ||
          existing.parent.schedulerIdentity !== input.parent.schedulerIdentity) {
        this.invalidateSubtree(path)
        throw new DOMException('FSA file authority identity changed', 'InvalidStateError')
      } else {
        return existing
      }
    }
    const authority = Object.freeze({
      operationId: this.#operationId,
      handleId: requireIdentity(input.handleId, 'file handle ID'),
      ownedObjectId: requireIdentity(input.ownedObjectId, 'file owned object ID'),
      parent: input.parent,
      canonicalPath: path,
      physicalName: requireIdentity(input.physicalName, 'file physical name'),
      handle: requireFileHandle(input.handle),
    })
    this.#files.set(key, authority)
    this.#filePathOwners.set(pathKey(path), key)
    return authority
  }

  invalidateSubtree(pathInput: readonly string[]): void {
    if (this.#closed || this.#operationInvalidated) return
    const path = snapshotMaterializationRootRelativePath(pathInput)
    this.#invalidations += 1
    observePerformance(this.#performance, summary => summary.observeAuthorityInvalidation())
    this.#generation += 1
    for (const [key, authority] of this.#directories) {
      if (authority.canonicalPath !== undefined && isPathWithin(authority.canonicalPath, path)) {
        this.#directories.delete(key)
      }
    }
    for (const [key, authority] of this.#files) {
      if (isPathWithin(authority.canonicalPath, path)) {
        this.#files.delete(key)
        this.#filePathOwners.delete(pathKey(authority.canonicalPath))
      }
    }
    for (const [key] of this.#inFlightDirectories) {
      if (isPathWithin(JSON.parse(key) as readonly string[], path)) {
        this.#inFlightDirectories.delete(key)
      }
    }
  }

  invalidateOperation(): void {
    if (this.#closed || this.#operationInvalidated) return
    this.#invalidations += 1
    observePerformance(this.#performance, summary => summary.observeAuthorityInvalidation())
    this.#generation += 1
    this.#operationInvalidated = true
    this.#parent = undefined
    this.#directories.clear()
    this.#files.clear()
    this.#filePathOwners.clear()
    this.#inFlightDirectories.clear()
  }

  close(): void {
    if (this.#closed) return
    this.invalidateOperation()
    this.#closed = true
  }

  diagnostics(): FSAAuthorityCacheDiagnosticsSnapshot {
    return Object.freeze({
      hits: this.#hits,
      misses: this.#misses,
      invalidations: this.#invalidations,
      walkedDepth: this.#walkedDepth,
      retainedPickedParent: this.#parent !== undefined,
      retainedDirectories: this.#directories.size,
      retainedFiles: this.#files.size,
      inFlightDirectoryResolutions: this.#inFlightDirectories.size,
      operationInvalidated: this.#operationInvalidated,
      closed: this.#closed,
    })
  }

  #requireOperation(authority: FSAVerifiedDirectoryAuthority): void {
    if (authority.operationId !== this.#operationId) {
      throw new TypeError('FSA authority escaped its receive operation')
    }
  }

  #assertOpen(): void {
    if (this.#closed) {
      throw new DOMException('The File System Access authority cache is closed', 'InvalidStateError')
    }
    if (this.#operationInvalidated) {
      throw new DOMException('The File System Access operation authority was invalidated', 'InvalidStateError')
    }
  }

  #observeLookup(
    kind: 'directory' | 'file',
    result: 'hit' | 'miss',
    walkDepth?: number,
  ): void {
    observePerformance(this.#performance, summary => summary.observeAuthorityLookup({
      kind,
      result,
      ...(walkDepth === undefined ? {} : { walkDepth }),
    }))
  }
}

/** One root-lease composition owns one canonical cache even when tree and repair facades open separately. */
export function fsaAuthorityCacheForRoot(input: Readonly<{
  owner: FSARootMutationAuthority
  binding: PersistedFSAOperationBinding
  rootParentIdentity: FSAParentMutationIdentity
}>): FSAAuthorityCache {
  let operationCaches = ROOT_AUTHORITY_CACHES.get(input.owner)
  if (operationCaches === undefined) {
    operationCaches = new Map()
    ROOT_AUTHORITY_CACHES.set(input.owner, operationCaches)
  }
  const operationId = input.binding.intent.operationId
  const existing = operationCaches.get(operationId)
  if (existing !== undefined) {
    existing.assertRootBinding(input.binding, input.rootParentIdentity)
    return existing
  }
  const created = new FSAAuthorityCache({
    binding: input.binding,
    rootParentIdentity: input.rootParentIdentity,
    ...(input.owner.performance === undefined ? {} : { performance: input.owner.performance }),
  })
  operationCaches.set(operationId, created)
  input.owner.registerAuthorityRelease(() => created.close())
  return created
}

function pathKey(path: readonly string[]): string {
  return JSON.stringify(path)
}

function fileKey(path: readonly string[], ownedObjectId: string): string {
  return `${pathKey(path)}\0${ownedObjectId}`
}

function isPathWithin(candidate: readonly string[], ancestor: readonly string[]): boolean {
  return ancestor.length <= candidate.length && ancestor.every((segment, index) => candidate[index] === segment)
}

function requireIdentity(value: string, label: string): string {
  if (typeof value !== 'string' || value.length === 0) throw new TypeError(`FSA ${label} is empty`)
  return value
}

function requireWalkedDepth(value: number): number {
  if (!Number.isSafeInteger(value) || value < 0) throw new TypeError('FSA walked depth is invalid')
  return value
}

function requireDirectoryHandle(handle: FileSystemDirectoryHandle): FileSystemDirectoryHandle {
  if (handle.kind !== 'directory') throw new TypeError('FSA directory authority requires a directory handle')
  return handle
}

function requireFileHandle(handle: FileSystemFileHandle): FileSystemFileHandle {
  if (handle.kind !== 'file') throw new TypeError('FSA file authority requires a file handle')
  return handle
}
