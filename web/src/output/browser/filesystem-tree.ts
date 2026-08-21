import { OutputDirectoryMutationError } from '../../transfer/output-session'
import type { CompatibleNamePathAuthority } from '../file-system-access/compatible-name/coordinator'
import type { PersistentHandleRepository } from '../persistence/journal'
import type {
  OpenedFileRevision,
  PersistentDirectoryMaterialization,
  PersistentOutputTree,
  PersistentTreeFile,
} from '../persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import {
  PathComponentRejectedError,
  inspectFileSystemComponent,
} from './filesystem-component-inspection'
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
  openDirectoryEntry,
  snapshotRelativePath,
  verifySameDirectory,
} from './filesystem-directory-authority'
import {
  FSA_OPERATION_HANDLE_DIRECTORY,
  persistFSAOwnedDirectory,
  readFSAOwnedDirectory,
  verifyFSAOperationBinding,
  type FSAOperationBindingRepository,
  type PersistedFSAOperationBinding,
} from './indexeddb-root-binding'
import type {
  FSARootMutationAuthority,
  FSANamespaceMutationKind,
} from './namespace-mutation'

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
    this.#stageAuthority = options.stageAuthority
    this.#compatibleNames = options.compatibleNames
    this.#randomOwnedObjectId = options.randomOwnedObjectId ?? createOwnedObjectId
    this.#files = new BrowserFileLineageAuthority({
      binding: options.binding,
      fileHandles: options.fileHandles,
      mutations: options.mutations,
      prepareRoot: () => this.#requirePreparedRoot(),
      verifyParent: scope => this.#verifyParent(scope),
      resolveParent: (path, scope) => this.#parent(path, scope),
      pairParent: () => this.#root,
      ...(this.#compatibleNames === undefined ? {} : { compatibleNames: this.#compatibleNames }),
      randomOwnedObjectId: this.#randomOwnedObjectId,
    })
  }

  async authorize(): Promise<void> {
    const scope = this.#rootScope()
    const parent = (await this.#verifyParent(scope)) as PermissionCapableDirectoryHandle
    if (parent.queryPermission === undefined) return
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
    await this.#mutate('create-directory', async () => {
      const parent = await this.#verifyParent(scope)
      const handleId = await fsaDirectoryHandleId(this.#binding.reservation, [])
      const persisted = await readFSAOwnedDirectory({
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
          persisted,
          this.#binding.intent.operationId,
          scope,
          'fsa.root.handle.verify',
        )) {
          throw new TargetOwnershipUnknownError(
            'parent-authority',
            this.#binding.intent.operationId,
          )
        }
        const stored = await runPersistentOutputStage(
          scope,
          'indexeddb.root-handle.read',
          () => this.#operationRepository.readHandle<FileSystemDirectoryHandle>(handleId),
        )
        if (stored?.ownedObjectId === undefined) {
          throw new TargetOwnershipUnknownError(
            'parent-authority',
            this.#binding.intent.operationId,
          )
        }
        this.#root = current
        this.#rootOwnedObjectId = stored.ownedObjectId
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
      await persistFSAOwnedDirectory({
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
      const persisted = await readFSAOwnedDirectory({
        repository: this.#operationRepository,
        reservation: this.#binding.reservation,
        handleId,
        diagnosticTarget: 'root',
        ...(scope === undefined ? {} : { stageScope: scope }),
      })
      const record = await runPersistentOutputStage(
        scope,
        'indexeddb.root-handle.read',
        () => this.#operationRepository.readHandle<FileSystemDirectoryHandle>(handleId),
      )
      if (persisted === undefined || record?.ownedObjectId === undefined) {
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
        persisted,
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
      this.#rootOwnedObjectId = record.ownedObjectId
    })
    await this.#compatibleNames?.ensurePairReady(this.#root)
  }

  async ensureDirectory(path: readonly string[]): Promise<PersistentDirectoryMaterialization> {
    if (this.#binding.reservation.entryKind === 'single-file') {
      throw new TypeError('Single-file DirectoryTree cannot materialize a directory')
    }
    const canonicalPath = snapshotRelativePath(path, true)
    await this.#requirePreparedRoot()
    if (canonicalPath.length === 0) {
      if (this.#rootOwnedObjectId === undefined) {
        throw new TargetOwnershipUnknownError(
          'parent-authority',
          this.#binding.intent.operationId,
        )
      }
      return Object.freeze({ ownedObjectId: this.#rootOwnedObjectId, created: false })
    }
    const scope = this.#directoryScope(canonicalPath)
    return this.#mutate('create-directory', async () => {
      await this.#verifyParent(scope)
      const parent = await this.#directory(canonicalPath.slice(0, -1), undefined, true)
      let name = this.#compatibleNames?.physicalComponent(canonicalPath, 'directory') ??
        canonicalPath.at(-1)!
      const handleId = await fsaDirectoryHandleId(
        this.#binding.reservation,
        canonicalPath,
      )
      const persisted = await readFSAOwnedDirectory({
        repository: this.#operationRepository,
        reservation: this.#binding.reservation,
        handleId,
        diagnosticTarget: 'directory',
        ...(scope === undefined ? {} : { stageScope: scope }),
      })
      if (persisted !== undefined) {
        const current = await openDirectoryEntry(
          parent,
          name,
          this.#binding.intent.operationId,
          scope,
          'fsa.directory.entry.open',
        )
        if (!await verifySameDirectory(
          current,
          persisted,
          this.#binding.intent.operationId,
          scope,
          'fsa.directory.handle.verify',
        )) {
          throw new TargetOwnershipUnknownError(
            'parent-authority',
            this.#binding.intent.operationId,
          )
        }
        const record = await runPersistentOutputStage(
          scope,
          'indexeddb.directory-handle.read',
          () => this.#operationRepository.readHandle<FileSystemDirectoryHandle>(handleId),
        )
        if (record?.ownedObjectId === undefined) {
          throw new TargetOwnershipUnknownError(
            'parent-authority',
            this.#binding.intent.operationId,
          )
        }
        await this.#compatibleNames?.commitVerifiedDirectory(
          canonicalPath,
          record.ownedObjectId,
        )
        return Object.freeze({ ownedObjectId: record.ownedObjectId, created: false })
      }
      if (this.#compatibleNames?.hasLateLogicalCollision(canonicalPath, 'directory')) {
        throw new OutputDirectoryMutationError(
          'A logical directory collides with an operation-owned compatible name',
          false,
        )
      }
      let inspection: 'absent' | 'occupied'
      try {
        inspection = await runPersistentOutputStage(
          scope,
          'fsa.directory.entry.inspect',
          () => inspectFileSystemComponent({
            verifiedParent: parent,
            component: name,
            expectedKind: 'directory',
            stage: 'fsa.directory.entry.inspect',
            mode: this.#compatibleNames?.hasMapping(canonicalPath, 'directory') === true
              ? 'diagnostic'
              : 'classify-rejection',
          }),
        )
      } catch (error) {
        if (!(error instanceof PathComponentRejectedError) || this.#compatibleNames === undefined ||
            this.#root === undefined) throw error
        name = await this.#compatibleNames.resolveRejectedComponent({
          rejection: error,
          artifactPath: canonicalPath,
          entryKind: 'directory',
          parent,
          pairParent: this.#root,
        })
        inspection = 'absent'
      }
      if (inspection === 'occupied') {
        throw new TargetOwnershipUnknownError(
          'namespace-create',
          this.#binding.intent.operationId,
        )
      }
      const created = await runPersistentOutputStage(
        scope,
        'fsa.directory.entry.create',
        () => parent.getDirectoryHandle(name, { create: true }),
      )
      await this.#recordCompatibleDirectoryTargetCreated(canonicalPath)
      const ownedObjectId = requireOwnedObjectId(this.#randomOwnedObjectId())
      const ownedScope = scope?.withCorrelation({ ownedObjectId })
      await persistFSAOwnedDirectory({
        repository: this.#operationRepository,
        reservation: this.#binding.reservation,
        handleId,
        ownedObjectId,
        handle: created,
        diagnosticTarget: 'directory',
        ...(ownedScope === undefined ? {} : { stageScope: ownedScope }),
      })
      const current = await openDirectoryEntry(
        parent,
        name,
        this.#binding.intent.operationId,
        ownedScope,
        'fsa.directory.entry.open',
      )
      if (!await verifySameDirectory(
        current,
        created,
        this.#binding.intent.operationId,
        ownedScope,
        'fsa.directory.handle.verify',
      )) {
        throw new TargetOwnershipUnknownError(
          'namespace-create',
          this.#binding.intent.operationId,
        )
      }
      await this.#compatibleNames?.commitVerifiedDirectory(canonicalPath, ownedObjectId)
      return Object.freeze({ ownedObjectId, created: true })
    })
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
    const persisted = await readFSAOwnedDirectory({
      repository: this.#operationRepository,
      reservation: this.#binding.reservation,
      handleId,
      ownedObjectId,
      diagnosticTarget: canonicalPath.length === 0 ? 'root' : 'directory',
      ...(scope === undefined ? {} : { stageScope: scope }),
    })
    if (persisted === undefined) return false
    const current = canonicalPath.length === 0
      ? await openDirectoryEntry(
          await this.#verifyParent(scope),
          this.#binding.reservation.physicalName,
          this.#binding.intent.operationId,
          scope,
          'fsa.root.entry.open',
        )
      : await this.#directory(canonicalPath, scope, true)
    return verifySameDirectory(
      current,
      persisted,
      this.#binding.intent.operationId,
      scope,
      canonicalPath.length === 0
        ? 'fsa.root.handle.verify'
        : 'fsa.directory.handle.verify',
    )
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
  ): Promise<PersistentTreeFile> {
    return this.#files.createAfterRevisionOpen(
      path,
      revision,
      selectedOwnedObjectId,
      stageScope,
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

  async removeDirectory(path: readonly string[], ownedObjectId: string): Promise<void> {
    if (this.#binding.reservation.entryKind === 'single-file') {
      throw new TypeError('Single-file DirectoryTree has no owned directory')
    }
    const canonicalPath = snapshotRelativePath(path, true)
    if (!await this.validateDirectory(canonicalPath, ownedObjectId)) {
      throw new TargetOwnershipUnknownError('cleanup', this.#binding.intent.operationId)
    }
    await this.#mutate('remove-entry', async () => {
      if (canonicalPath.length === 0) {
        const parent = await this.#verifyParent()
        await parent.removeEntry(this.#binding.reservation.physicalName, { recursive: true })
        return
      }
      const { parent, name } = await this.#parent(canonicalPath, undefined, 'directory')
      await parent.removeEntry(name, { recursive: true })
    })
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

  async #verifyParent(
    stageScope?: PersistentOutputStageScope,
  ): Promise<FileSystemDirectoryHandle> {
    const verified = await verifyFSAOperationBinding({
      repository: this.#operationRepository,
      intent: this.#binding.intent,
      expectedParent: this.#binding.parent,
      ...(stageScope === undefined ? {} : { stageScope }),
    })
    return verified.parent
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
  ): Promise<Readonly<{ parent: FileSystemDirectoryHandle; name: string }>> {
    const name = path.at(-1)
    if (name === undefined) throw new TypeError('Output file path has no leaf')
    if (this.#binding.reservation.entryKind === 'single-file') {
      return {
        parent: await this.#verifyParent(stageScope),
        name: this.#binding.reservation.physicalName,
      }
    }
    return {
      parent: await this.#directory(path.slice(0, -1), stageScope),
      name: this.#compatibleNames?.physicalComponent(path, entryKind) ?? name,
    }
  }

  async #directory(
    path: readonly string[],
    stageScope?: PersistentOutputStageScope,
    correlateWalkedPath = false,
  ): Promise<FileSystemDirectoryHandle> {
    if (this.#root === undefined) {
      throw new TargetOwnershipUnknownError(
        'parent-authority',
        this.#binding.intent.operationId,
      )
    }
    let current = this.#root
    const walked: string[] = []
    for (const segment of path) {
      walked.push(segment)
      let operationScope = stageScope
      if (correlateWalkedPath &&
          (walked.length !== path.length || operationScope === undefined)) {
        operationScope = this.#directoryScope(walked)
      }
      const handleId = await fsaDirectoryHandleId(this.#binding.reservation, walked)
      const persisted = await readFSAOwnedDirectory({
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
      const next = await openDirectoryEntry(
        current,
        this.#compatibleNames?.physicalComponent(walked, 'directory') ?? segment,
        this.#binding.intent.operationId,
        operationScope,
        'fsa.directory.entry.open',
      )
      if (!await verifySameDirectory(
        next,
        persisted,
        this.#binding.intent.operationId,
        operationScope,
        'fsa.directory.handle.verify',
      )) {
        throw new TargetOwnershipUnknownError(
          'parent-authority',
          this.#binding.intent.operationId,
        )
      }
      current = next
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
        resolve: () => this.#parent(path, undefined, 'directory'),
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
