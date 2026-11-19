import { IndexedDbFileCheckpointRepository } from '../browser/indexeddb-repository'
import type { OutputDiagnosticsPorts } from '../diagnostics'
import { acquireArtifactReader, withArtifactCleanup } from '../origin-private/export-readers'
import type { NativeObjectFactory } from '../origin-private/native-object/contracts'
import type { ObjectCapacity } from '../origin-private/object-capacity'
import { ORIGIN_PRIVATE_RAW_FILE_CONTAINER } from '../origin-private/workspace-root'
import { OriginPrivateWorkspaceTree, originPrivateRawFileHandleId } from '../origin-private/workspace-tree'
import { fileCheckpointDigest, fileCheckpointIsComplete, type FileCheckpointV2 } from '../persistence/checkpoint'
import { checkpointMatchesNamespace } from '../persistence/journal'
import { PersistentTreeOutputSession } from '../persistent-tree/session'
import type { PersistentFileRequest } from '../persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import { readBrowserDeliveryCheckpoint } from './checkpoint-reader'
import type { BrowserSavePolicyV1 } from './model'
import type { BrowserDeliveryStageDiscard, BrowserDeliveryStagePort, BrowserDeliveryStageReader } from './ports'
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

  beginFile(request: PersistentFileRequest, capacity: ObjectCapacity) {
    return this.#session.beginFile(request, ownedObjectId => this.#root.bindCapacity(ownedObjectId, capacity))
  }

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
      await this.#removeOwnedFile(checkpoint.canonicalPath, checkpoint.ownedObjectId)
    })
  }

  async discard(stagedFile: BrowserDeliveryStageDiscard): Promise<void> {
    await withArtifactCleanup(this.#root.operationId, async () => {
      if (stagedFile.kind === 'checkpointed') {
        const { checkpoint } = stagedFile
        await this.#requireCommitted(checkpoint)
        await this.#removeOwnedFile(checkpoint.canonicalPath, checkpoint.ownedObjectId)
        return
      }
      // The initial allocation may exist before its checkpoint commits.
      const path = browserDeliveryStagingPath(stagedFile.source.fileId)
      const ownedObjectId = await this.#tree.proposeFileOwnedObjectId(path, stagedFile.source)
      await this.#removeOwnedFile(path, ownedObjectId)
    })
  }

  async #removeOwnedFile(path: readonly string[], ownedObjectId: string): Promise<void> {
    const current = await this.#root.readObject(ORIGIN_PRIVATE_RAW_FILE_CONTAINER, ownedObjectId, 'cleanup')
    if (current === undefined) {
      // A persisted cleanup cut permits finishing a crash after deletion but before handle retirement.
      await this.#checkpoints.deleteHandle(originPrivateRawFileHandleId(this.#root.operationId, ownedObjectId))
      return
    }
    await this.#tree.removeFile(path, ownedObjectId)
  }

  async #requireComplete(checkpoint: FileCheckpointV2): Promise<void> {
    if (!fileCheckpointIsComplete(checkpoint)) {
      throw new TypeError('Local delivery requires the exact complete staging checkpoint')
    }
    await this.#requireCommitted(checkpoint)
  }

  async #requireCommitted(checkpoint: FileCheckpointV2): Promise<void> {
    if (!checkpointMatchesNamespace(checkpoint, this.#checkpoints.binding) ||
        checkpoint.canonicalPath.length !== 1 || checkpoint.canonicalPath[0] !== checkpoint.fileId) {
      throw new TypeError('Checkpoint does not belong to this staging file')
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
