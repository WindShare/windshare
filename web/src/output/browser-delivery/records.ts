import { snapshotSourceAuthenticationPath, snapshotMaterializationRootRelativePath } from '../../transfer/job/coordinate/direct-tree'
import {
  FILE_CHECKPOINT_COMMIT_VERIFIED, FILE_CHECKPOINT_ID_BYTES, FILE_CHECKPOINT_MAX_FILE_SIZE,
  FILE_ID_BYTES, FILE_REVISION_BYTES, fileCheckpointDigest, fileCheckpointIsComplete,
  newFileCheckpointV2, validateFileCheckpoint, type FileCheckpointV2,
} from '../persistence/checkpoint'
import { sameDurableCheckpointNamespace, type DurableCheckpointNamespaceIdentity } from '../persistence/namespace'
import { snapshotIdentity } from '../workspace/canonical'
import { deliveryDigest, deliveryReason } from './codec'
import {
  BROWSER_DELIVERY_RECORD_VERSION, type BrowserDeliveryRecordV1, type BrowserDeliverySource,
  type BrowserDeliveryState, type BrowserFilePlacement, type BrowserSavePolicyV1,
} from './model'
import { validateBrowserSavePolicy } from './policy'

export function createBrowserDeliveryRecord(input: {
  readonly policy: BrowserSavePolicyV1
  readonly source: BrowserDeliverySource
  readonly materializationRelativePath: readonly string[]
  readonly placement: BrowserFilePlacement
  readonly placementReason: string
}): BrowserDeliveryRecordV1 {
  return snapshotBrowserDeliveryRecord(input.policy, {
    schemaVersion: BROWSER_DELIVERY_RECORD_VERSION,
    operationId: input.policy.operationId,
    policyDigest: input.policy.digest,
    fileId: input.source.fileId,
    source: input.source,
    materializationRelativePath: input.materializationRelativePath,
    placement: input.placement,
    placementReason: input.placementReason,
    generation: 1n,
    state: { kind: 'receiving' },
  })
}

export function validateBrowserDeliveryRecord(
  policy: BrowserSavePolicyV1,
  input: BrowserDeliveryRecordV1,
): BrowserDeliveryRecordV1 {
  const value = snapshotBrowserDeliveryRecord(policy, input)
  if (input.digest !== value.digest) throw new TypeError('Browser delivery record digest mismatch')
  return value
}

export function snapshotBrowserDeliveryRecord(
  policyInput: BrowserSavePolicyV1,
  input: Omit<BrowserDeliveryRecordV1, 'digest'>,
): BrowserDeliveryRecordV1 {
  const policy = validateBrowserSavePolicy(policyInput)
  if (input.schemaVersion !== BROWSER_DELIVERY_RECORD_VERSION || input.operationId !== policy.operationId ||
      input.policyDigest !== policy.digest || input.fileId !== input.source.fileId) {
    throw new TypeError('Browser delivery escaped its immutable policy or file identity')
  }
  if ((input.placement !== 'direct' && input.placement !== 'staged') ||
      (input.placement === 'staged' && (policy.preference === 'direct' || policy.staging === undefined))) {
    throw new TypeError('Browser delivery placement is not authorized by save policy')
  }
  if (typeof input.generation !== 'bigint' || input.generation < 1n ||
      input.generation > FILE_CHECKPOINT_MAX_FILE_SIZE) {
    throw new TypeError('Browser delivery generation is invalid')
  }
  const source = snapshotSource(input.source)
  const materializationRelativePath = snapshotMaterializationRootRelativePath(input.materializationRelativePath)
  const state = snapshotState(policy, source, materializationRelativePath, input.placement, input.state)
  const localMutation = snapshotLocalMutation(policy, source, materializationRelativePath, input.localMutation)
  const value = {
    schemaVersion: BROWSER_DELIVERY_RECORD_VERSION, operationId: policy.operationId,
    policyDigest: policy.digest, fileId: source.fileId, source, materializationRelativePath,
    ...(localMutation === undefined ? {} : { localMutation }), placement: input.placement,
    placementReason: deliveryReason(input.placementReason), generation: input.generation, state,
  } as const
  return Object.freeze({
    ...value,
    digest: deliveryDigest('windshare/browser-file-delivery/v1', [
      value.schemaVersion, value.operationId, value.policyDigest, source.fileId, source.fileRevision,
      source.canonicalPath, source.exactSize.toString(), materializationRelativePath, localMutationFields(localMutation),
      value.placement, value.placementReason,
      value.generation.toString(), stateFields(state),
    ]),
  })
}

export function browserDeliveryStagingPath(fileId: string): readonly string[] {
  return Object.freeze([snapshotIdentity(fileId, FILE_ID_BYTES, 'file ID')])
}

function snapshotLocalMutation(
  policy: BrowserSavePolicyV1,
  source: BrowserDeliverySource,
  path: readonly string[],
  input: BrowserDeliveryRecordV1['localMutation'],
): BrowserDeliveryRecordV1['localMutation'] {
  if (input === undefined) return undefined
  if (typeof input.lifecycleGeneration !== 'bigint' || input.lifecycleGeneration < 1n ||
      input.lifecycleGeneration > FILE_CHECKPOINT_MAX_FILE_SIZE) {
    throw new TypeError('Local delivery mutation requires a lifecycle generation')
  }
  return Object.freeze({
    lifecycleGeneration: input.lifecycleGeneration,
    checkpointSetDigest: snapshotIdentity(input.checkpointSetDigest, FILE_CHECKPOINT_ID_BYTES, 'checkpoint set digest'),
    ...(input.priorTargetCheckpoint === undefined ? {} : {
      priorTargetCheckpoint: snapshotCheckpoint(policy.target, source, path, input.priorTargetCheckpoint, false),
    }),
  })
}

export function localMutationFields(input: BrowserDeliveryRecordV1['localMutation']): readonly unknown[] | null {
  return input === undefined ? null : [
    input.lifecycleGeneration.toString(), input.checkpointSetDigest,
    input.priorTargetCheckpoint === undefined ? null : fileCheckpointDigest(input.priorTargetCheckpoint),
  ]
}

function snapshotSource(source: BrowserDeliverySource): BrowserDeliverySource {
  if (typeof source.exactSize !== 'bigint' || source.exactSize < 0n ||
      source.exactSize > FILE_CHECKPOINT_MAX_FILE_SIZE) throw new TypeError('Invalid authenticated file size')
  return Object.freeze({
    fileId: snapshotIdentity(source.fileId, FILE_ID_BYTES, 'file ID'),
    fileRevision: snapshotIdentity(source.fileRevision, FILE_REVISION_BYTES, 'file revision'),
    canonicalPath: snapshotSourceAuthenticationPath(source.canonicalPath),
    exactSize: source.exactSize,
  })
}

function snapshotState(
  policy: BrowserSavePolicyV1,
  source: BrowserDeliverySource,
  materializationRelativePath: readonly string[],
  placement: BrowserFilePlacement,
  state: BrowserDeliveryState,
): BrowserDeliveryState {
  const target = (value: FileCheckpointV2) => snapshotCheckpoint(policy.target, source, materializationRelativePath, value, true)
  const stage = (value: FileCheckpointV2) => {
    if (placement !== 'staged' || policy.staging === undefined) throw new TypeError('Direct receiving has no staging proof')
    return snapshotCheckpoint(policy.staging, source, browserDeliveryStagingPath(source.fileId), value, true)
  }
  switch (state.kind) {
    case 'receiving':
    case 'discarding': return snapshotReceivingState(policy, source, materializationRelativePath, placement, state)
    case 'restart-authorized': return snapshotRestartState(policy, source, materializationRelativePath, placement, state)
    case 'staged-complete': return Object.freeze({
      kind: state.kind, stage: stage(state.stage),
      ...(state.failureReason === undefined ? {} : { failureReason: deliveryReason(state.failureReason) }),
    })
    case 'copying': return Object.freeze({
      kind: state.kind, stage: stage(state.stage),
      attempt: Object.freeze({
        attemptId: deliveryReason(state.attempt.attemptId),
        ...(state.attempt.targetOwnedObjectId === undefined ? {} : {
          targetOwnedObjectId: snapshotIdentity(state.attempt.targetOwnedObjectId, FILE_CHECKPOINT_ID_BYTES, 'target object ID'),
        }),
      }),
    })
    case 'target-saved':
      if ((placement === 'staged') !== (state.stage !== undefined)) throw new TypeError('Saved delivery lost its staging ownership')
      return Object.freeze({
        kind: state.kind, target: target(state.target),
        ...(state.stage === undefined ? {} : { stage: stage(state.stage) }),
      })
    case 'cleanup-pending': return Object.freeze({
      kind: state.kind, target: target(state.target), stage: stage(state.stage),
      ...(state.failureReason === undefined ? {} : { failureReason: deliveryReason(state.failureReason) }),
    })
    case 'cleaned': return Object.freeze({ kind: state.kind, target: target(state.target) })
    case 'discarded': return Object.freeze({ kind: state.kind })
    default: throw new TypeError('Unknown browser delivery state')
  }
}

function snapshotRestartState(
  policy: BrowserSavePolicyV1, source: BrowserDeliverySource, path: readonly string[],
  placement: BrowserFilePlacement, state: Extract<BrowserDeliveryState, { kind: 'restart-authorized' }>,
): BrowserDeliveryState {
  if (placement !== 'direct' || state.checkpoint.verifiedRanges.length === 0 || fileCheckpointIsComplete(state.checkpoint)) {
    throw new TypeError('Restart authorization only applies to incomplete direct target progress')
  }
  return Object.freeze({
    kind: state.kind, authorizationId: deliveryReason(state.authorizationId),
    checkpoint: snapshotCheckpoint(policy.target, source, path, state.checkpoint, false),
  })
}

function snapshotReceivingState(
  policy: BrowserSavePolicyV1,
  source: BrowserDeliverySource,
  materializationRelativePath: readonly string[],
  placement: BrowserFilePlacement,
  state: Extract<BrowserDeliveryState, { kind: 'receiving' | 'discarding' }>,
): BrowserDeliveryState {
  if (state.checkpoint === undefined) return Object.freeze({ kind: state.kind })
  return Object.freeze({
    kind: state.kind,
    checkpoint: snapshotCheckpoint(
      placement === 'staged' ? policy.staging! : policy.target, source,
      placement === 'staged' ? browserDeliveryStagingPath(source.fileId) : materializationRelativePath, state.checkpoint, false,
    ),
  })
}

function snapshotCheckpoint(
  namespace: DurableCheckpointNamespaceIdentity,
  source: BrowserDeliverySource,
  checkpointPath: readonly string[],
  input: FileCheckpointV2,
  complete: boolean,
): FileCheckpointV2 {
  validateFileCheckpoint(input)
  if (!sameDurableCheckpointNamespace(namespace, input) || input.fileId !== source.fileId ||
      input.fileRevision !== source.fileRevision || input.exactSize !== source.exactSize ||
      JSON.stringify(input.canonicalPath) !== JSON.stringify(checkpointPath) ||
      input.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED || (complete && !fileCheckpointIsComplete(input))) {
    throw new TypeError('Browser delivery checkpoint does not prove its bound source and storage')
  }
  return newFileCheckpointV2(input)
}

function stateFields(state: BrowserDeliveryState): readonly unknown[] {
  switch (state.kind) {
    case 'receiving':
    case 'discarding': return [state.kind, state.checkpoint === undefined ? null : fileCheckpointDigest(state.checkpoint)]
    case 'discarded': return [state.kind]
    case 'restart-authorized': return [state.kind, fileCheckpointDigest(state.checkpoint), state.authorizationId]
    case 'staged-complete': return [state.kind, fileCheckpointDigest(state.stage), state.failureReason ?? null]
    case 'copying': return [state.kind, fileCheckpointDigest(state.stage), state.attempt.attemptId, state.attempt.targetOwnedObjectId ?? null]
    case 'target-saved': return [state.kind, fileCheckpointDigest(state.target), state.stage === undefined ? null : fileCheckpointDigest(state.stage)]
    case 'cleanup-pending': return [state.kind, fileCheckpointDigest(state.target), fileCheckpointDigest(state.stage), state.failureReason ?? null]
    case 'cleaned': return [state.kind, fileCheckpointDigest(state.target)]
  }
}
