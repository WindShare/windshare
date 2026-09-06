import type { ReceiveIntent } from '../../transfer/intent'
import { progressiveZipObjectRef } from '../origin-private/progressive-backend'
import { IndexedDbTaskCheckpointStore } from '../origin-private/task-checkpoint/indexeddb-store'
import type { TaskCheckpointStore } from '../origin-private/task-checkpoint/store'
import type { ReceiveLifecycleState } from '../workspace/state'
import { verifyProgressiveZipRecovery, verifyProgressiveZipSelection } from './progressive-checkpoint'

const FAILURE_SCAN_PAGE_SIZE = 128
export const SOURCE_REVISION_FAILURE_DISPLAY_LIMIT = 20

export interface SourceRevisionFailure {
  readonly entryId: string
  readonly path: readonly string[]
  readonly sourcePath: readonly string[]
}

export interface SourceRevisionFailures {
  readonly shareInstance: string
  readonly count: bigint
  readonly files: readonly SourceRevisionFailure[]
}

/** Failed revisions remain immutable; these paths authorize only a separate selection. */
export async function projectSourceRevisionFailures(
  store: TaskCheckpointStore, shareInstance: string,
): Promise<SourceRevisionFailures | undefined> {
  let count = 0n
  let afterSequence: bigint | undefined
  const files: SourceRevisionFailure[] = []
  const append = (entry: import('../origin-private/task-checkpoint/model').TaskEntry) => {
    if (entry.kind !== 'file' || !entry.revisionFailure) return
    count++
    if (files.length >= SOURCE_REVISION_FAILURE_DISPLAY_LIMIT) return
    files.push(Object.freeze({
      entryId: entry.entryId, path: Object.freeze([...entry.path]),
      sourcePath: Object.freeze([...entry.source.sourcePath]),
    }))
  }
  for (;;) {
    const entries = await store.readEntries({
      ...(afterSequence === undefined ? {} : { afterSequence }), limit: FAILURE_SCAN_PAGE_SIZE,
    })
    if (entries.length === 0) break
    for (const entry of entries) {
      if (entry.zipLayout === undefined) throw new TypeError('Revision failure projection requires ZIP layout')
      afterSequence = entry.zipLayout.sequence
      append(entry)
    }
  }
  return count === 0n ? undefined : Object.freeze({ shareInstance, count, files: Object.freeze(files) })
}

export async function readSourceRevisionFailures(
  intent: ReceiveIntent, lifecycle: ReceiveLifecycleState, databaseName?: string,
): Promise<SourceRevisionFailures | undefined> {
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind !== 'zip-archive' ||
      intent.plan.preparation !== 'none' ||
      (lifecycle.kind !== 'receiving' && lifecycle.kind !== 'resumable-receive')) return undefined
  const object = await progressiveZipObjectRef(intent)
  const store = await IndexedDbTaskCheckpointStore.open(object, databaseName)
  try {
    const verified = await verifyProgressiveZipRecovery(store, object, lifecycle)
    verifyProgressiveZipSelection(verified.checkpoint, intent)
    return await projectSourceRevisionFailures(store, intent.selection.shareInstance)
  } finally { store.close() }
}
