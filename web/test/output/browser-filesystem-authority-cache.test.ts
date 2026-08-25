import { describe, expect, it } from 'vitest'

import { BrowserFileLineageAuthority } from '../../src/output/browser/filesystem-file-lineage'
import { fileHandleId } from '../../src/output/browser/filesystem-file-record'
import {
  FSAAuthorityCache,
  type FSAVerifiedDirectoryAuthority,
} from '../../src/output/browser/mutation-coordination/authority-cache'
import { createFSAOperationMutationScheduler } from '../../src/output/browser/mutation-coordination/scheduler'
import type { FSAParentMutationIdentity } from '../../src/output/browser/mutation-coordination/model'
import type { PersistedFSAOperationBinding } from '../../src/output/browser/indexeddb-root-binding'
import type { FSARootMutationAuthority } from '../../src/output/browser/namespace-mutation'
import type {
  PersistentHandleRecord,
  PersistentHandleRepository,
} from '../../src/output/persistence/journal'
import type { ReceiveIntent } from '../../src/transfer/intent'
import { identity } from './planning/fixture'

describe('browser filesystem authority cache', () => {
  it('deduplicates concurrent directory misses and retains only validated results', async () => {
    const fixture = cacheFixture()
    const durableValidation = deferred<void>()
    let resolutions = 0
    const resolve = async () => {
      resolutions += 1
      await durableValidation.promise
      return directoryInstallation(fixture, ['nested'], 'nested-object')
    }

    const first = fixture.cache.resolveDirectory(['nested'], 1, resolve)
    const second = fixture.cache.resolveDirectory(['nested'], 1, resolve)
    expect(fixture.cache.diagnostics()).toMatchObject({
      hits: 1,
      misses: 1,
      retainedDirectories: 0,
      inFlightDirectoryResolutions: 1,
      walkedDepth: 1,
    })
    durableValidation.resolve()

    const [firstAuthority, secondAuthority] = await Promise.all([first, second])
    expect(firstAuthority).toBe(secondAuthority)
    expect(resolutions).toBe(1)
    expect(fixture.cache.directory(['nested'])).toBe(firstAuthority)
  })

  it('invalidates only the affected materialization subtree', () => {
    const fixture = cacheFixture()
    const first = fixture.cache.installDirectory(directoryInstallation(fixture, ['first'], 'first-object'))
    const nested = fixture.cache.installDirectory(
      directoryInstallation(fixture, ['first', 'nested'], 'nested-object'),
    )
    const sibling = fixture.cache.installDirectory(directoryInstallation(fixture, ['sibling'], 'sibling-object'))
    fixture.cache.installFile(fileInstallation(first, ['first', 'one.bin'], 'owned-one'))
    fixture.cache.installFile(fileInstallation(nested, ['first', 'nested', 'two.bin'], 'owned-two'))
    fixture.cache.installFile(fileInstallation(sibling, ['sibling', 'three.bin'], 'owned-three'))

    fixture.cache.invalidateSubtree(['first'])

    expect(fixture.cache.directory(['first'])).toBeUndefined()
    expect(fixture.cache.directory(['first', 'nested'])).toBeUndefined()
    expect(fixture.cache.file(['first', 'one.bin'], 'owned-one')).toBeUndefined()
    expect(fixture.cache.file(['first', 'nested', 'two.bin'], 'owned-two')).toBeUndefined()
    expect(fixture.cache.directory(['sibling'])).toBe(sibling)
    expect(fixture.cache.file(['sibling', 'three.bin'], 'owned-three')?.parent).toBe(sibling)
    expect(fixture.cache.diagnostics().invalidations).toBe(1)
  })

  it('rejects an in-flight result invalidated by a compatible-name remap', async () => {
    const fixture = cacheFixture()
    const validation = deferred<void>()
    const pending = fixture.cache.resolveDirectory(['mapped'], 1, async () => {
      await validation.promise
      return directoryInstallation(fixture, ['mapped'], 'mapped-object')
    })
    fixture.cache.invalidateSubtree(['mapped'])
    validation.resolve()

    await expect(pending).rejects.toMatchObject({ name: 'InvalidStateError' })
    expect(fixture.cache.directory(['mapped'])).toBeUndefined()
  })

  it('clears retained handles and rejects later admission after close', () => {
    const fixture = cacheFixture()
    fixture.cache.installDirectory(directoryInstallation(fixture, ['nested'], 'nested-object'))
    fixture.cache.close()
    fixture.cache.close()

    expect(fixture.cache.diagnostics()).toMatchObject({
      closed: true,
      retainedPickedParent: false,
      retainedDirectories: 0,
      retainedFiles: 0,
      invalidations: 1,
    })
    expect(() => fixture.cache.pickedParent()).toThrowError(/authority cache is closed/i)
  })

  it('fails closed after operation authority loss', () => {
    const fixture = cacheFixture()
    fixture.cache.installDirectory(directoryInstallation(fixture, ['nested'], 'nested-object'))
    fixture.cache.invalidateOperation()

    expect(fixture.cache.diagnostics()).toMatchObject({
      operationInvalidated: true,
      retainedDirectories: 0,
      invalidations: 1,
    })
    expect(() => fixture.cache.pickedParent()).toThrowError(/operation authority was invalidated/i)
    expect(() => fixture.cache.installDirectory(
      directoryInstallation(fixture, ['later'], 'later-object'),
    )).toThrowError(/operation authority was invalidated/i)
  })

  it('does not alias same-named picked parents from different root leases', () => {
    const left = cacheFixture('downloads')
    const right = cacheFixture('downloads')

    expect(left.cache.pickedParent().physicalName).toBe(right.cache.pickedParent().physicalName)
    expect(left.cache.pickedParent().schedulerIdentity)
      .not.toBe(right.cache.pickedParent().schedulerIdentity)
  })

  it('reuses the persisted file handle while retaining a fresh leaf check at every open', async () => {
    const fixture = cacheFixture()
    const fileHandle = new ObservedFileHandle('payload.bin')
    const repository = new MemoryHandleRepository()
    const ownedObjectId = identity(91, 32)
    const handleId = fileHandleId(fixture.binding.intent.operationId, ownedObjectId)
    repository.records.set(handleId, {
      id: handleId,
      operationId: fixture.binding.intent.operationId,
      kind: 1,
      authorityRef: fixture.binding.reservation.authorityRef,
      ownedObjectId,
      handle: fileHandle as unknown as FileSystemFileHandle,
    })
    const parent = fixture.cache.pickedParent()
    const directory = fixture.binding.parent as unknown as ObservedDirectoryHandle
    directory.file = fileHandle
    const mutations = schedulerAuthority(fixture.rootIdentity)
    const lineage = new BrowserFileLineageAuthority({
      binding: fixture.binding,
      fileHandles: repository,
      mutations,
      authorities: fixture.cache,
      prepareRoot: async () => undefined,
      resolveParent: async () => ({ authority: parent, name: 'payload.bin' }),
    })

    expect(await lineage.open([], ownedObjectId)).toBeDefined()
    expect(await lineage.open([], ownedObjectId)).toBeDefined()

    expect(repository.reads).toBe(1)
    // The first cache fill and both reopen boundaries each authenticate the physical leaf.
    expect(directory.fileOpens).toBe(3)
    expect(fileHandle.sameEntryChecks).toBe(3)
  })

  it('admits a created file only after the atomic handle/checkpoint callback settles', async () => {
    const fixture = cacheFixture()
    const repository = new MemoryHandleRepository()
    const parent = fixture.cache.pickedParent()
    const directory = fixture.binding.parent as unknown as ObservedDirectoryHandle
    const mutations = schedulerAuthority(fixture.rootIdentity)
    const lineage = new BrowserFileLineageAuthority({
      binding: fixture.binding,
      fileHandles: repository,
      mutations,
      authorities: fixture.cache,
      prepareRoot: async () => undefined,
      resolveParent: async () => ({ authority: parent, name: 'payload.bin' }),
    })
    const ownedObjectId = identity(92, 32)
    const commitStarted = deferred<void>()
    const durableCommit = deferred<void>()
    let receipt: PersistentHandleRecord<unknown> | undefined

    const creation = lineage.createAfterRevisionOpen(
      [],
      { fileId: identity(93), fileRevision: identity(94), exactSize: 1n },
      ownedObjectId,
      undefined,
      async handle => {
        receipt = handle
        commitStarted.resolve()
        await durableCommit.promise
      },
    )
    await commitStarted.promise

    expect(receipt?.handle).toBe(directory.file)
    expect(repository.puts).toBe(0)
    expect(fixture.cache.file([], ownedObjectId)).toBeUndefined()
    durableCommit.resolve()

    const file = await creation
    expect(file.persistedHandle).toBe(receipt)
    expect(fixture.cache.file([], ownedObjectId)?.handle).toBe(directory.file)
    await mutations.scheduler.close()
  })

  it('fails closed without caching when the atomic created-file callback rejects', async () => {
    const fixture = cacheFixture()
    const repository = new MemoryHandleRepository()
    const parent = fixture.cache.pickedParent()
    const mutations = schedulerAuthority(fixture.rootIdentity)
    const lineage = new BrowserFileLineageAuthority({
      binding: fixture.binding,
      fileHandles: repository,
      mutations,
      authorities: fixture.cache,
      prepareRoot: async () => undefined,
      resolveParent: async () => ({ authority: parent, name: 'payload.bin' }),
    })
    const ownedObjectId = identity(95, 32)
    const failure = new DOMException('atomic commit failed', 'UnknownError')

    await expect(lineage.createAfterRevisionOpen(
      [],
      { fileId: identity(96), fileRevision: identity(97), exactSize: 1n },
      ownedObjectId,
      undefined,
      async () => { throw failure },
    )).rejects.toMatchObject({
      name: 'InvalidStateError',
      reason: 'target-ownership-unknown',
      cause: failure,
    })
    expect(repository.puts).toBe(0)
    expect(fixture.cache.file([], ownedObjectId)).toBeUndefined()
    await mutations.scheduler.close()
  })
})

function cacheFixture(name = 'downloads'): Readonly<{
  cache: FSAAuthorityCache
  binding: PersistedFSAOperationBinding
  rootIdentity: FSAParentMutationIdentity
}> {
  const parent = new ObservedDirectoryHandle(name)
  const binding = {
    intent: { operationId: `authority-operation-${identitySequence++}` } as ReceiveIntent,
    reservation: {
      entryKind: 'single-file',
      operationId: `authority-operation-${identitySequence - 1}`,
      authorityRef: `authority-ref-${identitySequence}`,
      physicalName: 'payload.bin',
    },
    parent: parent as unknown as FileSystemDirectoryHandle,
    parentHandleId: `picked-parent-${identitySequence}`,
  } as PersistedFSAOperationBinding
  const rootIdentity = Symbol(binding.parentHandleId) as FSAParentMutationIdentity
  return Object.freeze({
    cache: new FSAAuthorityCache({ binding, rootParentIdentity: rootIdentity }),
    binding,
    rootIdentity,
  })
}

let identitySequence = 0

function directoryInstallation(
  fixture: ReturnType<typeof cacheFixture>,
  canonicalPath: readonly string[],
  ownedObjectId: string,
) {
  return {
    handleId: `directory-handle:${fixture.binding.intent.operationId}:${canonicalPath.join('/')}`,
    ownedObjectId,
    canonicalPath,
    physicalName: canonicalPath.at(-1) ?? 'root',
    handle: new ObservedDirectoryHandle(canonicalPath.at(-1) ?? 'root') as unknown as FileSystemDirectoryHandle,
  }
}

function fileInstallation(
  parent: FSAVerifiedDirectoryAuthority,
  canonicalPath: readonly string[],
  ownedObjectId: string,
) {
  return {
    handleId: `file-handle:${canonicalPath.join('/')}`,
    ownedObjectId,
    parent,
    canonicalPath,
    physicalName: canonicalPath.at(-1)!,
    handle: new ObservedFileHandle(canonicalPath.at(-1)!) as unknown as FileSystemFileHandle,
  }
}

function schedulerAuthority(rootIdentity: FSAParentMutationIdentity): FSARootMutationAuthority {
  const scheduler = createFSAOperationMutationScheduler({
    rootParent: rootIdentity,
    maximumActiveWriters: 2,
  })
  return Object.freeze({
    scheduler,
    rootParentIdentity: rootIdentity,
    registerAuthorityRelease: () => undefined,
    run: <Value>(_kind: never, operation: () => Promise<Value>) => operation(),
  }) as FSARootMutationAuthority
}

class MemoryHandleRepository implements PersistentHandleRepository {
  readonly records = new Map<string, PersistentHandleRecord>()
  reads = 0
  puts = 0

  async putHandle(record: PersistentHandleRecord): Promise<void> {
    this.puts += 1
    this.records.set(record.id, record)
  }

  async readHandle(id: string): Promise<PersistentHandleRecord | undefined> {
    this.reads += 1
    return this.records.get(id)
  }

  async deleteHandle(id: string): Promise<void> {
    this.records.delete(id)
  }
}

class ObservedDirectoryHandle {
  readonly kind = 'directory' as const
  readonly name: string
  file: ObservedFileHandle | undefined
  fileOpens = 0

  constructor(name: string) {
    this.name = name
  }

  async getFileHandle(
    name?: string,
    options?: FileSystemGetFileOptions,
  ): Promise<FileSystemFileHandle> {
    this.fileOpens += 1
    if (this.file === undefined && options?.create === true) {
      this.file = new ObservedFileHandle(name ?? 'payload.bin')
    }
    if (this.file === undefined) throw new DOMException('missing', 'NotFoundError')
    return this.file as unknown as FileSystemFileHandle
  }

  async getDirectoryHandle(): Promise<FileSystemDirectoryHandle> {
    throw new DOMException('missing', 'NotFoundError')
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return other === this
  }
}

class ObservedFileHandle {
  readonly kind = 'file' as const
  readonly name: string
  sameEntryChecks = 0

  constructor(name: string) {
    this.name = name
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    this.sameEntryChecks += 1
    return other === this
  }
}

interface Deferred<Value> {
  readonly promise: Promise<Value>
  readonly resolve: (value?: Value | PromiseLike<Value>) => void
}

function deferred<Value>(): Deferred<Value> {
  let resolve!: Deferred<Value>['resolve']
  const promise = new Promise<Value>(complete => {
    resolve = value => { complete(value as Value | PromiseLike<Value>) }
  })
  return { promise, resolve }
}
