import { validateReceiveIntent, type ReceiveIntent } from '../../transfer/intent'
import { canonicalDigest, canonicalFrame, canonicalIdentity, canonicalRecord } from '../workspace/canonical'
import { receiveOperationHandleRecord } from '../workspace/records'
import { WORKSPACE_HANDLE_PACKAGE_OBJECT, type WorkspaceStageTraceListener } from '../workspace/stages'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import { ProgressiveZipArchive } from '../progressive-zip/archive'
import { openNativeObject } from './native-object/client'
import type { NativeObjectFactory } from './native-object/contracts'
import { ObjectCheckpointCoordinator } from './native-object/coordinator'
import { IndexedDbTaskCheckpointStore } from './task-checkpoint/indexeddb-store'
import type { TaskObjectRef } from './task-checkpoint/model'
import { originPrivatePackageHandleId } from './package-store'
import { openOriginPrivateWorkspaceBackend, type OriginPrivateWorkspaceBackend } from './session'
import { ORIGIN_PRIVATE_PACKAGE_CONTAINER, OriginPrivateWorkspaceRoot } from './workspace-root'

export interface OriginPrivateProgressiveZipBackend extends OriginPrivateWorkspaceBackend {
  readonly archive: ProgressiveZipArchive
  readonly object: TaskObjectRef
  readonly store: IndexedDbTaskCheckpointStore
  readonly handle: FileSystemFileHandle
}

export async function progressiveZipObjectRef(intent: ReceiveIntent): Promise<TaskObjectRef> {
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind !== 'zip-archive') {
    throw new TypeError('Progressive ZIP object requires its immutable workspace intent')
  }
  const objectId = await canonicalDigest(canonicalRecord('windshare/opfs-zip-object/v1', 1, [
    canonicalFrame(canonicalIdentity(intent.operationId, 16, 'operation ID')),
    canonicalFrame(canonicalIdentity(intent.digest, 32, 'receive intent digest')),
  ]))
  return Object.freeze({
    operationId: intent.operationId, objectId, kind: 'zip-archive',
    handleId: originPrivatePackageHandleId(intent.operationId, objectId),
  })
}

/** The one package object is allocated once and remains the payload and final artifact. */
export async function openOriginPrivateProgressiveZipBackend(options:
  Parameters<typeof openOriginPrivateWorkspaceBackend>[0] & {
    readonly nativeObjectFactory?: NativeObjectFactory
    readonly onNativeTrace?: WorkspaceStageTraceListener
  },
): Promise<OriginPrivateProgressiveZipBackend> {
  const intent = await validateReceiveIntent(options.receiveIntent)
  if (intent.plan.kind !== 'workspace-then-publish') throw new TypeError('Expected workspace intent')
  const object = await progressiveZipObjectRef(intent)
  const backend = await openOriginPrivateWorkspaceBackend(options)
  let store: IndexedDbTaskCheckpointStore | undefined
  let coordinator: ObjectCheckpointCoordinator | undefined
  try {
    const root = new OriginPrivateWorkspaceRoot({
      operationId: intent.operationId, receiveIntentDigest: intent.digest,
      workspaceBindingDigest: intent.plan.workspace.digest, authorityRef: intent.plan.workspace.repositoryRef,
      workspaceRootHandleId: options.namespace.rootHandleId, workspaceRootHandle: options.namespace.root,
      repository: options.operationRepository, contentGate: options.contentGate, budgetClaim: options.budgetClaim,
    })
    await root.authorize()
    const saved = await options.operationRepository.readHandle<FileSystemFileHandle>(object.handleId)
    let handle: FileSystemFileHandle
    if (saved === undefined) {
      const pending = await root.readObject(ORIGIN_PRIVATE_PACKAGE_CONTAINER, object.objectId, 'writer-open')
      if (pending !== undefined && (await pending.getFile()).size !== 0) {
        throw new TargetOwnershipUnknownError('writer-open', object.operationId)
      }
      // A crash between creating the deterministic task object and journaling its handle
      // can leave only an empty object: mutable writer authority is acquired after the journal.
      handle = pending ?? await root.createObject(ORIGIN_PRIVATE_PACKAGE_CONTAINER, object.objectId)
      const lease = await options.operationRepository.readLease(intent.operationId)
      if (lease === undefined) throw new TypeError('ZIP allocation lost operation ownership')
      await options.operationRepository.commitTransition({
        operationId: intent.operationId, expectedLeaseId: lease.leaseId,
        handles: [receiveOperationHandleRecord({
          id: object.handleId, operationId: object.operationId, kind: WORKSPACE_HANDLE_PACKAGE_OBJECT,
          authorityRef: intent.plan.workspace.repositoryRef, ownedObjectId: object.objectId, handle,
        })],
      })
    } else {
      const current = await root.readObject(ORIGIN_PRIVATE_PACKAGE_CONTAINER, object.objectId, 'writer-open')
      if (saved.operationId !== object.operationId || saved.ownedObjectId !== object.objectId ||
          saved.authorityRef !== intent.plan.workspace.repositoryRef || current === undefined ||
          !await current.isSameEntry(saved.handle)) {
        throw new TargetOwnershipUnknownError('writer-open', object.operationId)
      }
      handle = current
    }
    const activeLease = await options.operationRepository.readLease(intent.operationId)
    if (activeLease === undefined) throw new TypeError('ZIP writer lost operation ownership')
    store = await IndexedDbTaskCheckpointStore.open(object, options.checkpointDatabaseName, activeLease.leaseId)
    const io = await (options.nativeObjectFactory ?? openNativeObject)(handle)
    coordinator = new ObjectCheckpointCoordinator({
      io, operationId: object.operationId, objectId: object.objectId,
      trace: event => options.onNativeTrace?.({
        name: 'receive.opfs.checkpoint', operation_id: event.operationId, object_id: event.objectId,
        stage: event.stage, ...(event.reason === undefined ? {} : { reason: event.reason }),
      }),
    })
    await root.reconcileObject(object.objectId, await coordinator.size())
    const archive = await ProgressiveZipArchive.open({
      object, store, coordinator, capacity: root,
      trace: (event, detail) => options.onNativeTrace?.({
        name: 'receive.opfs.checkpoint', operation_id: object.operationId, object_id: object.objectId,
        stage: event, ...(typeof detail.generation === 'bigint' ? { checkpoint_generation: detail.generation } : {}),
        ...(typeof detail.nextEntry === 'bigint' ? { next_entry: detail.nextEntry } : {}),
        ...(typeof detail.entryCount === 'bigint' ? { entry_count: detail.entryCount } : {}),
        ...(typeof detail.committedLength === 'bigint' ? { committed_length: detail.committedLength } : {}),
      }),
      selectedPaths: intent.selection.rules.mode === 'catalog-path'
        ? intent.selection.rules.paths.map(path => path.split('/')) : [],
    })
    const ownedStore = store
    let closed = false
    return Object.freeze({
      ...backend, materialization: backend.materialization, packages: backend.packages,
      packagedArtifacts: backend.packagedArtifacts, finalCheckpoints: backend.finalCheckpoints,
      cleanup: backend.cleanup, object, archive, store: ownedStore, handle,
      close: async () => {
        if (closed) return
        closed = true
        try { await archive.close() } finally {
          ownedStore.close()
          await backend.close()
        }
      },
    })
  } catch (error) {
    await coordinator?.close().catch(() => undefined)
    store?.close()
    await backend.close().catch(() => undefined)
    throw error
  }
}
