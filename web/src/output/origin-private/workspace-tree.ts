import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import { openNativeObject } from './native-object/client'
import type { NativeObjectFactory, NativeObjectIO } from './native-object/contracts'
import type { ObjectGrowthReservation } from './object-capacity'
import type {
  PersistentHandleRecord,
  PersistentHandleRepository,
} from '../persistence/journal'
import type {
  OpenedFileRevision,
  PersistentDirectoryMaterialization,
  PersistentOutputTree,
  PersistentTreeFile,
  PersistentWriterOpenMode,
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
// The bounded original-file journal permits 16,384 ranges. Keep room for the
// replacement checkpoint and its metadata while admitted payload consumes quota.
const ORIGINAL_CHECKPOINT_HEADROOM_BYTES = 1024n * 1024n
const RAW_FILE_OBJECT_DOMAIN = 'windshare/origin-private/raw-file-object/v2'
const DIRECTORY_OBJECT_DOMAIN = 'windshare/origin-private/directory-object/v2'
const FILE_HANDLE_DOMAIN = 'windshare/origin-private/raw-file-handle/v2'
const DIRECTORY_HANDLE_DOMAIN = 'windshare/origin-private/directory-handle/v2'

type OriginPrivateObjectIdentityStage = Exclude<TargetOwnershipStage, 'reservation'>

export interface OriginPrivateWorkspaceTreeOptions {
  readonly root: OriginPrivateWorkspaceRoot
  readonly handles: PersistentHandleRepository
  readonly nativeObjectFactory?: NativeObjectFactory
}

/** Flat object names avoid giving mutable artifact paths any namespace authority. */
export class OriginPrivateWorkspaceTree implements PersistentOutputTree {
  readonly #root: OriginPrivateWorkspaceRoot
  readonly #handles: PersistentHandleRepository
  readonly #nativeObjectFactory: NativeObjectFactory

  constructor(options: OriginPrivateWorkspaceTreeOptions) {
    this.#root = options.root
    this.#handles = options.handles
    this.#nativeObjectFactory = options.nativeObjectFactory ?? openNativeObject
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

  async proposeFileOwnedObjectId(
    path: readonly string[],
    revision: OpenedFileRevision,
  ): Promise<string> {
    return rawFileObjectId(
      this.#root.operationId,
      snapshotPath(path, false),
      snapshotRevision(revision),
    )
  }

  async inspectFileDestination(
    path: readonly string[],
    selectedOwnedObjectId: string,
  ): Promise<'absent' | 'occupied'> {
    snapshotPath(path, false)
    const ownedObjectId = snapshotIdentity(selectedOwnedObjectId, 32, 'owned object ID')
    const persisted = await this.#readHandle(
      originPrivateRawFileHandleId(this.#root.operationId, ownedObjectId),
      'namespace-create',
    )
    const current = await this.#root.readObject(
      ORIGIN_PRIVATE_RAW_FILE_CONTAINER,
      ownedObjectId,
      'namespace-create',
    )
    return persisted === undefined && current === undefined ? 'absent' : 'occupied'
  }

  async createFileAfterRevisionOpen(
    path: readonly string[],
    revision: OpenedFileRevision,
    selectedOwnedObjectId: string,
  ): Promise<PersistentTreeFile> {
    const canonical = snapshotPath(path, false)
    const opened = snapshotRevision(revision)
    const ownedObjectId = snapshotIdentity(selectedOwnedObjectId, 32, 'owned object ID')
    if (ownedObjectId !== await rawFileObjectId(this.#root.operationId, canonical, opened)) {
      throw new TypeError('selected raw file object does not match its materialization plan')
    }
    const id = originPrivateRawFileHandleId(this.#root.operationId, ownedObjectId)
    const persisted = await this.#readHandle(id, 'namespace-create')
    const current = await this.#root.readObject(
      ORIGIN_PRIVATE_RAW_FILE_CONTAINER,
      ownedObjectId,
      'namespace-create',
    )
    if (persisted !== undefined || current !== undefined) {
      const handle = await this.#requireMatchingHandle(
        persisted,
        ORIGIN_PRIVATE_FILE_HANDLE_KIND,
        ownedObjectId,
        current,
        'namespace-create',
      )
      return this.#file(canonical, ownedObjectId, handle)
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
      root: this.#root,
      nativeObjectFactory: this.#nativeObjectFactory,
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
  readonly durability = 'native-in-place' as const
  readonly ownedObjectId: string
  readonly #handle: FileSystemFileHandle
  readonly #root: OriginPrivateWorkspaceRoot
  readonly #nativeObjectFactory: NativeObjectFactory
  readonly #verifyIdentity: PersistentTreeFile['verify']
  #writer: NativeObjectIO | undefined
  #checkpointHeadroom: ObjectGrowthReservation | undefined

  constructor(input: {
    readonly ownedObjectId: string
    readonly handle: FileSystemFileHandle
    readonly root: OriginPrivateWorkspaceRoot
    readonly nativeObjectFactory: NativeObjectFactory
    readonly verify: PersistentTreeFile['verify']
  }) {
    this.ownedObjectId = input.ownedObjectId
    this.#handle = input.handle
    this.#root = input.root
    this.#nativeObjectFactory = input.nativeObjectFactory
    this.#verifyIdentity = input.verify
  }

  async openWriter(mode: PersistentWriterOpenMode): Promise<void> {
    if (this.#writer !== undefined) {
      throw new DOMException('The origin-private writer is already open', 'InvalidStateError')
    }
    await this.#verifyIdentity('writer-open')
    const writer = await this.#nativeObjectFactory(this.#handle)
    try {
      // Reclaim starts with an observed task total. Register the object's original
      // contribution before any shrink so reconciliation can subtract it exactly.
      let currentLength = await writer.size()
      await this.#root.reconcileObject(this.ownedObjectId, currentLength)
      if (mode === 'truncate') {
        await writer.truncate(0n)
        currentLength = await writer.size()
        await this.#root.reconcileObject(this.ownedObjectId, currentLength)
      }
      this.#checkpointHeadroom = await this.#root.reserveGrowth({
        objectId: this.ownedObjectId, currentLength, targetLength: 0n,
        metadataHeadroom: ORIGINAL_CHECKPOINT_HEADROOM_BYTES,
      })
      this.#writer = writer
    } catch (error) {
      await writer.close().catch(() => undefined)
      throw error
    }
  }

  async writeAt(offset: bigint, data: Uint8Array): Promise<void> {
    const writer = this.#writer
    if (writer === undefined) {
      throw new DOMException('The origin-private writer is not open', 'InvalidStateError')
    }
    const currentLength = await writer.size()
    const targetLength = offset + BigInt(data.byteLength)
    const reservation = await this.#root.reserveGrowth({
      objectId: this.ownedObjectId, currentLength, targetLength, metadataHeadroom: 0n,
    })
    try {
      await writer.writeAt(offset, data)
      await reservation.settle(await writer.size())
    } catch (error) {
      // Worker failure may hide a partially extended file. Preserve the full
      // admitted high-water length when physical size is no longer observable.
      const conservativeLength = currentLength > targetLength ? currentLength : targetLength
      const actualLength = await writer.size().catch(() => conservativeLength)
      try { await reservation.settle(actualLength) }
      catch { /* Keep the outstanding reservation fenced until recovery measures it. */ }
      throw error
    }
  }

  async flush(): Promise<void> {
    const writer = this.#writer
    if (writer === undefined) return
    await writer.flush()
  }

  async size(): Promise<bigint> {
    if (this.#writer !== undefined) return this.#writer.size()
    return BigInt((await this.#handle.getFile()).size)
  }

  verify(stage: 'writer-open' | 'checkpoint' | 'commit'): Promise<void> {
    return this.#verifyIdentity(stage)
  }

  async close(): Promise<void> {
    const writer = this.#writer
    const headroom = this.#checkpointHeadroom
    this.#writer = undefined
    this.#checkpointHeadroom = undefined
    try { await writer?.close() }
    finally { await headroom?.release() }
  }

  async abort(): Promise<void> {
    // Cancellation releases the exclusive handle; it cannot undo native writes.
    await this.close()
  }

  async read(): Promise<Blob> {
    await this.close()
    return this.#handle.getFile()
  }
}

export async function rawFileObjectId(
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
