import type { CompatibleNamePathAuthority } from '../file-system-access/compatible-name/coordinator'
import type {
  PersistentHandleRecord,
  PersistentHandleRepository,
} from '../persistence/journal'
import type {
  OpenedFileRevision,
  PersistentDirectoryMaterialization,
  PersistentOutputTree,
  PersistentTreeFile,
} from '../persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import {
  runPersistentOutputStage,
  type PersistentOutputStageAuthority,
  type PersistentOutputStageScope,
} from '../persistent-tree/stage-diagnostics'
import {
  BrowserFileLineageAuthority,
  createOwnedObjectId,
  namespaceEntryExists,
  requireOwnedObjectId,
} from './filesystem-file-lineage'
import { captureFSAFailureFacts } from './filesystem-failure-facts'
import {
  fsaDirectoryHandleId,
  admitFSAOwnedDirectory,
  openDirectoryEntry,
  snapshotRelativePath,
  verifySameDirectory,
} from './filesystem-directory-authority'
import {
  FSA_OPERATION_HANDLE_DIRECTORY,
  persistFSAOwnedDirectory,
  readFSAOwnedDirectoryBinding,
  verifyFSAOperationBinding,
  type FSAOperationBindingRepository,
  type PersistedFSAOperationBinding,
} from './indexeddb-root-binding'
import type {
  FSARootMutationAuthority,
  FSANamespaceMutationKind,
} from './namespace-mutation'
import type { FSATerminalExclusiveAuthority } from './mutation-coordination/model'
import {
  fsaAuthorityCacheForRoot,
  type FSAAuthorityCache,
  type FSAVerifiedDirectoryAuthority,
} from './mutation-coordination/authority-cache'

export { FSA_FILE_HANDLE_KIND } from './filesystem-file-lineage'
export { fsaDirectoryHandleId } from './filesystem-directory-authority'

const READ_WRITE_PERMISSION = Object.freeze({ mode: 'readwrite' as const })

interface PermissionCapableDirectoryHandle extends FileSystemDirectoryHandle {
  queryPermission?(descriptor?: { readonly mode?: 'read' | 'readwrite' }): Promise<PermissionState>
  requestPermission?(descriptor?: { readonly mode?: 'read' | 'readwrite' }): Promise<PermissionState>
}

export interface BrowserFileSystemTreeOptions {
  readonly binding: PersistedFSAOperationBinding
  readonly operationRepository: FSAOperationBindingRepository
  readonly fileHandles: PersistentHandleRepository
  readonly mutations: FSARootMutationAuthority
  readonly compatibleNames?: CompatibleNamePathAuthority
  readonly stageAuthority?: PersistentOutputStageAuthority
  readonly randomOwnedObjectId?: () => string
}

export class BrowserFileSystemTree implements PersistentOutputTree {
  readonly #binding: PersistedFSAOperationBinding
  readonly #operationRepository: FSAOperationBindingRepository
  readonly #mutations: FSARootMutationAuthority
  readonly #authorities: FSAAuthorityCache
  readonly #stageAuthority: PersistentOutputStageAuthority | undefined
  readonly #compatibleNames: CompatibleNamePathAuthority | undefined
  readonly #randomOwnedObjectId: () => string
  readonly #files: BrowserFileLineageAuthority
  #root: FileSystemDirectoryHandle | undefined
  #rootOwnedObjectId: string | undefined

  constructor(options: BrowserFileSystemTreeOptions) {
    this.#binding = options.binding
    this.#operationRepository = options.operationRepository
    this.#mutations = options.mutations
    this.#authorities = fsaAuthorityCacheForRoot({
      owner: options.mutations,
      binding: options.binding,
      rootParentIdentity: options.mutations.rootParentIdentity,
    })
    this.#stageAuthority = options.stageAuthority
    this.#compatibleNames = options.compatibleNames
    this.#randomOwnedObjectId = options.randomOwnedObjectId ?? createOwnedObjectId
    this.#files = new BrowserFileLineageAuthority({
      binding: options.binding,
      fileHandles: options.fileHandles,
      mutations: options.mutations,
      authorities: this.#authorities,
      prepareRoot: () => this.#requirePreparedRoot(),
      resolveParent: (path, scope) => this.#parent(path, scope),
      pairParent: () => this.#authorities.directory([]),
      ...(this.#compatibleNames === undefined ? {} : { compatibleNames: this.#compatibleNames }),
      randomOwnedObjectId: this.#randomOwnedObjectId,
    })
  }

  async authorize(): Promise<void> {
    const scope = this.#rootScope()
    const parent = this.#authorities.pickedParent().handle as PermissionCapableDirectoryHandle
    if (parent.queryPermission === undefined) return
    try {
      if (await runPersistentOutputStage(
        scope,
        'fsa.root.permission.query',
        () => parent.queryPermission!(READ_WRITE_PERMISSION),
      ) === 'granted') return
      if (parent.requestPermission === undefined ||
          await runPersistentOutputStage(
            scope,
            'fsa.root.permission.request',
            () => parent.requestPermission!(READ_WRITE_PERMISSION),
          ) !== 'granted') {
        throw new DOMException('Output permission was not granted', 'NotAllowedError')
      }
      await this.#verifyParent(scope)
    } catch (error) {
      this.#authorities.invalidateOperation()
      throw error
    }
  }

  async prepareRoot(): Promise<void> {
    const scope = this.#rootScope()
    await this.authorize()
    if (this.#binding.reservation.entryKind === 'single-file') {
      await this.#compatibleNames?.ensurePairReady(undefined)
      return
    }
    if (this.#compatibleNames?.pairPlacement === 'beside-mapped-root') {
      await this.#compatibleNames.ensurePairReady(undefined)
    }
    const pickedParent = this.#authorities.pickedParent()
    await this.#mutations.scheduler.runNamespace([pickedParent.schedulerIdentity], 'create-directory', async () => {
      const parent = pickedParent.handle
      const handleId = await fsaDirectoryHandleId(this.#binding.reservation, [])
      const persisted = await readFSAOwnedDirectoryBinding({
        repository: this.#operationRepository,
        reservation: this.#binding.reservation,
        handleId,
        diagnosticTarget: 'root',
        ...(scope === undefined ? {} : { stageScope: scope }),
      })
      if (persisted !== undefined) {
        const current = await openDirectoryEntry(
          parent,
          this.#binding.reservation.physicalName,
          this.#binding.intent.operationId,
          scope,
          'fsa.root.entry.open',
        )
        if (!await verifySameDirectory(
          current,
          persisted.handle,
          this.#binding.intent.operationId,
          scope,
          'fsa.root.handle.verify',
        )) {
          throw new TargetOwnershipUnknownError(
            'parent-authority',
            this.#binding.intent.operationId,
          )
        }
        this.#root = current
        this.#rootOwnedObjectId = persisted.ownedObjectId
        this.#authorities.installDirectory({
          handleId: persisted.handleId,
          ownedObjectId: persisted.ownedObjectId,
          canonicalPath: [],
          physicalName: this.#binding.reservation.physicalName,
          handle: current,
        })
        return
      }
      if (await runPersistentOutputStage(
        scope,
        'fsa.root.entry.inspect',
        () => namespaceEntryExists(parent, this.#binding.reservation.physicalName),
      )) {
        throw new TargetOwnershipUnknownError(
          'namespace-create',
          this.#binding.intent.operationId,
        )
      }
      const created = await runPersistentOutputStage(
        scope,
        'fsa.root.entry.create',
        () => parent.getDirectoryHandle(
          this.#binding.reservation.physicalName,
          { create: true },
        ),
      )
      await this.#compatibleNames?.recordCompatibleTargetCreated(
        Object.freeze([]),
        'directory',
        created,
      )
      const ownedObjectId = requireOwnedObjectId(this.#randomOwnedObjectId())
      const ownedScope = scope?.withCorrelation({ ownedObjectId })
      const committedBinding = await persistFSAOwnedDirectory({
        repository: this.#operationRepository,
        reservation: this.#binding.reservation,
        handleId,
        ownedObjectId,
        handle: created,
        diagnosticTarget: 'root',
        ...(ownedScope === undefined ? {} : { stageScope: ownedScope }),
      })
      const current = await openDirectoryEntry(
        parent,
        this.#binding.reservation.physicalName,
        this.#binding.intent.operationId,
        ownedScope,
        'fsa.root.entry.open',
      )
      if (!await verifySameDirectory(
        current,
        created,
        this.#binding.intent.operationId,
        ownedScope,
        'fsa.root.handle.verify',
      )) {
        throw new TargetOwnershipUnknownError(
          'namespace-create',
          this.#binding.intent.operationId,
        )
      }
      this.#root = current
      this.#rootOwnedObjectId = ownedObjectId
      this.#authorities.installDirectory({
        ...committedBinding,
        canonicalPath: [],
        physicalName: this.#binding.reservation.physicalName,
        handle: current,
      })
    })
    await this.#compatibleNames?.ensurePairReady(this.#root)
  }

  async reopenOwnedRootForCleanup(): Promise<void> {
    const scope = this.#rootScope()
    await this.authorize()
    if (this.#binding.reservation.entryKind === 'single-file') {
      await this.#compatibleNames?.ensurePairReady(undefined)
      return
    }
    await this.#mutate('settle-operation', async () => {
      const parent = await this.#verifyParent(scope)
      const handleId = await fsaDirectoryHandleId(this.#binding.reservation, [])
      const persisted = await readFSAOwnedDirectoryBinding({
        repository: this.#operationRepository,
        reservation: this.#binding.reservation,
        handleId,
        diagnosticTarget: 'root',
        ...(scope === undefined ? {} : { stageScope: scope }),
      })
      if (persisted === undefined) {
        throw new TargetOwnershipUnknownError('cleanup', this.#binding.intent.operationId)
      }
      const current = await openDirectoryEntry(
        parent,
        this.#binding.reservation.physicalName,
        this.#binding.intent.operationId,
        scope,
        'fsa.root.entry.open',
      )
      if (!await verifySameDirectory(
        current,
        persisted.handle,
        this.#binding.intent.operationId,
        scope,
        'fsa.root.handle.verify',
      )) {
        throw new TargetOwnershipUnknownError(
          'parent-authority',
          this.#binding.intent.operationId,
        )
      }
      this.#root = current
      this.#rootOwnedObjectId = persisted.ownedObjectId
      this.#authorities.installDirectory({
        ...persisted,
        canonicalPath: [],
        physicalName: this.#binding.reservation.physicalName,
        handle: current,
      })
    })
    await this.#compatibleNames?.ensurePairReady(this.#root)
  }

  async ensureDirectory(path: readonly string[]): Promise<PersistentDirectoryMaterialization> {
    if (this.#binding.reservation.entryKind === 'single-file') {
      throw new TypeError('Single-file DirectoryTree cannot materialize a directory')
    }
    const canonicalPath = snapshotRelativePath(path, true)
    await this.#requirePreparedRoot()
    const retained = this.#retainedDirectoryMaterialization(canonicalPath)
    if (retained !== undefined) return retained
    const scope = this.#directoryScope(canonicalPath)
    while (true) {
      const parent = await this.#directoryAuthority(canonicalPath.slice(0, -1), undefined, true)
      const name = this.#compatibleNames?.physicalComponent(canonicalPath, 'directory') ??
        canonicalPath.at(-1)!
      const handleId = await fsaDirectoryHandleId(
        this.#binding.reservation,
        canonicalPath,
      )
      try {
        const outcome = await admitFSAOwnedDirectory({
          scheduler: this.#mutations.scheduler,
          authorities: this.#authorities,
          repository: this.#operationRepository,
          reservation: this.#binding.reservation,
          operationId: this.#binding.intent.operationId,
          canonicalPath,
          parent,
          name,
          handleId,
          ...(scope === undefined ? {} : { stageScope: scope }),
          ...(this.#compatibleNames === undefined
            ? {}
            : { compatibleNames: this.#compatibleNames }),
          recordCompatibleTargetCreated: () =>
            this.#recordCompatibleDirectoryTargetCreated(canonicalPath),
          randomOwnedObjectId: this.#randomOwnedObjectId,
        })
        if (outcome.kind === 'materialized') return outcome.value
        const pairParent = this.#authorities.directory([])
        if (this.#compatibleNames === undefined || pairParent === undefined) {
          throw outcome.rejection
        }
        await this.#compatibleNames.resolveRejectedComponent({
          rejection: outcome.rejection,
          artifactPath: canonicalPath,
          entryKind: 'directory',
          parent: parent.handle,
          parentAuthority: parent,
          pairParent: pairParent.handle,
          pairParentAuthority: pairParent,
        })
      } catch (error) {
        this.#authorities.invalidateSubtree(canonicalPath)
        throw error
      }
    }
  }

  #retainedDirectoryMaterialization(
    canonicalPath: readonly string[],
  ): PersistentDirectoryMaterialization | undefined {
    const authority = this.#authorities.directory(canonicalPath)
    if (canonicalPath.length === 0 &&
        (this.#rootOwnedObjectId === undefined ||
          authority?.ownedObjectId !== this.#rootOwnedObjectId)) {
      throw new TargetOwnershipUnknownError(
        'parent-authority',
        this.#binding.intent.operationId,
      )
    }
    return authority?.ownedObjectId === undefined
      ? undefined
      : Object.freeze({ ownedObjectId: authority.ownedObjectId, created: false })
  }

  async #recordCompatibleDirectoryTargetCreated(canonicalPath: readonly string[]): Promise<void> {
    if (this.#compatibleNames === undefined || this.#root === undefined) return
    await this.#compatibleNames.recordCompatibleTargetCreated(
      canonicalPath,
      'directory',
      this.#root,
    )
  }

  async validateDirectory(path: readonly string[], ownedObjectId: string): Promise<boolean> {
    if (this.#binding.reservation.entryKind === 'single-file') return false
    const canonicalPath = snapshotRelativePath(path, true)
    const scope = canonicalPath.length === 0
      ? this.#rootScope()?.withCorrelation({ ownedObjectId })
      : this.#directoryScope(canonicalPath)?.withCorrelation({ ownedObjectId })
    const handleId = await fsaDirectoryHandleId(this.#binding.reservation, canonicalPath)
    const persisted = await readFSAOwnedDirectoryBinding({
      repository: this.#operationRepository,
      reservation: this.#binding.reservation,
      handleId,
      ownedObjectId,
      diagnosticTarget: canonicalPath.length === 0 ? 'root' : 'directory',
      ...(scope === undefined ? {} : { stageScope: scope }),
    })
    if (persisted === undefined) return false
    try {
      const current = canonicalPath.length === 0
        ? await openDirectoryEntry(
            await this.#verifyParent(scope),
            this.#binding.reservation.physicalName,
            this.#binding.intent.operationId,
            scope,
            'fsa.root.entry.open',
          )
        : (await this.#directoryAuthority(canonicalPath, scope, true)).handle
      const verified = await verifySameDirectory(
        current,
        persisted.handle,
        this.#binding.intent.operationId,
        scope,
        canonicalPath.length === 0
          ? 'fsa.root.handle.verify'
          : 'fsa.directory.handle.verify',
      )
      if (!verified) this.#authorities.invalidateSubtree(canonicalPath)
      return verified
    } catch (error) {
      this.#authorities.invalidateSubtree(canonicalPath)
      throw error
    }
  }

  proposeFileOwnedObjectId(
    path: readonly string[],
    revision: OpenedFileRevision,
  ): Promise<string> {
    return this.#files.proposeOwnedObjectId(path, revision)
  }

  inspectFileDestination(
    path: readonly string[],
    selectedOwnedObjectId: string,
    stageScope?: PersistentOutputStageScope,
  ): Promise<'absent' | 'occupied'> {
    return this.#files.inspectDestination(path, selectedOwnedObjectId, stageScope)
  }

  createFileAfterRevisionOpen(
    path: readonly string[],
    revision: OpenedFileRevision,
    selectedOwnedObjectId: string,
    stageScope?: PersistentOutputStageScope,
    commitCreatedFile?: (handle: PersistentHandleRecord<unknown>) => Promise<void>,
  ): Promise<PersistentTreeFile> {
    return this.#files.createAfterRevisionOpen(
      path,
      revision,
      selectedOwnedObjectId,
      stageScope,
      commitCreatedFile,
    )
  }

  openFile(
    path: readonly string[],
    ownedObjectId: string,
    stageScope?: PersistentOutputStageScope,
  ): Promise<PersistentTreeFile | undefined> {
    return this.#files.open(path, ownedObjectId, stageScope)
  }

  removeFile(path: readonly string[], ownedObjectId: string): Promise<void> {
    return this.#files.remove(path, ownedObjectId)
  }

  removeFileWithinTerminal(
    authority: FSATerminalExclusiveAuthority,
    path: readonly string[],
    ownedObjectId: string,
  ): Promise<void> {
    return this.#files.removeWithinTerminal(authority, path, ownedObjectId)
  }

  async removeDirectory(path: readonly string[], ownedObjectId: string): Promise<void> {
    if (this.#binding.reservation.entryKind === 'single-file') {
      throw new TypeError('Single-file DirectoryTree has no owned directory')
    }
    const canonicalPath = snapshotRelativePath(path, true)
    if (!await this.validateDirectory(canonicalPath, ownedObjectId)) {
      throw new TargetOwnershipUnknownError('cleanup', this.#binding.intent.operationId)
    }
    await this.#mutate('remove-entry', () => this.#removeDirectoryAuthority(canonicalPath))
  }

  async removeDirectoryWithinTerminal(
    _authority: FSATerminalExclusiveAuthority,
    path: readonly string[],
    ownedObjectId: string,
  ): Promise<void> {
    if (this.#binding.reservation.entryKind === 'single-file') {
      throw new TypeError('Single-file DirectoryTree has no owned directory')
    }
    const canonicalPath = snapshotRelativePath(path, true)
    if (!await this.validateDirectory(canonicalPath, ownedObjectId)) {
      throw new TargetOwnershipUnknownError('cleanup', this.#binding.intent.operationId)
    }
    await this.#removeDirectoryAuthority(canonicalPath)
  }

  physicalChild(
    parentPath: readonly string[],
    physicalComponent: string,
    entryKind: 'file' | 'directory',
  ) {
    return this.#compatibleNames?.physicalChild(parentPath, physicalComponent, entryKind) ??
      Object.freeze({ kind: 'logical' as const, logicalComponent: physicalComponent })
  }

  ownedRoot(): FileSystemDirectoryHandle | undefined {
    return this.#root
  }

  async #removeDirectoryAuthority(canonicalPath: readonly string[]): Promise<void> {
    if (canonicalPath.length === 0) {
      const parent = await this.#verifyParent()
      await parent.removeEntry(this.#binding.reservation.physicalName, { recursive: true })
      this.#authorities.invalidateSubtree([])
      return
    }
    const { authority: parent, name } = await this.#parent(canonicalPath, undefined, 'directory')
    await parent.handle.removeEntry(name, { recursive: true })
    this.#authorities.invalidateSubtree(canonicalPath)
  }

  async #verifyParent(
    stageScope?: PersistentOutputStageScope,
  ): Promise<FileSystemDirectoryHandle> {
    try {
      const verified = await verifyFSAOperationBinding({
        repository: this.#operationRepository,
        intent: this.#binding.intent,
        expectedParent: this.#binding.parent,
        ...(stageScope === undefined ? {} : { stageScope }),
      })
      return verified.parent
    } catch (error) {
      this.#authorities.invalidateOperation()
      throw error
    }
  }

  async #requirePreparedRoot(): Promise<void> {
    if (this.#binding.reservation.entryKind === 'result-root' && this.#root === undefined) {
      await this.prepareRoot()
    }
  }

  async #parent(
    path: readonly string[],
    stageScope?: PersistentOutputStageScope,
    entryKind: 'file' | 'directory' = 'file',
  ): Promise<Readonly<{ authority: FSAVerifiedDirectoryAuthority; name: string }>> {
    if (this.#binding.reservation.entryKind === 'single-file') {
      return {
        authority: this.#authorities.pickedParent(),
        name: this.#binding.reservation.physicalName,
      }
    }
    const name = path.at(-1)
    if (name === undefined) throw new TypeError('Output file path has no leaf')
    return {
      authority: await this.#directoryAuthority(path.slice(0, -1), stageScope),
      name: this.#compatibleNames?.physicalComponent(path, entryKind) ?? name,
    }
  }

  async #directoryAuthority(
    path: readonly string[],
    stageScope?: PersistentOutputStageScope,
    correlateWalkedPath = false,
  ): Promise<FSAVerifiedDirectoryAuthority> {
    const retainedRoot = this.#authorities.directory([])
    if (retainedRoot === undefined) {
      throw new TargetOwnershipUnknownError(
        'parent-authority',
        this.#binding.intent.operationId,
      )
    }
    let current = retainedRoot
    const walked: string[] = []
    for (const segment of path) {
      walked.push(segment)
      let operationScope = stageScope
      if (correlateWalkedPath &&
          (walked.length !== path.length || operationScope === undefined)) {
        operationScope = this.#directoryScope(walked)
      }
      const parent = current
      const canonicalWalked = snapshotRelativePath(walked, false)
      try {
        current = await this.#authorities.resolveDirectory(canonicalWalked, 1, async () => {
          const handleId = await fsaDirectoryHandleId(this.#binding.reservation, canonicalWalked)
          const persisted = await readFSAOwnedDirectoryBinding({
            repository: this.#operationRepository,
            reservation: this.#binding.reservation,
            handleId,
            diagnosticTarget: 'directory',
            ...(operationScope === undefined ? {} : { stageScope: operationScope }),
          })
          if (persisted === undefined) {
            throw new TargetOwnershipUnknownError(
              'parent-authority',
              this.#binding.intent.operationId,
            )
          }
          const physicalName = this.#compatibleNames
            ?.physicalComponent(canonicalWalked, 'directory') ?? segment
          const next = await openDirectoryEntry(
            parent.handle,
            physicalName,
            this.#binding.intent.operationId,
            operationScope,
            'fsa.directory.entry.open',
          )
          if (!await verifySameDirectory(
            next,
            persisted.handle,
            this.#binding.intent.operationId,
            operationScope,
            'fsa.directory.handle.verify',
          )) {
            throw new TargetOwnershipUnknownError(
              'parent-authority',
              this.#binding.intent.operationId,
            )
          }
          return {
            ...persisted,
            canonicalPath: canonicalWalked,
            physicalName,
            handle: next,
          }
        })
      } catch (error) {
        this.#authorities.invalidateSubtree(canonicalWalked)
        throw error
      }
    }
    return current
  }

  #rootScope(): PersistentOutputStageScope | undefined {
    const scope = this.#stageAuthority?.rootScope()
    if (scope === undefined || this.#binding.reservation.entryKind === 'single-file') return scope
    scope.addFailureFacts('fsa', async context => captureFSAFailureFacts({
      target: {
        kind: 'named-entry',
        resolve: async () => ({
          parent: this.#binding.parent,
          name: this.#binding.reservation.physicalName,
        }),
      },
      permissionFallback: this.#binding.parent,
      expectedKind: 'directory',
      readPersistedHandle: async () => {
        const handleId = await fsaDirectoryHandleId(this.#binding.reservation, [])
        return (await this.#operationRepository
          .readHandle<FileSystemDirectoryHandle>(handleId))?.handle
      },
    }, context))
    return scope
  }

  #directoryScope(path: readonly string[]): PersistentOutputStageScope | undefined {
    const scope = this.#stageAuthority?.directoryScope(path)
    if (scope === undefined) return undefined
    scope.addFailureFacts('fsa', async context => captureFSAFailureFacts({
      target: {
        kind: 'named-entry',
        resolve: async () => {
          const resolved = await this.#parent(path, undefined, 'directory')
          return { parent: resolved.authority.handle, name: resolved.name }
        },
      },
      permissionFallback: this.#binding.parent,
      expectedKind: 'directory',
      readPersistedHandle: async () => {
        const handleId = await fsaDirectoryHandleId(this.#binding.reservation, path)
        const record = await this.#operationRepository
          .readHandle<FileSystemDirectoryHandle>(handleId)
        if (record === undefined) return undefined
        const valid = record.kind === FSA_OPERATION_HANDLE_DIRECTORY &&
          record.operationId === this.#binding.intent.operationId &&
          record.authorityRef === this.#binding.reservation.authorityRef
        return valid ? record.handle : Object.freeze({})
      },
    }, context))
    return scope
  }

  #mutate<Value>(
    kind: FSANamespaceMutationKind,
    operation: () => Promise<Value>,
  ): Promise<Value> {
    return this.#mutations.run(kind, operation)
  }
}
