import { IndexedDbFileCheckpointRepository } from '../browser/indexeddb-repository'
import type { OutputDiagnosticsPorts } from '../diagnostics'
import { acquireArtifactReader, withArtifactCleanup } from '../origin-private/export-readers'
import type { NativeObjectFactory } from '../origin-private/native-object/contracts'
import type { ObjectCapacity } from '../origin-private/object-capacity'
import { ORIGIN_PRIVATE_RAW_FILE_CONTAINER } from '../origin-private/workspace-root'
import { OriginPrivateWorkspaceTree, originPrivateRawFileHandleId, rawFileObjectId } from '../origin-private/workspace-tree'
import { fileCheckpointDigest, fileCheckpointIsComplete, type FileCheckpointV2 } from '../persistence/checkpoint'
import { checkpointMatchesNamespace } from '../persistence/journal'
import { PersistentTreeOutputSession } from '../persistent-tree/session'
import type { PersistentFileRequest } from '../persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import { readBrowserDeliveryCheckpoint } from './checkpoint-reader'
import type { BrowserDeliverySource, BrowserSavePolicyV1 } from './model'
import type { BrowserDeliveryStagePort, BrowserDeliveryStageReader } from './ports'
import { browserDeliveryStagingPath } from './records'
import { BrowserDeliveryStagingRoot } from './staging-root'

export class BrowserDeliveryStaging implements BrowserDeliveryStagePort {
  readonly #root: BrowserDeliveryStagingRoot
  readonly #tree: OriginPrivateWorkspaceTree
  readonly #session: PersistentTreeOutputSession
  readonly #checkpoints: IndexedDbFileCheckpointRepository

  private constructor(root: BrowserDeliveryStagingRoot, tree: OriginPrivateWorkspaceTree, session: PersistentTreeOutputSession, checkpoints: IndexedDbFileCheckpointRepository) {
    this.#root = root
    this.#tree = tree
    this.#session = session
    this.#checkpoints = checkpoints
  }

  static async open(input: {
    readonly policy: BrowserSavePolicyV1
    readonly parent: FileSystemDirectoryHandle
    readonly databaseName?: string
    readonly nativeObjectFactory?: NativeObjectFactory
    readonly diagnostics?: OutputDiagnosticsPorts
  }): Promise<BrowserDeliveryStaging> {
    if (input.policy.staging === undefined) throw new TypeError('Browser save policy has no staging authority')
    const checkpoints = await IndexedDbFileCheckpointRepository.open(input.policy.staging, input.databaseName)
    try {
      const root = await BrowserDeliveryStagingRoot.open({ binding: input.policy.staging, parentOperationId: input.policy.operationId, parent: input.parent, handles: checkpoints })
      const tree = new OriginPrivateWorkspaceTree({ root, handles: checkpoints,
        ...(input.nativeObjectFactory === undefined ? {} : { nativeObjectFactory: input.nativeObjectFactory }) })
      const session = await PersistentTreeOutputSession.open({ tree, checkpoints,
        ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }) })
      return new BrowserDeliveryStaging(root, tree, session, checkpoints)
    } catch (error) { checkpoints.close(); throw error }
  }

  async bindCapacity(source: BrowserDeliverySource, capacity: ObjectCapacity): Promise<void> {
    this.#root.bindCapacity(await rawFileObjectId(this.#root.operationId, browserDeliveryStagingPath(source.fileId), source), capacity)
  }

  beginFile(request: PersistentFileRequest) { return this.#session.beginFile(request) }
  ensureDirectory(path: readonly string[]) { return this.#session.ensureDirectory(path) }
  readCheckpoint(fileId: string) { return readBrowserDeliveryCheckpoint(this.#checkpoints, fileId) }

  async readComplete(checkpoint: FileCheckpointV2): Promise<BrowserDeliveryStageReader> {
    const lease = await acquireArtifactReader(this.#root.operationId)
    try {
      await this.#requireComplete(checkpoint)
      const file = await this.#tree.openFile(checkpoint.canonicalPath, checkpoint.ownedObjectId)
      if (file === undefined) throw new TargetOwnershipUnknownError('commit', this.#root.operationId)
      const blob = await file.read()
      if (BigInt(blob.size) !== checkpoint.exactSize) throw new TargetOwnershipUnknownError('commit', this.#root.operationId)
      return Object.freeze({ blob, release: () => lease.release() })
    } catch (error) { lease.release(); throw error }
  }

  async removeComplete(checkpoint: FileCheckpointV2): Promise<void> {
    await withArtifactCleanup(this.#root.operationId, async () => {
      await this.#requireComplete(checkpoint)
      const current = await this.#root.readObject(ORIGIN_PRIVATE_RAW_FILE_CONTAINER, checkpoint.ownedObjectId, 'cleanup')
      const handleId = originPrivateRawFileHandleId(this.#root.operationId, checkpoint.ownedObjectId)
      if (current === undefined) {
        // A persisted cleanup cut permits finishing a crash after deletion but before handle retirement.
        await this.#checkpoints.deleteHandle(handleId)
        return
      }
      await this.#tree.removeFile(checkpoint.canonicalPath, checkpoint.ownedObjectId)
    })
  }

  async discard(source: BrowserDeliverySource, checkpoint?: FileCheckpointV2): Promise<void> {
    await withArtifactCleanup(this.#root.operationId, async () => {
      const objectId = await rawFileObjectId(this.#root.operationId, browserDeliveryStagingPath(source.fileId), source)
      if (checkpoint !== undefined) {
        const committed = await this.#checkpoints.readCommitted(checkpoint.recordId)
        if (!checkpointMatchesNamespace(checkpoint, this.#checkpoints.binding) || checkpoint.ownedObjectId !== objectId ||
            committed === undefined || fileCheckpointDigest(committed) !== fileCheckpointDigest(checkpoint)) {
          throw new TypeError('Discard requires the current owned receiving checkpoint')
        }
      }
      const current = await this.#root.readObject(ORIGIN_PRIVATE_RAW_FILE_CONTAINER, objectId, 'cleanup')
      if (current === undefined) {
        await this.#checkpoints.deleteHandle(originPrivateRawFileHandleId(this.#root.operationId, objectId))
      } else {
        await this.#tree.removeFile(browserDeliveryStagingPath(source.fileId), objectId)
      }
    })
  }

  async #requireComplete(checkpoint: FileCheckpointV2): Promise<void> {
    if (!checkpointMatchesNamespace(checkpoint, this.#checkpoints.binding) || !fileCheckpointIsComplete(checkpoint)) {
      throw new TypeError('Local delivery requires the exact complete staging checkpoint')
    }
    const committed = await this.#checkpoints.readCommitted(checkpoint.recordId)
    if (committed === undefined || fileCheckpointDigest(committed) !== fileCheckpointDigest(checkpoint)) {
      throw new TypeError('Local staging checkpoint no longer has committed authority')
    }
  }

  async close(): Promise<void> {
    try { await this.#session.close() }
    finally { this.#checkpoints.close() }
  }
}
