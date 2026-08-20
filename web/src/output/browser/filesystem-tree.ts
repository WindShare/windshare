import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import { encodeBase64Url } from '../../crypto/bytes'
import type { NamedContainerEntryReservation } from '../../transfer/intent'
import type { PersistentHandleRepository } from '../persistence/journal'
import type {
  OpenedFileRevision,
  PersistentDirectoryMaterialization,
  PersistentOutputTree,
  PersistentTreeFile,
} from '../persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import {
  runPersistentOutputStage,
  type PersistentOutputStage,
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
  FSA_OPERATION_HANDLE_DIRECTORY,
  fsaOwnedDirectoryHandleId,
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

const READ_WRITE_PERMISSION = Object.freeze({ mode: 'readwrite' as const })
const FSA_DIRECTORY_LOCATOR_DOMAIN = 'windshare/fsa-directory-locator/v1'

interface PermissionCapableDirectoryHandle extends FileSystemDirectoryHandle {
  queryPermission?(descriptor?: { readonly mode?: 'read' | 'readwrite' }): Promise<PermissionState>
  requestPermission?(descriptor?: { readonly mode?: 'read' | 'readwrite' }): Promise<PermissionState>
}

export interface BrowserFileSystemTreeOptions {
  readonly binding: PersistedFSAOperationBinding
  readonly operationRepository: FSAOperationBindingRepository
  readonly fileHandles: PersistentHandleRepository
  readonly mutations: FSARootMutationAuthority
  readonly stageAuthority?: PersistentOutputStageAuthority
  readonly randomOwnedObjectId?: () => string
}

export class BrowserFileSystemTree implements PersistentOutputTree {
  readonly #binding: PersistedFSAOperationBinding
  readonly #operationRepository: FSAOperationBindingRepository
  readonly #mutations: FSARootMutationAuthority
  readonly #stageAuthority: PersistentOutputStageAuthority | undefined
  readonly #randomOwnedObjectId: () => string
  readonly #files: BrowserFileLineageAuthority
  #root: FileSystemDirectoryHandle | undefined
  #rootOwnedObjectId: string | undefined

  constructor(options: BrowserFileSystemTreeOptions) {
    this.#binding = options.binding
    this.#operationRepository = options.operationRepository
    this.#mutations = options.mutations
    this.#stageAuthority = options.stageAuthority
    this.#randomOwnedObjectId = options.randomOwnedObjectId ?? createOwnedObjectId
    this.#files = new BrowserFileLineageAuthority({
      binding: options.binding,
      fileHandles: options.fileHandles,
      mutations: options.mutations,
      prepareRoot: () => this.#requirePreparedRoot(),
      verifyParent: scope => this.#verifyParent(scope),
      resolveParent: (path, scope) => this.#parent(path, scope),
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
    if (this.#binding.reservation.entryKind === 'single-file') return
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
          this.#binding.reservation.reservedName,
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
        () => namespaceEntryExists(parent, this.#binding.reservation.reservedName),
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
          this.#binding.reservation.reservedName,
          { create: true },
        ),
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
        this.#binding.reservation.reservedName,
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
  }

  async reopenOwnedRootForCleanup(): Promise<void> {
    const scope = this.#rootScope()
    await this.authorize()
    if (this.#binding.reservation.entryKind === 'single-file') return
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
        this.#binding.reservation.reservedName,
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
      const name = canonicalPath.at(-1)!
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
        return Object.freeze({ ownedObjectId: record.ownedObjectId, created: false })
      }
      if (await runPersistentOutputStage(
        scope,
        'fsa.directory.entry.inspect',
        () => namespaceEntryExists(parent, name),
      )) {
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
      return Object.freeze({ ownedObjectId, created: true })
    })
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
          this.#binding.reservation.reservedName,
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
        await parent.removeEntry(this.#binding.reservation.reservedName, { recursive: true })
        return
      }
      const { parent, name } = await this.#parent(canonicalPath)
      await parent.removeEntry(name, { recursive: true })
    })
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
  ): Promise<Readonly<{ parent: FileSystemDirectoryHandle; name: string }>> {
    const name = path.at(-1)
    if (name === undefined) throw new TypeError('Output file path has no leaf')
    if (this.#binding.reservation.entryKind === 'single-file') {
      return { parent: await this.#verifyParent(stageScope), name }
    }
    return { parent: await this.#directory(path.slice(0, -1), stageScope), name }
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
        segment,
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
          name: this.#binding.reservation.reservedName,
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
        resolve: () => this.#parent(path),
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

export async function fsaDirectoryHandleId(
  reservation: NamedContainerEntryReservation,
  path: readonly string[],
): Promise<string> {
  const encodedPath = path.map(segment => `${segment.length}:${segment}`).join('/')
  const material = new TextEncoder().encode(
    `${FSA_DIRECTORY_LOCATOR_DOMAIN}\0${reservation.digest}\0${encodedPath}`,
  )
  const digest = encodeBase64Url(new Uint8Array(await crypto.subtle.digest('SHA-256', material)))
  return fsaOwnedDirectoryHandleId(reservation.operationId, digest)
}

function snapshotRelativePath(path: readonly string[], allowEmpty: boolean): readonly string[] {
  if (allowEmpty && path.length === 0) return Object.freeze([])
  return snapshotPortableCatalogPath(path)
}

async function openDirectoryEntry(
  parent: FileSystemDirectoryHandle,
  name: string,
  operationId: string,
  stageScope: PersistentOutputStageScope | undefined,
  diagnosticStage: Extract<
    PersistentOutputStage,
    'fsa.root.entry.open' | 'fsa.directory.entry.open'
  >,
): Promise<FileSystemDirectoryHandle> {
  try {
    return await runPersistentOutputStage(
      stageScope,
      diagnosticStage,
      () => parent.getDirectoryHandle(name),
    )
  } catch (cause) {
    throw new TargetOwnershipUnknownError('parent-authority', operationId, { cause })
  }
}

async function verifySameDirectory(
  left: FileSystemDirectoryHandle,
  right: FileSystemDirectoryHandle,
  operationId: string,
  stageScope: PersistentOutputStageScope | undefined,
  diagnosticStage: Extract<
    PersistentOutputStage,
    'fsa.root.handle.verify' | 'fsa.directory.handle.verify'
  >,
): Promise<boolean> {
  try {
    return await runPersistentOutputStage(
      stageScope,
      diagnosticStage,
      () => left.isSameEntry(right),
    )
  } catch (cause) {
    throw new TargetOwnershipUnknownError('parent-authority', operationId, { cause })
  }
}
