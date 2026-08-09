import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  identityBytes,
  newFileCheckpointV2,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import {
  validateFileCheckpointPage,
  type FileCheckpointJournal,
} from '../persistence/journal'
import type {
  OpenedFileRevision,
  PersistentDirectoryMaterialization,
  PersistentFileRequest,
  PersistentMaterializationPort,
  PersistentTreeSessionOptions,
} from './contracts'
import {
  NOOP_PERSISTENT_TREE_TRACE,
} from './contracts'
import {
  SourceRevisionChangedError,
  TargetOwnershipUnknownError,
} from './errors'
import { PersistentFileTransaction } from './file-transaction'

export class PersistentTreeOutputSession implements PersistentMaterializationPort {
  readonly #tree: PersistentTreeSessionOptions['tree']
  readonly #checkpoints: FileCheckpointJournal
  readonly #trace: NonNullable<PersistentTreeSessionOptions['trace']>
  readonly #active = new Set<PersistentFileTransaction>()
  #needsAttentionReported = false
  #closed = false

  private constructor(options: PersistentTreeSessionOptions) {
    this.#tree = options.tree
    this.#checkpoints = options.checkpoints
    this.#trace = options.trace ?? NOOP_PERSISTENT_TREE_TRACE
  }

  static async open(options: PersistentTreeSessionOptions): Promise<PersistentTreeOutputSession> {
    const session = new PersistentTreeOutputSession(options)
    await options.tree.authorize()
    await options.tree.prepareRoot()
    return session
  }

  async beginFile(request: PersistentFileRequest): Promise<PersistentFileTransaction> {
    this.#requireOpen()
    const artifactPath = snapshotPortableCatalogPath(request.artifactPath)
    // Awaiting the authenticated revision is intentionally before any tree call. This
    // keeps unopened revisions from creating empty namespace placeholders.
    const revision = snapshotOpenedRevision(await request.openRevision())
    try {
      const existing = await this.#findCheckpoint(revision, artifactPath)
      let handle
      let checkpoint: FileCheckpointV2
      if (existing === undefined) {
        handle = await this.#tree.createFileAfterRevisionOpen(artifactPath, revision)
        checkpoint = await this.#installInitialCheckpoint(revision, artifactPath, handle.ownedObjectId)
      } else {
        handle = await this.#tree.openFile(artifactPath, existing.ownedObjectId)
        if (handle === undefined) {
          throw new TargetOwnershipUnknownError('checkpoint', existing.operationId)
        }
        const actualSize = await handle.size()
        const durableEnd = existing.verifiedRanges.at(-1)?.end ?? 0n
        if (actualSize < durableEnd || actualSize > revision.exactSize) {
          throw new TargetOwnershipUnknownError('checkpoint', existing.operationId)
        }
        await handle.verify('checkpoint')
        checkpoint = existing
      }
      const transaction = new PersistentFileTransaction({
        revision,
        handle,
        checkpoint,
        checkpoints: this.#checkpoints,
        onClose: (closed) => this.#active.delete(closed),
        onOwnershipUnknown: () => this.#reportNeedsAttention(),
      })
      this.#active.add(transaction)
      return transaction
    } catch (error) {
      if (error instanceof TargetOwnershipUnknownError) this.#reportNeedsAttention()
      throw error
    }
  }

  ensureDirectory(path: readonly string[]): Promise<PersistentDirectoryMaterialization> {
    this.#requireOpen()
    return this.#tree.ensureDirectory(path)
  }

  async close(): Promise<void> {
    if (this.#closed) return
    this.#closed = true
    const failures: unknown[] = []
    for (const transaction of this.#active) {
      try {
        await transaction.close()
      } catch (error) {
        failures.push(error)
      }
    }
    this.#active.clear()
    if (failures.length !== 0) {
      throw new AggregateError(failures, 'Persistent tree file transactions did not close cleanly')
    }
  }

  async #installInitialCheckpoint(
    revision: OpenedFileRevision,
    artifactPath: readonly string[],
    ownedObjectId: string,
  ): Promise<FileCheckpointV2> {
    const binding = this.#checkpoints.binding
    const candidate = newFileCheckpointV2({
      operationId: binding.operationId,
      receiveIntentDigest: binding.receiveIntentDigest,
      materializationBindingDigest: binding.materializationBindingDigest,
      fileId: revision.fileId,
      fileRevision: revision.fileRevision,
      canonicalPath: artifactPath,
      exactSize: revision.exactSize,
      materializerKind: binding.materializerKind,
      authorityRef: binding.authorityRef,
      ownedObjectId,
      stateGeneration: 1n,
      checkpointGeneration: 0n,
      verifiedRanges: [],
      phase: FILE_CHECKPOINT_PHASE_ACTIVE,
      commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
    })
    await this.#checkpoints.putCandidate(candidate)
    const committed = newFileCheckpointV2({
      ...candidate,
      commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
    })
    await this.#checkpoints.commit(committed)
    const reread = await this.#checkpoints.readCommitted(committed.recordId)
    if (reread === undefined || reread.checksum !== committed.checksum) {
      throw new TargetOwnershipUnknownError('checkpoint', binding.operationId)
    }
    return reread
  }

  async #findCheckpoint(
    revision: OpenedFileRevision,
    artifactPath: readonly string[],
  ): Promise<FileCheckpointV2 | undefined> {
    const candidates: FileCheckpointV2[] = []
    let cursor: string | undefined
    do {
      const scan = {
        direction: 'ascending' as const,
        fileId: revision.fileId,
        ...(cursor === undefined ? {} : { cursor }),
      }
      const page = validateFileCheckpointPage(
        await this.#checkpoints.scanCommitted(scan),
        scan,
        this.#checkpoints.binding,
      )
      candidates.push(...page.records)
      cursor = page.nextCursor
    } while (cursor !== undefined)
    if (candidates.length === 0) return undefined
    const matching = candidates.filter((checkpoint) =>
      checkpoint.fileRevision === revision.fileRevision &&
      checkpoint.exactSize === revision.exactSize &&
      samePath(checkpoint.canonicalPath, artifactPath))
    if (matching.length !== 1 || candidates.length !== 1) {
      if (matching.length === 0) throw new SourceRevisionChangedError()
      throw new TargetOwnershipUnknownError('checkpoint', this.#checkpoints.binding.operationId)
    }
    return matching[0]
  }

  #requireOpen(): void {
    if (this.#closed) throw new DOMException('Persistent tree session is closed', 'InvalidStateError')
  }

  #reportNeedsAttention(): void {
    if (this.#needsAttentionReported) return
    this.#needsAttentionReported = true
    try {
      this.#trace(Object.freeze({
        name: 'receive.operation.needs_attention',
        operation_id: this.#checkpoints.binding.operationId,
        prior_state: 'receiving',
        needs_attention_reason: 'target-ownership-unknown',
      }))
    } catch {
      // Ownership failures remain authoritative even if telemetry is unavailable.
    }
  }
}

function snapshotOpenedRevision(input: OpenedFileRevision): OpenedFileRevision {
  identityBytes(input.fileId, 16, 'file ID')
  identityBytes(input.fileRevision, 16, 'file revision')
  if (typeof input.exactSize !== 'bigint' || input.exactSize < 0n ||
      input.exactSize > 0xffff_ffff_ffff_ffffn) {
    throw new TypeError('Opened file revision exact size is invalid')
  }
  return Object.freeze({ ...input })
}

function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((segment, index) => segment === right[index])
}

export type {
  PersistentMaterializationPort,
  PersistentFileRequest,
  PersistentDirectoryMaterialization,
} from './contracts'
