import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import { bigintToSafeNumber } from '../../content/geometry'
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
import {
  TargetOwnershipUnknownError,
  type TargetOwnershipStage,
} from '../persistent-tree/errors'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalIdentity,
  canonicalPath,
  canonicalRecord,
  canonicalU64,
  snapshotIdentity,
} from '../workspace/canonical'
import {
  ORIGIN_PRIVATE_DIRECTORY_OBJECT_CONTAINER,
  ORIGIN_PRIVATE_RAW_FILE_CONTAINER,
  type OriginPrivateWorkspaceRoot,
} from './workspace-root'

export const ORIGIN_PRIVATE_FILE_HANDLE_KIND = 3 as const
export const ORIGIN_PRIVATE_DIRECTORY_HANDLE_KIND = 4 as const
const RAW_FILE_OBJECT_DOMAIN = 'windshare/origin-private/raw-file-object/v2'
const DIRECTORY_OBJECT_DOMAIN = 'windshare/origin-private/directory-object/v2'
const FILE_HANDLE_DOMAIN = 'windshare/origin-private/raw-file-handle/v2'
const DIRECTORY_HANDLE_DOMAIN = 'windshare/origin-private/directory-handle/v2'

type OriginPrivateObjectIdentityStage = Exclude<TargetOwnershipStage, 'reservation'>

export interface OriginPrivateWorkspaceTreeOptions {
  readonly root: OriginPrivateWorkspaceRoot
  readonly handles: PersistentHandleRepository
}

/** Flat object names avoid giving mutable artifact paths any namespace authority. */
export class OriginPrivateWorkspaceTree implements PersistentOutputTree {
  readonly #root: OriginPrivateWorkspaceRoot
  readonly #handles: PersistentHandleRepository

  constructor(options: OriginPrivateWorkspaceTreeOptions) {
    this.#root = options.root
    this.#handles = options.handles
  }

  authorize(): Promise<void> {
    return this.#root.authorize()
  }

  prepareRoot(): Promise<void> {
    return this.#root.prepareContainers()
  }

  async ensureDirectory(path: readonly string[]): Promise<PersistentDirectoryMaterialization> {
    const canonical = snapshotPath(path, true)
    if (canonical.length === 0) {
      await this.#root.authorize()
      return Object.freeze({ ownedObjectId: this.#root.rootOwnedObjectId(), created: false })
    }
    const ownedObjectId = await directoryObjectId(this.#root.operationId, canonical)
    const id = originPrivateDirectoryHandleId(this.#root.operationId, ownedObjectId)
    const persisted = await this.#readHandle(id, 'parent-authority')
    const current = await this.#root.readObject(
      ORIGIN_PRIVATE_DIRECTORY_OBJECT_CONTAINER,
      ownedObjectId,
      'parent-authority',
    )
    if (persisted !== undefined) {
      await this.#requireMatchingHandle(
        persisted,
        ORIGIN_PRIVATE_DIRECTORY_HANDLE_KIND,
        ownedObjectId,
        current,
        'parent-authority',
      )
      return Object.freeze({ ownedObjectId, created: false })
    }
    if (current !== undefined) {
      throw new TargetOwnershipUnknownError('namespace-create', this.#root.operationId)
    }
    const created = await this.#root.createObject(
      ORIGIN_PRIVATE_DIRECTORY_OBJECT_CONTAINER,
      ownedObjectId,
    )
    await this.#persistHandle(Object.freeze({
      id,
      operationId: this.#root.operationId,
      kind: ORIGIN_PRIVATE_DIRECTORY_HANDLE_KIND,
      authorityRef: this.#root.authorityRef,
      ownedObjectId,
      handle: created,
    }), 'namespace-create')
    await this.#requireMatchingHandle(
      await this.#readHandle(id, 'namespace-create'),
      ORIGIN_PRIVATE_DIRECTORY_HANDLE_KIND,
      ownedObjectId,
      await this.#root.readObject(
        ORIGIN_PRIVATE_DIRECTORY_OBJECT_CONTAINER,
        ownedObjectId,
        'namespace-create',
      ),
      'namespace-create',
    )
    return Object.freeze({ ownedObjectId, created: true })
  }

  async validateDirectory(path: readonly string[], ownedObjectId: string): Promise<boolean> {
    const canonical = snapshotPath(path, true)
    const objectId = snapshotIdentity(ownedObjectId, 32, 'owned object ID')
    if (canonical.length === 0) {
      await this.#root.authorize()
      return objectId === this.#root.rootOwnedObjectId()
    }
    if (objectId !== await directoryObjectId(this.#root.operationId, canonical)) return false
    const persisted = await this.#readHandle(
      originPrivateDirectoryHandleId(this.#root.operationId, objectId),
      'parent-authority',
    )
    const current = await this.#root.readObject(
      ORIGIN_PRIVATE_DIRECTORY_OBJECT_CONTAINER,
      objectId,
      'parent-authority',
    )
    if (persisted === undefined || current === undefined) return false
    await this.#requireMatchingHandle(
      persisted,
      ORIGIN_PRIVATE_DIRECTORY_HANDLE_KIND,
      objectId,
      current,
      'parent-authority',
    )
    return true
  }

  async createFileAfterRevisionOpen(
    path: readonly string[],
    revision: OpenedFileRevision,
  ): Promise<PersistentTreeFile> {
    const canonical = snapshotPath(path, false)
    const opened = snapshotRevision(revision)
    const ownedObjectId = await rawFileObjectId(this.#root.operationId, canonical, opened)
    const id = originPrivateRawFileHandleId(this.#root.operationId, ownedObjectId)
    const persisted = await this.#readHandle(id, 'namespace-create')
    const current = await this.#root.readObject(
      ORIGIN_PRIVATE_RAW_FILE_CONTAINER,
      ownedObjectId,
      'namespace-create',
    )
    if (persisted !== undefined) {
      const handle = await this.#requireMatchingHandle(
        persisted,
        ORIGIN_PRIVATE_FILE_HANDLE_KIND,
        ownedObjectId,
        current,
        'namespace-create',
      )
      // A durable handle without a checkpoint is the precise crash cut this method owns.
      const reset = await handle.createWritable({ keepExistingData: false })
      await reset.close()
      return this.#file(canonical, ownedObjectId, handle)
    }
    if (current !== undefined) {
      throw new TargetOwnershipUnknownError('namespace-create', this.#root.operationId)
    }
    const created = await this.#root.createObject(ORIGIN_PRIVATE_RAW_FILE_CONTAINER, ownedObjectId)
    await this.#persistHandle(Object.freeze({
      id,
      operationId: this.#root.operationId,
      kind: ORIGIN_PRIVATE_FILE_HANDLE_KIND,
      authorityRef: this.#root.authorityRef,
      ownedObjectId,
      handle: created,
    }), 'namespace-create')
    const verified = await this.#requireMatchingHandle(
      await this.#readHandle(id, 'namespace-create'),
      ORIGIN_PRIVATE_FILE_HANDLE_KIND,
      ownedObjectId,
      await this.#root.readObject(
        ORIGIN_PRIVATE_RAW_FILE_CONTAINER,
        ownedObjectId,
        'namespace-create',
      ),
      'namespace-create',
    )
    return this.#file(canonical, ownedObjectId, verified)
  }

  async openFile(
    path: readonly string[],
    ownedObjectId: string,
  ): Promise<PersistentTreeFile | undefined> {
    const canonical = snapshotPath(path, false)
    const objectId = snapshotIdentity(ownedObjectId, 32, 'owned object ID')
    const persisted = await this.#readHandle(
      originPrivateRawFileHandleId(this.#root.operationId, objectId),
      'writer-open',
    )
    if (persisted === undefined) return undefined
    const current = await this.#root.readObject(
      ORIGIN_PRIVATE_RAW_FILE_CONTAINER,
      objectId,
      'writer-open',
    )
    const handle = await this.#requireMatchingHandle(
      persisted,
      ORIGIN_PRIVATE_FILE_HANDLE_KIND,
      objectId,
      current,
      'writer-open',
    )
    return this.#file(canonical, objectId, handle)
  }

  async removeFile(path: readonly string[], ownedObjectId: string): Promise<void> {
    const canonical = snapshotPath(path, false)
    const objectId = snapshotIdentity(ownedObjectId, 32, 'owned object ID')
    const handle = await this.#verifyFile(canonical, objectId, 'cleanup')
    await this.#root.removeObject(ORIGIN_PRIVATE_RAW_FILE_CONTAINER, objectId, handle)
    await this.#handles.deleteHandle(originPrivateRawFileHandleId(
      this.#root.operationId,
      objectId,
    ))
  }

  async removeDirectory(path: readonly string[], ownedObjectId: string): Promise<void> {
    const canonical = snapshotPath(path, true)
    if (canonical.length === 0) {
      throw new TypeError('workspace root cleanup belongs to the aggregate lifecycle')
    }
    const objectId = snapshotIdentity(ownedObjectId, 32, 'owned object ID')
    if (!await this.validateDirectory(canonical, objectId)) {
      throw new TargetOwnershipUnknownError('cleanup', this.#root.operationId)
    }
    const record = await this.#readHandle(
      originPrivateDirectoryHandleId(this.#root.operationId, objectId),
      'cleanup',
    )
    const handle = requireFileHandle(record?.handle, this.#root.operationId, 'cleanup')
    await this.#root.removeObject(ORIGIN_PRIVATE_DIRECTORY_OBJECT_CONTAINER, objectId, handle)
    await this.#handles.deleteHandle(originPrivateDirectoryHandleId(
      this.#root.operationId,
      objectId,
    ))
  }

  #file(
    path: readonly string[],
    ownedObjectId: string,
    handle: FileSystemFileHandle,
  ): OriginPrivatePersistentFile {
    return new OriginPrivatePersistentFile({
      ownedObjectId,
      handle,
      beforeFirstWrite: () => this.#root.readmit(),
      verify: (stage) => this.#verifyFile(path, ownedObjectId, stage).then(() => undefined),
    })
  }

  async #verifyFile(
    path: readonly string[],
    ownedObjectId: string,
    stage: 'writer-open' | 'checkpoint' | 'commit' | 'cleanup',
  ): Promise<FileSystemFileHandle> {
    snapshotPath(path, false)
    const persisted = await this.#readHandle(
      originPrivateRawFileHandleId(this.#root.operationId, ownedObjectId),
      stage,
    )
    const current = await this.#root.readObject(
      ORIGIN_PRIVATE_RAW_FILE_CONTAINER,
      ownedObjectId,
      stage,
    )
    return this.#requireMatchingHandle(
      persisted,
      ORIGIN_PRIVATE_FILE_HANDLE_KIND,
      ownedObjectId,
      current,
      stage,
    )
  }

  async #requireMatchingHandle(
    record: PersistentHandleRecord | undefined,
    kind: number,
    ownedObjectId: string,
    current: FileSystemFileHandle | undefined,
    stage: OriginPrivateObjectIdentityStage,
  ): Promise<FileSystemFileHandle> {
    const persisted = requireFileHandle(record?.handle, this.#root.operationId, stage)
    if (record === undefined || current === undefined ||
        record.operationId !== this.#root.operationId || record.kind !== kind ||
        record.authorityRef !== this.#root.authorityRef ||
        record.ownedObjectId !== ownedObjectId ||
        !await this.#root.sameObject(current, persisted, stage)) {
      throw new TargetOwnershipUnknownError(stage, this.#root.operationId)
    }
    return persisted
  }

  async #readHandle(
    id: string,
    stage: OriginPrivateObjectIdentityStage,
  ): Promise<PersistentHandleRecord | undefined> {
    try {
      return await this.#handles.readHandle(id)
    } catch (cause) {
      throw new TargetOwnershipUnknownError(stage, this.#root.operationId, { cause })
    }
  }

  async #persistHandle(
    record: PersistentHandleRecord,
    stage: 'namespace-create',
  ): Promise<void> {
    try {
      await this.#handles.putHandle(record)
    } catch (cause) {
      throw new TargetOwnershipUnknownError(stage, this.#root.operationId, { cause })
    }
  }
}

class OriginPrivatePersistentFile implements PersistentTreeFile {
  readonly ownedObjectId: string
  readonly #handle: FileSystemFileHandle
  readonly #beforeFirstWrite: () => Promise<void>
  readonly #verifyIdentity: PersistentTreeFile['verify']
  #writer: FileSystemWritableFileStream | undefined
  #writeAdmitted = false

  constructor(input: {
    readonly ownedObjectId: string
    readonly handle: FileSystemFileHandle
    readonly beforeFirstWrite: () => Promise<void>
    readonly verify: PersistentTreeFile['verify']
  }) {
    this.ownedObjectId = input.ownedObjectId
    this.#handle = input.handle
    this.#beforeFirstWrite = input.beforeFirstWrite
    this.#verifyIdentity = input.verify
  }

  async writeAt(offset: bigint, data: Uint8Array): Promise<void> {
    if (!this.#writeAdmitted) {
      await this.#beforeFirstWrite()
      this.#writeAdmitted = true
    }
    await this.#verifyIdentity('writer-open')
    this.#writer ??= await this.#handle.createWritable({ keepExistingData: true })
    await this.#writer.write({
      type: 'write',
      position: bigintToSafeNumber(offset, 'origin-private output offset'),
      data: data.slice(),
    })
  }

  async flush(): Promise<void> {
    const writer = this.#writer
    if (writer === undefined) return
    this.#writer = undefined
    await writer.close()
  }

  async size(): Promise<bigint> {
    await this.flush()
    return BigInt((await this.#handle.getFile()).size)
  }

  verify(stage: 'writer-open' | 'checkpoint' | 'commit'): Promise<void> {
    return this.#verifyIdentity(stage)
  }

  close(): Promise<void> {
    return this.flush()
  }

  async read(): Promise<Blob> {
    await this.flush()
    return this.#handle.getFile()
  }
}

async function rawFileObjectId(
  operationId: string,
  path: readonly string[],
  revision: OpenedFileRevision,
): Promise<string> {
  return canonicalDigest(canonicalRecord(RAW_FILE_OBJECT_DOMAIN, 2, [
    canonicalFrame(canonicalIdentity(operationId, 16, 'operation ID')),
    canonicalFrame(canonicalPath(path)),
    canonicalFrame(canonicalIdentity(revision.fileId, 16, 'file ID')),
    canonicalFrame(canonicalIdentity(revision.fileRevision, 16, 'file revision')),
    canonicalU64(revision.exactSize),
  ]))
}

async function directoryObjectId(operationId: string, path: readonly string[]): Promise<string> {
  return canonicalDigest(canonicalRecord(DIRECTORY_OBJECT_DOMAIN, 2, [
    canonicalFrame(canonicalIdentity(operationId, 16, 'operation ID')),
    canonicalPath(path),
  ]))
}

export function originPrivateRawFileHandleId(
  operationId: string,
  ownedObjectId: string,
): string {
  return `${FILE_HANDLE_DOMAIN}/${operationId}/${ownedObjectId}`
}

export function originPrivateDirectoryHandleId(
  operationId: string,
  ownedObjectId: string,
): string {
  return `${DIRECTORY_HANDLE_DOMAIN}/${operationId}/${ownedObjectId}`
}

function snapshotRevision(revision: OpenedFileRevision): OpenedFileRevision {
  return Object.freeze({
    fileId: snapshotIdentity(revision.fileId, 16, 'file ID'),
    fileRevision: snapshotIdentity(revision.fileRevision, 16, 'file revision'),
    exactSize: checkedU64(revision.exactSize, 'opened revision size'),
  })
}

function snapshotPath(path: readonly string[], allowEmpty: boolean): readonly string[] {
  if (allowEmpty && path.length === 0) return Object.freeze([])
  return snapshotPortableCatalogPath(path)
}

function requireFileHandle(
  value: unknown,
  operationId: string,
  stage: OriginPrivateObjectIdentityStage,
): FileSystemFileHandle {
  if (typeof value !== 'object' || value === null ||
      !('kind' in value) || value.kind !== 'file' ||
      !('isSameEntry' in value) || typeof value.isSameEntry !== 'function') {
    throw new TargetOwnershipUnknownError(stage, operationId)
  }
  return value as FileSystemFileHandle
}

function checkedU64(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n || value > 0xffff_ffff_ffff_ffffn) {
    throw new TypeError(`${label} is not a u64`)
  }
  return value
}
