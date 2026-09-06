import type { ReceiveIntent } from '../../transfer/intent'
import { progressiveZipObjectRef } from '../origin-private/progressive-backend'
import { IndexedDbTaskCheckpointStore } from '../origin-private/task-checkpoint/indexeddb-store'
import { taskEntryComplete, type TaskCheckpoint, type TaskObjectRef } from '../origin-private/task-checkpoint/model'
import type { TaskCheckpointStore } from '../origin-private/task-checkpoint/store'
import type { ReceiveLifecycleState } from '../workspace/state'

export class NativeZipRecoveryUnavailableError extends Error {
  constructor(cause: unknown) {
    super('Retained ZIP recovery metadata is unavailable. Start a new download; retained data has not been changed.', { cause })
    this.name = 'NativeZipRecoveryUnavailableError'
  }
}

const RECOVERY_ENTRY_PAGE_SIZE = 128
export type ProgressiveZipRecoveryRequirement = 'remote-content-needed' | 'local-finalization'

/** Summary counters are presentation facts; local readiness must inspect committed range authority. */
export async function verifyProgressiveZipRecovery(
  store: TaskCheckpointStore, object: TaskObjectRef, lifecycle: ReceiveLifecycleState,
): Promise<{ checkpoint: TaskCheckpoint; requirement: ProgressiveZipRecoveryRequirement }> {
  const checkpoint = await store.readCheckpoint()
  if (checkpoint === undefined || checkpoint.object.operationId !== object.operationId ||
      checkpoint.object.objectId !== object.objectId || checkpoint.object.handleId !== object.handleId ||
      checkpoint.object.kind !== 'zip-archive') throw new TypeError('Native ZIP checkpoint ownership is missing')
  if (lifecycle.kind === 'resumable-receive' &&
      (lifecycle.payloadKind !== 'opfs-zip' || lifecycle.objectId !== object.objectId ||
       lifecycle.checkpointGeneration > checkpoint.generation)) {
    throw new TypeError('Native ZIP lifecycle does not match its committed checkpoint')
  }
  const complete = await checkpointEntriesComplete(store, checkpoint)
  if (checkpoint.artifactState !== 'receiving' && !complete) {
    throw new TypeError('Native ZIP finalization checkpoint has incomplete content')
  }
  return { checkpoint, requirement: complete ? 'local-finalization' : 'remote-content-needed' }
}

async function checkpointEntriesComplete(store: TaskCheckpointStore, checkpoint: TaskCheckpoint): Promise<boolean> {
  let count = 0n
  let complete = checkpoint.discoveryComplete
  let afterSequence: bigint | undefined
  for (;;) {
    const entries = await store.readEntries({
      ...(afterSequence === undefined ? {} : { afterSequence }), limit: RECOVERY_ENTRY_PAGE_SIZE,
    })
    if (entries.length === 0) break
    for (const entry of entries) {
      if (entry.zipLayout === undefined || entry.zipLayout.sequence !== count) {
        throw new TypeError('Native ZIP recovery layout sequence is incomplete')
      }
      complete &&= taskEntryComplete(entry)
      count++
      afterSequence = entry.zipLayout.sequence
    }
  }
  if (count !== checkpoint.entryCount) throw new TypeError('Native ZIP recovery entry authority is incomplete')
  return complete
}

export function verifyProgressiveZipSelection(checkpoint: TaskCheckpoint, intent: ReceiveIntent): void {
  const expected = intent.selection.rules.mode === 'catalog-path'
    ? intent.selection.rules.paths.map(path => path.split('/')) : []
  if (JSON.stringify(checkpoint.selectedPaths) !== JSON.stringify(expected)) {
    throw new TypeError('Native ZIP checkpoint changed its immutable selection')
  }
}

export async function readProgressiveZipRecoveryRequirement(
  intent: ReceiveIntent, lifecycle: ReceiveLifecycleState, databaseName?: string,
): Promise<ProgressiveZipRecoveryRequirement | undefined> {
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind !== 'zip-archive' ||
      intent.plan.preparation !== 'none' ||
      (lifecycle.kind !== 'receiving' && lifecycle.kind !== 'resumable-receive')) return undefined
  const object = await progressiveZipObjectRef(intent)
  const store = await IndexedDbTaskCheckpointStore.open(object, databaseName)
  try {
    const verified = await verifyProgressiveZipRecovery(store, object, lifecycle)
    verifyProgressiveZipSelection(verified.checkpoint, intent)
    return verified.requirement
  } catch (error) {
    if (error instanceof TypeError) throw new NativeZipRecoveryUnavailableError(error)
    throw error
  } finally { store.close() }
}
