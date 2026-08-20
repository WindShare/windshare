import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import type {
  PersistentHandleRecord,
  PersistentHandleRepository,
} from '../persistence/journal'
import type {
  OpenedFileRevision,
  PersistentTreeFile,
} from '../persistent-tree/contracts'
import {
  runPersistentOutputStage,
  type PersistentOutputStageScope,
} from '../persistent-tree/stage-diagnostics'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import { captureFSAFailureFacts } from './filesystem-failure-facts'
import {
  createOwnedObjectId,
  fileHandleId,
  fileHandleRecord,
  namespaceEntryExists,
  recordOwnsFile,
  requireFileHandle,
  requireOpenedRevision,
  requireOwnedObjectId,
  sameFileHandleRecord,
  type FSAFileIdentityStage,
} from './filesystem-file-record'
import { createBrowserPersistentFile } from './filesystem-persistent-file'
import type { PersistedFSAOperationBinding } from './indexeddb-root-binding'
import type { FSARootMutationAuthority } from './namespace-mutation'

export {
  FSA_FILE_HANDLE_KIND,
  createOwnedObjectId,
  namespaceEntryExists,
  requireOwnedObjectId,
  sameEntry,
} from './filesystem-file-record'

interface BrowserFileLineageAuthorityOptions {
  readonly binding: PersistedFSAOperationBinding
  readonly fileHandles: PersistentHandleRepository
  readonly mutations: FSARootMutationAuthority
  readonly prepareRoot: () => Promise<void>
  readonly verifyParent: (stageScope?: PersistentOutputStageScope) =>
    Promise<FileSystemDirectoryHandle>
  readonly resolveParent: (
    path: readonly string[],
    stageScope?: PersistentOutputStageScope,
  ) => Promise<Readonly<{
    parent: FileSystemDirectoryHandle
    name: string
  }>>
  readonly randomOwnedObjectId?: () => string
}

/**
 * Owns the claim-before-create boundary between one durable object ID and one
 * browser file entry. Directory traversal remains encapsulated by the tree facade.
 */
export class BrowserFileLineageAuthority {
  readonly #binding: PersistedFSAOperationBinding
  readonly #fileHandles: PersistentHandleRepository
  readonly #mutations: FSARootMutationAuthority
  readonly #prepareRoot: () => Promise<void>
  readonly #verifyParent: BrowserFileLineageAuthorityOptions['verifyParent']
  readonly #resolveParent: BrowserFileLineageAuthorityOptions['resolveParent']
  readonly #randomOwnedObjectId: () => string

  constructor(options: BrowserFileLineageAuthorityOptions) {
    this.#binding = options.binding
    this.#fileHandles = options.fileHandles
    this.#mutations = options.mutations
    this.#prepareRoot = options.prepareRoot
    this.#verifyParent = options.verifyParent
    this.#resolveParent = options.resolveParent
    this.#randomOwnedObjectId = options.randomOwnedObjectId ?? createOwnedObjectId
  }

  async proposeOwnedObjectId(
    path: readonly string[],
    revision: OpenedFileRevision,
  ): Promise<string> {
    this.#filePath(path)
    requireOpenedRevision(revision)
    return requireOwnedObjectId(this.#randomOwnedObjectId())
  }

  async inspectDestination(
    path: readonly string[],
    selectedOwnedObjectId: string,
    stageScope?: PersistentOutputStageScope,
  ): Promise<'absent' | 'occupied'> {
    const canonicalPath = this.#filePath(path)
    requireOwnedObjectId(selectedOwnedObjectId)
    this.#addFailureFacts(stageScope, canonicalPath, selectedOwnedObjectId)
    await this.#prepareRoot()
    return this.#mutations.run('create-file', async () => {
      await this.#verifyParent(stageScope)
      const { parent, name } = await this.#resolveParent(canonicalPath, stageScope)
      return await runPersistentOutputStage(
        stageScope,
        'fsa.file.entry.inspect',
        () => namespaceEntryExists(parent, name),
      ) ? 'occupied' : 'absent'
    })
  }

  async createAfterRevisionOpen(
    path: readonly string[],
    revision: OpenedFileRevision,
    selectedOwnedObjectId: string,
    stageScope?: PersistentOutputStageScope,
  ): Promise<PersistentTreeFile> {
    requireOpenedRevision(revision)
    const canonicalPath = this.#filePath(path)
    const ownedObjectId = requireOwnedObjectId(selectedOwnedObjectId)
    this.#addFailureFacts(stageScope, canonicalPath, ownedObjectId)
    await this.#prepareRoot()
    return this.#mutations.run('create-file', async () => {
      await this.#verifyParent(stageScope)
      const { parent, name } = await this.#resolveParent(canonicalPath, stageScope)
      const handleId = fileHandleId(this.#binding.intent.operationId, ownedObjectId)
      const existing = await this.#readFileHandle(
        handleId,
        'namespace-create',
        stageScope,
        'indexeddb.file-handle.read',
      )
      if (existing !== undefined) {
        return this.#openClaimedFile(
          canonicalPath,
          parent,
          name,
          ownedObjectId,
          existing,
          stageScope,
        )
      }
      if (await runPersistentOutputStage(
        stageScope,
        'fsa.file.entry.inspect',
        () => namespaceEntryExists(parent, name),
      )) {
        throw new TargetOwnershipUnknownError(
          'namespace-create',
          this.#binding.intent.operationId,
        )
      }
      return this.#createClaimedFile(
        canonicalPath,
        parent,
        name,
        ownedObjectId,
        stageScope,
      )
    })
  }

  async open(
    path: readonly string[],
    ownedObjectId: string,
    stageScope?: PersistentOutputStageScope,
  ): Promise<PersistentTreeFile | undefined> {
    const canonicalPath = this.#filePath(path)
    const objectId = requireOwnedObjectId(ownedObjectId)
    this.#addFailureFacts(stageScope, canonicalPath, objectId)
    const record = await this.#readFileHandle(
      fileHandleId(this.#binding.intent.operationId, objectId),
      'parent-authority',
      stageScope,
      'indexeddb.file-handle.read',
    )
    if (record === undefined) return undefined
    const handle = requireFileHandle(
      record.handle,
      this.#binding.intent.operationId,
      'parent-authority',
    )
    if (!recordOwnsFile(record, this.#binding.reservation, objectId)) {
      throw new TargetOwnershipUnknownError(
        'parent-authority',
        this.#binding.intent.operationId,
      )
    }
    await this.#verifyFile(canonicalPath, objectId, 'writer-open', stageScope)
    return this.#persistentFile(canonicalPath, objectId, handle, stageScope)
  }

  async remove(path: readonly string[], ownedObjectId: string): Promise<void> {
    const canonicalPath = this.#filePath(path)
    const objectId = requireOwnedObjectId(ownedObjectId)
    await this.#mutations.run('remove-entry', async () => {
      await this.#verifyFile(canonicalPath, objectId, 'commit')
      const { parent, name } = await this.#resolveParent(canonicalPath)
      await parent.removeEntry(name)
      try {
        await this.#fileHandles.deleteHandle(fileHandleId(
          this.#binding.intent.operationId,
          objectId,
        ))
      } catch (cause) {
        throw new TargetOwnershipUnknownError(
          'cleanup',
          this.#binding.intent.operationId,
          { cause },
        )
      }
    })
  }

  async #openClaimedFile(
    canonicalPath: readonly string[],
    parent: FileSystemDirectoryHandle,
    name: string,
    ownedObjectId: string,
    existing: PersistentHandleRecord,
    stageScope: PersistentOutputStageScope | undefined,
  ): Promise<PersistentTreeFile> {
    const persistedHandle = requireFileHandle(
      existing.handle,
      this.#binding.intent.operationId,
      'namespace-create',
    )
    const current = await runPersistentOutputStage(
      stageScope,
      'fsa.file.entry.open',
      () => parent.getFileHandle(name),
    )
    const expected = fileHandleRecord(
      this.#binding.reservation,
      ownedObjectId,
      persistedHandle,
    )
    if (!await sameFileHandleRecord(existing, expected, stageScope) ||
        !await runPersistentOutputStage(
          stageScope,
          'fsa.file.handle.verify',
          () => current.isSameEntry(persistedHandle),
        )) {
      throw new TargetOwnershipUnknownError(
        'namespace-create',
        this.#binding.intent.operationId,
      )
    }
    return this.#persistentFile(canonicalPath, ownedObjectId, persistedHandle, stageScope)
  }

  async #createClaimedFile(
    canonicalPath: readonly string[],
    parent: FileSystemDirectoryHandle,
    name: string,
    ownedObjectId: string,
    stageScope: PersistentOutputStageScope | undefined,
  ): Promise<PersistentTreeFile> {
    const created = await runPersistentOutputStage(
      stageScope,
      'fsa.file.entry.create',
      () => parent.getFileHandle(name, { create: true }),
    )
    const record = fileHandleRecord(this.#binding.reservation, ownedObjectId, created)
    try {
      await runPersistentOutputStage(
        stageScope,
        'indexeddb.file-handle.persist',
        () => this.#fileHandles.putHandle(record),
      )
    } catch (cause) {
      throw new TargetOwnershipUnknownError(
        'namespace-create',
        this.#binding.intent.operationId,
        { cause },
      )
    }
    const persisted = await this.#readFileHandle(
      record.id,
      'namespace-create',
      stageScope,
      'indexeddb.file-handle.committed-read',
    )
    const current = await runPersistentOutputStage(
      stageScope,
      'fsa.file.entry.open',
      () => parent.getFileHandle(name),
    )
    if (!await sameFileHandleRecord(persisted, record, stageScope) ||
        !await runPersistentOutputStage(
          stageScope,
          'fsa.file.handle.verify',
          () => current.isSameEntry(created),
        )) {
      throw new TargetOwnershipUnknownError(
        'namespace-create',
        this.#binding.intent.operationId,
      )
    }
    return this.#persistentFile(canonicalPath, ownedObjectId, created, stageScope)
  }

  #persistentFile(
    canonicalPath: readonly string[],
    ownedObjectId: string,
    handle: FileSystemFileHandle,
    stageScope: PersistentOutputStageScope | undefined,
  ): PersistentTreeFile {
    return createBrowserPersistentFile({
      ownedObjectId,
      handle,
      verify: stage => this.#verifyFile(
        canonicalPath,
        ownedObjectId,
        stage,
        stageScope,
      ),
      mutate: (kind, operation) => this.#mutations.run(kind, operation),
      ...(stageScope === undefined ? {} : { stageScope }),
    })
  }

  async #verifyFile(
    path: readonly string[],
    ownedObjectId: string,
    stage: 'writer-open' | 'checkpoint' | 'commit',
    stageScope?: PersistentOutputStageScope,
  ): Promise<void> {
    await this.#verifyParent(stageScope)
    const record = await this.#readFileHandle(
      fileHandleId(this.#binding.intent.operationId, ownedObjectId),
      stage,
      stageScope,
      'indexeddb.file-handle.read',
    )
    const persistedHandle = record === undefined
      ? undefined
      : requireFileHandle(record.handle, this.#binding.intent.operationId, stage)
    if (record === undefined ||
        !recordOwnsFile(record, this.#binding.reservation, ownedObjectId) ||
        persistedHandle === undefined) {
      throw new TargetOwnershipUnknownError(stage, this.#binding.intent.operationId)
    }
    const { parent, name } = await this.#resolveParent(path, stageScope)
    const current = await runPersistentOutputStage(
      stageScope,
      'fsa.file.entry.open',
      () => parent.getFileHandle(name),
    )
    if (!await runPersistentOutputStage(
      stageScope,
      'fsa.file.handle.verify',
      () => current.isSameEntry(persistedHandle),
    )) {
      throw new TargetOwnershipUnknownError(stage, this.#binding.intent.operationId)
    }
  }

  #addFailureFacts(
    stageScope: PersistentOutputStageScope | undefined,
    path: readonly string[],
    ownedObjectId: string,
  ): void {
    stageScope?.addFailureFacts('fsa', context => captureFSAFailureFacts({
      target: {
        kind: 'named-entry',
        resolve: () => this.#resolveParent(path),
      },
      permissionFallback: this.#binding.parent,
      expectedKind: 'file',
      readPersistedHandle: async () => {
        const record = await this.#fileHandles.readHandle(fileHandleId(
          this.#binding.intent.operationId,
          ownedObjectId,
        ))
        if (record === undefined) return undefined
        return recordOwnsFile(record, this.#binding.reservation, ownedObjectId)
          ? record.handle
          : Object.freeze({})
      },
      writer: factsContext => stageScope.writerFacts(factsContext),
    }, context))
  }

  #filePath(path: readonly string[]): readonly string[] {
    const canonical = snapshotPortableCatalogPath(path)
    if (this.#binding.reservation.entryKind === 'single-file' &&
        (canonical.length !== 1 ||
         canonical[0] !== this.#binding.reservation.reservedName)) {
      throw new TypeError('Single-file DirectoryTree must write directly below its parent')
    }
    return canonical
  }

  async #readFileHandle(
    id: string,
    stage: FSAFileIdentityStage,
    stageScope: PersistentOutputStageScope | undefined,
    diagnosticStage: 'indexeddb.file-handle.read' | 'indexeddb.file-handle.committed-read',
  ) {
    try {
      return await runPersistentOutputStage(
        stageScope,
        diagnosticStage,
        () => this.#fileHandles.readHandle(id),
      )
    } catch (cause) {
      throw new TargetOwnershipUnknownError(stage, this.#binding.intent.operationId, { cause })
    }
  }
}
