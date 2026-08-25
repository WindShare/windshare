import type {
  PersistentHandleRecord,
  PersistentHandleRepository,
} from '../persistence/journal'
import type { OpenedFileRevision, PersistentTreeFile } from '../persistent-tree/contracts'
import {
  runPersistentOutputStage,
  type PersistentOutputStageScope,
} from '../persistent-tree/stage-diagnostics'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import type { CompatibleNamePathAuthority } from '../file-system-access/compatible-name/coordinator'
import { snapshotRelativePath } from './filesystem-directory-authority'
import {
  PathComponentRejectedError,
  inspectFileSystemComponent,
} from './filesystem-component-inspection'
import { captureFSAFailureFacts } from './filesystem-failure-facts'
import {
  createOwnedObjectId,
  fileHandleId,
  fileHandleRecord,
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
import type { FSATerminalExclusiveAuthority } from './mutation-coordination/model'
import type {
  FSAAuthorityCache,
  FSAVerifiedDirectoryAuthority,
  FSAVerifiedFileAuthority,
} from './mutation-coordination/authority-cache'

export {
  FSA_FILE_HANDLE_KIND,
  createOwnedObjectId,
  namespaceEntryExists,
  requireOwnedObjectId,
  sameEntry,
} from './filesystem-file-record'

export interface ResolvedFSAFileParent {
  readonly authority: FSAVerifiedDirectoryAuthority
  readonly name: string
}

interface BrowserFileLineageAuthorityOptions {
  readonly binding: PersistedFSAOperationBinding
  readonly fileHandles: PersistentHandleRepository
  readonly mutations: FSARootMutationAuthority
  readonly authorities: FSAAuthorityCache
  readonly prepareRoot: () => Promise<void>
  readonly pairParent?: () => FSAVerifiedDirectoryAuthority | undefined
  readonly compatibleNames?: CompatibleNamePathAuthority
  readonly resolveParent: (
    path: readonly string[],
    stageScope?: PersistentOutputStageScope,
  ) => Promise<ResolvedFSAFileParent>
  readonly randomOwnedObjectId?: () => string
}

/** Owns the durable claim-to-exact-handle boundary for one browser file lineage. */
export class BrowserFileLineageAuthority {
  readonly #binding: PersistedFSAOperationBinding
  readonly #fileHandles: PersistentHandleRepository
  readonly #mutations: FSARootMutationAuthority
  readonly #authorities: FSAAuthorityCache
  readonly #prepareRoot: () => Promise<void>
  readonly #pairParent: () => FSAVerifiedDirectoryAuthority | undefined
  readonly #compatibleNames: CompatibleNamePathAuthority | undefined
  readonly #resolveParent: BrowserFileLineageAuthorityOptions['resolveParent']
  readonly #randomOwnedObjectId: () => string

  constructor(options: BrowserFileLineageAuthorityOptions) {
    this.#binding = options.binding
    this.#fileHandles = options.fileHandles
    this.#mutations = options.mutations
    this.#authorities = options.authorities
    this.#prepareRoot = options.prepareRoot
    this.#pairParent = options.pairParent ?? (() => undefined)
    this.#compatibleNames = options.compatibleNames
    this.#resolveParent = options.resolveParent
    this.#randomOwnedObjectId = options.randomOwnedObjectId ?? createOwnedObjectId
  }

  async proposeOwnedObjectId(path: readonly string[], revision: OpenedFileRevision): Promise<string> {
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
    const { authority: parent, name } = await this.#resolveParent(canonicalPath, stageScope)
    if (this.#compatibleNames?.hasLateLogicalCollision(canonicalPath, 'file')) return 'occupied'
    const outcome = await this.#mutations.scheduler.runNamespace(
      [parent.schedulerIdentity],
      'create-file',
      async () => {
        try {
          return Object.freeze({
            kind: 'inspection' as const,
            value: await runPersistentOutputStage(
              stageScope,
              'fsa.file.entry.inspect',
              () => inspectFileSystemComponent({
                verifiedParent: parent.handle,
                component: name,
                expectedKind: 'file',
                stage: 'fsa.file.entry.inspect',
                mode: this.#compatibleNames?.hasMapping(canonicalPath, 'file') === true
                  ? 'diagnostic'
                  : 'classify-rejection',
              }),
            ),
          })
        } catch (error) {
          if (!(error instanceof PathComponentRejectedError)) throw error
          return Object.freeze({ kind: 'rejected' as const, rejection: error })
        }
      },
    )
    if (outcome.kind === 'inspection') return outcome.value
    const pairParent = this.#pairParent()
    if (this.#compatibleNames === undefined || pairParent === undefined) throw outcome.rejection
    await this.#compatibleNames.resolveRejectedComponent({
      rejection: outcome.rejection,
      artifactPath: canonicalPath,
      entryKind: 'file',
      parent: parent.handle,
      parentAuthority: parent,
      pairParent: pairParent.handle,
      pairParentAuthority: pairParent,
    })
    return 'absent'
  }

  async createAfterRevisionOpen(
    path: readonly string[],
    revision: OpenedFileRevision,
    selectedOwnedObjectId: string,
    stageScope?: PersistentOutputStageScope,
    commitCreatedFile?: (handle: PersistentHandleRecord<unknown>) => Promise<void>,
  ): Promise<PersistentTreeFile> {
    requireOpenedRevision(revision)
    const canonicalPath = this.#filePath(path)
    const ownedObjectId = requireOwnedObjectId(selectedOwnedObjectId)
    this.#addFailureFacts(stageScope, canonicalPath, ownedObjectId)
    await this.#prepareRoot()
    const { authority: parent, name } = await this.#resolveParent(canonicalPath, stageScope)
    try {
      return await this.#mutations.scheduler.runNamespace(
        [parent.schedulerIdentity],
        'create-file',
        async () => {
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
            () => inspectFileSystemComponent({
              verifiedParent: parent.handle,
              component: name,
              expectedKind: 'file',
              stage: 'fsa.file.entry.inspect',
              mode: 'diagnostic',
            }),
          ) === 'occupied') {
            throw new TargetOwnershipUnknownError('namespace-create', this.#binding.intent.operationId)
          }
          return this.#createClaimedFile(
            canonicalPath,
            parent,
            name,
            ownedObjectId,
            stageScope,
            commitCreatedFile,
          )
        },
      )
    } catch (error) {
      this.#authorities.invalidateSubtree(canonicalPath)
      throw error
    }
  }

  async open(
    path: readonly string[],
    ownedObjectId: string,
    stageScope?: PersistentOutputStageScope,
  ): Promise<PersistentTreeFile | undefined> {
    const canonicalPath = this.#filePath(path)
    const objectId = requireOwnedObjectId(ownedObjectId)
    this.#addFailureFacts(stageScope, canonicalPath, objectId)
    const authority = await this.#resolveFileAuthority(canonicalPath, objectId, 'parent-authority', stageScope)
    if (authority === undefined) return undefined
    await this.#verifyFileAuthority(authority, 'writer-open', stageScope)
    return this.#persistentFile(authority, stageScope)
  }

  async remove(path: readonly string[], ownedObjectId: string): Promise<void> {
    const canonicalPath = this.#filePath(path)
    const objectId = requireOwnedObjectId(ownedObjectId)
    const authority = await this.#resolveFileAuthority(canonicalPath, objectId, 'commit')
    if (authority === undefined) {
      throw new TargetOwnershipUnknownError('cleanup', this.#binding.intent.operationId)
    }
    await this.#mutations.scheduler.runNamespace(
      [authority.parent.schedulerIdentity],
      'remove-entry',
      () => this.#removeAuthority(canonicalPath, authority),
    )
  }

  async removeWithinTerminal(
    _terminal: FSATerminalExclusiveAuthority,
    path: readonly string[],
    ownedObjectId: string,
  ): Promise<void> {
    const canonicalPath = this.#filePath(path)
    const authority = await this.#resolveFileAuthority(
      canonicalPath,
      requireOwnedObjectId(ownedObjectId),
      'commit',
    )
    if (authority === undefined) {
      throw new TargetOwnershipUnknownError('cleanup', this.#binding.intent.operationId)
    }
    await this.#removeAuthority(canonicalPath, authority)
  }

  async #removeAuthority(
    canonicalPath: readonly string[],
    authority: FSAVerifiedFileAuthority,
  ): Promise<void> {
    await this.#verifyFileAuthority(authority, 'commit')
    await authority.parent.handle.removeEntry(authority.physicalName)
    try {
      await this.#fileHandles.deleteHandle(authority.handleId)
    } catch (cause) {
      throw new TargetOwnershipUnknownError('cleanup', this.#binding.intent.operationId, { cause })
    }
    this.#authorities.invalidateSubtree(canonicalPath)
  }

  async #openClaimedFile(
    canonicalPath: readonly string[],
    parent: FSAVerifiedDirectoryAuthority,
    name: string,
    ownedObjectId: string,
    existing: PersistentHandleRecord,
    stageScope: PersistentOutputStageScope | undefined,
  ): Promise<PersistentTreeFile> {
    const persistedHandle = requireFileHandle(existing.handle, this.#binding.intent.operationId, 'namespace-create')
    const current = await runPersistentOutputStage(
      stageScope,
      'fsa.file.entry.open',
      () => parent.handle.getFileHandle(name),
    )
    const expected = fileHandleRecord(this.#binding.reservation, ownedObjectId, persistedHandle)
    if (!await sameFileHandleRecord(existing, expected, stageScope) ||
        !await runPersistentOutputStage(
          stageScope,
          'fsa.file.handle.verify',
          () => current.isSameEntry(persistedHandle),
        )) {
      throw new TargetOwnershipUnknownError('namespace-create', this.#binding.intent.operationId)
    }
    return this.#persistentFile(this.#authorities.installFile({
      handleId: expected.id,
      ownedObjectId,
      parent,
      canonicalPath,
      physicalName: name,
      handle: persistedHandle,
    }), stageScope, expected)
  }

  async #createClaimedFile(
    canonicalPath: readonly string[],
    parent: FSAVerifiedDirectoryAuthority,
    name: string,
    ownedObjectId: string,
    stageScope: PersistentOutputStageScope | undefined,
    commitCreatedFile: ((handle: PersistentHandleRecord<unknown>) => Promise<void>) | undefined,
  ): Promise<PersistentTreeFile> {
    const created = await runPersistentOutputStage(
      stageScope,
      'fsa.file.entry.create',
      () => parent.handle.getFileHandle(name, { create: true }),
    )
    await this.#compatibleNames?.recordCompatibleTargetCreated(
      canonicalPath,
      'file',
      this.#pairParent()?.handle,
    )
    const record = fileHandleRecord(this.#binding.reservation, ownedObjectId, created)
    const current = await runPersistentOutputStage(
      stageScope,
      'fsa.file.entry.open',
      () => parent.handle.getFileHandle(name),
    )
    if (!await runPersistentOutputStage(
      stageScope,
      'fsa.file.handle.verify',
      () => current.isSameEntry(created),
    )) {
      throw new TargetOwnershipUnknownError('namespace-create', this.#binding.intent.operationId)
    }
    if (commitCreatedFile === undefined) {
      throw new TargetOwnershipUnknownError(
        'namespace-create',
        this.#binding.intent.operationId,
        { cause: new TypeError('Created FSA handle requires an atomic checkpoint commit callback') },
      )
    }
    try {
      // The callback owns the one transaction that makes this exact handle and
      // its selected checkpoint jointly durable; cache authority follows it.
      await commitCreatedFile(record)
    } catch (cause) {
      throw new TargetOwnershipUnknownError('namespace-create', this.#binding.intent.operationId, { cause })
    }
    return this.#persistentFile(this.#authorities.installFile({
      handleId: record.id,
      ownedObjectId,
      parent,
      canonicalPath,
      physicalName: name,
      handle: created,
    }), stageScope, record)
  }

  #persistentFile(
    authority: FSAVerifiedFileAuthority,
    stageScope: PersistentOutputStageScope | undefined,
    persistedHandle: PersistentHandleRecord<FileSystemFileHandle> = fileHandleRecord(
      this.#binding.reservation,
      authority.ownedObjectId,
      authority.handle,
    ),
  ): PersistentTreeFile {
    return createBrowserPersistentFile({
      authority,
      persistedHandle,
      scheduler: this.#mutations.scheduler,
      verify: stage => this.#verifyFileAuthority(authority, stage, stageScope),
      ...(stageScope === undefined ? {} : { stageScope }),
    })
  }

  async #resolveFileAuthority(
    path: readonly string[],
    ownedObjectId: string,
    stage: FSAFileIdentityStage,
    stageScope?: PersistentOutputStageScope,
  ): Promise<FSAVerifiedFileAuthority | undefined> {
    const cached = this.#authorities.file(path, ownedObjectId)
    if (cached !== undefined) return cached
    const record = await this.#readFileHandle(
      fileHandleId(this.#binding.intent.operationId, ownedObjectId),
      stage,
      stageScope,
      'indexeddb.file-handle.read',
    )
    if (record === undefined) return undefined
    const handle = requireFileHandle(record.handle, this.#binding.intent.operationId, stage)
    if (!recordOwnsFile(record, this.#binding.reservation, ownedObjectId)) {
      throw new TargetOwnershipUnknownError(stage, this.#binding.intent.operationId)
    }
    const { authority: parent, name } = await this.#resolveParent(path, stageScope)
    const current = await runPersistentOutputStage(
      stageScope,
      'fsa.file.entry.open',
      () => parent.handle.getFileHandle(name),
    )
    if (!await runPersistentOutputStage(
      stageScope,
      'fsa.file.handle.verify',
      () => current.isSameEntry(handle),
    )) {
      this.#authorities.invalidateSubtree(path)
      throw new TargetOwnershipUnknownError(stage, this.#binding.intent.operationId)
    }
    return this.#authorities.installFile({
      handleId: record.id,
      ownedObjectId,
      parent,
      canonicalPath: path,
      physicalName: name,
      handle,
    })
  }

  async #verifyFileAuthority(
    authority: FSAVerifiedFileAuthority,
    stage: 'writer-open' | 'checkpoint' | 'commit',
    stageScope?: PersistentOutputStageScope,
  ): Promise<void> {
    try {
      const current = await runPersistentOutputStage(
        stageScope,
        'fsa.file.entry.open',
        () => authority.parent.handle.getFileHandle(authority.physicalName),
      )
      if (await runPersistentOutputStage(
        stageScope,
        'fsa.file.handle.verify',
        () => current.isSameEntry(authority.handle),
      )) return
    } catch (cause) {
      this.#authorities.invalidateSubtree(authority.canonicalPath)
      throw new TargetOwnershipUnknownError(stage, this.#binding.intent.operationId, { cause })
    }
    this.#authorities.invalidateSubtree(authority.canonicalPath)
    throw new TargetOwnershipUnknownError(stage, this.#binding.intent.operationId)
  }

  #addFailureFacts(
    stageScope: PersistentOutputStageScope | undefined,
    path: readonly string[],
    ownedObjectId: string,
  ): void {
    stageScope?.addFailureFacts('fsa', context => captureFSAFailureFacts({
      target: {
        kind: 'named-entry',
        resolve: async () => {
          const resolved = await this.#resolveParent(path)
          return { parent: resolved.authority.handle, name: resolved.name }
        },
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
    const singleFile = this.#binding.reservation.entryKind === 'single-file'
    const canonical = snapshotRelativePath(path, singleFile)
    if (singleFile && canonical.length !== 0) {
      throw new TypeError('Single-file DirectoryTree must write at its materialization root')
    }
    return canonical
  }

  async #readFileHandle(
    id: string,
    stage: FSAFileIdentityStage,
    stageScope: PersistentOutputStageScope | undefined,
    diagnosticStage: 'indexeddb.file-handle.read' | 'indexeddb.file-handle.committed-read',
  ): Promise<PersistentHandleRecord | undefined> {
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
