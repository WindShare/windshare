import { ORIGIN_PRIVATE_PACKAGE_CONTAINER, OriginPrivateWorkspaceRoot } from '../../origin-private/workspace-root'
import type { TaskObjectRef } from '../../origin-private/task-checkpoint/model'
import { IndexedDbTaskCheckpointStore } from '../../origin-private/task-checkpoint/indexeddb-store'
import type { TaskCheckpointStore } from '../../origin-private/task-checkpoint/store'
import type { CompleteZipEntrySpan } from '../../progressive-zip/archive'
import { completeZipEntryCrc } from '../../progressive-zip/crc-ranges'
import { TargetOwnershipUnknownError } from '../../persistent-tree/errors'
import type { WorkspaceContinuationInput } from './workspace-continuation-authority'

const PARTIAL_READER_PAGE_SIZE = 128

export interface RetainedZipPartialReader {
  readonly object: TaskObjectRef
  readonly handle: FileSystemFileHandle
  completeEntries(): AsyncIterable<CompleteZipEntrySpan>
  close(): Promise<void>
}

/** Read-only recovery never admits a write, reserves quota, or opens a native writer. */
export async function openRetainedZipPartialReader(
  input: WorkspaceContinuationInput, object: TaskObjectRef, databaseName?: string,
): Promise<RetainedZipPartialReader> {
  const intent = input.snapshot.operation.receiveIntent
  if (intent.plan.kind !== 'workspace-then-publish') throw new TypeError('ZIP reader needs workspace authority')
  const root = new OriginPrivateWorkspaceRoot({
    operationId: intent.operationId, receiveIntentDigest: intent.digest,
    workspaceBindingDigest: intent.plan.workspace.digest, authorityRef: intent.plan.workspace.repositoryRef,
    workspaceRootHandleId: input.target.namespace.rootHandleId, workspaceRootHandle: input.target.namespace.root,
    repository: input.repository,
  })
  await root.authorize()
  const saved = await input.repository.readHandle<FileSystemFileHandle>(object.handleId)
  const handle = await root.readObject(ORIGIN_PRIVATE_PACKAGE_CONTAINER, object.objectId, 'writer-open')
  if (saved === undefined || handle === undefined || saved.operationId !== object.operationId ||
      saved.ownedObjectId !== object.objectId || saved.authorityRef !== intent.plan.workspace.repositoryRef ||
      !await handle.isSameEntry(saved.handle)) {
    throw new TargetOwnershipUnknownError('writer-open', object.operationId)
  }
  const store = await IndexedDbTaskCheckpointStore.open(object, databaseName)
  let closed = false
  return {
    object, handle,
    completeEntries: () => {
      if (closed) throw new DOMException('ZIP reader is closed', 'InvalidStateError')
      return readCompleteZipEntries(store)
    },
    close: async () => { if (!closed) { closed = true; store.close() } },
  }
}

export async function* readCompleteZipEntries(store: TaskCheckpointStore): AsyncGenerator<CompleteZipEntrySpan> {
  let afterSequence: bigint | undefined
  for (;;) {
    const entries = await store.readEntries({
      ...(afterSequence === undefined ? {} : { afterSequence }), limit: PARTIAL_READER_PAGE_SIZE,
    })
    if (entries.length === 0) return
    for (const entry of entries) {
      const layout = entry.zipLayout
      if (layout === undefined) throw new TypeError('Retained ZIP entry lost its layout')
      afterSequence = layout.sequence
      const crc32 = completeZipEntryCrc(entry.ranges, layout.exactSize)
      if (crc32 !== undefined) yield { entry, payloadOffset: layout.payloadOffset, length: layout.exactSize, crc32 }
    }
  }
}
