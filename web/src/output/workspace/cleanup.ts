import {
  canonicalIdentity,
  snapshotIdentity,
} from './canonical'

export interface WorkspaceOwnedObjectCleanupTarget {
  readonly ownedObjectId: string
  readonly handleId: string
}

export type WorkspaceOwnedObjectCleanupObservation =
  | Readonly<{ kind: 'removed' }>
  | Readonly<{ kind: 'already-absent' }>
  | Readonly<{ kind: 'retryable-failure' }>
  | Readonly<{ kind: 'ownership-unknown' }>

export type WorkspaceCheckpointCleanupObservation =
  | Readonly<{
      kind: 'clean'
      removedRecordDigests: readonly string[]
    }>
  | Readonly<{ kind: 'retryable-failure' }>
  | Readonly<{ kind: 'ownership-unknown' }>

export interface WorkspaceOwnedCleanupPort {
  removeOwnedObject(
    target: WorkspaceOwnedObjectCleanupTarget,
  ): Promise<WorkspaceOwnedObjectCleanupObservation>
  removeFileCheckpoints(input: {
    readonly operationId: string
    readonly receiveIntentDigest: string
  }): Promise<WorkspaceCheckpointCleanupObservation>
}

export type WorkspaceCleanupExecution =
  | Readonly<{
      kind: 'clean'
      cleanedHandleIds: readonly string[]
      removedObjectIds: readonly string[]
      removedCheckpointRecordDigests: readonly string[]
    }>
  | Readonly<{
      kind: 'retryable-failure' | 'ownership-unknown'
      cleanedHandleIds: readonly string[]
      removedObjectIds: readonly string[]
      removedCheckpointRecordDigests: readonly string[]
    }>

/**
 * External deletion is intentionally idempotent and ownership-gated. A crash can replay
 * this sequence, while an uncertain observation can never be upgraded into absence.
 */
export async function executeWorkspaceCleanup(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly targets: readonly WorkspaceOwnedObjectCleanupTarget[]
  readonly port: WorkspaceOwnedCleanupPort
}): Promise<WorkspaceCleanupExecution> {
  const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
  const receiveIntentDigest = snapshotIdentity(
    input.receiveIntentDigest,
    32,
    'receive intent digest',
  )
  const targets = snapshotCleanupTargets(input.targets)
  const cleanedHandleIds: string[] = []
  const removedObjectIds: string[] = []
  for (const target of targets) {
    let observation: WorkspaceOwnedObjectCleanupObservation
    try {
      observation = await input.port.removeOwnedObject(target)
    } catch {
      return cleanupResult('retryable-failure', cleanedHandleIds, removedObjectIds, [])
    }
    if (observation.kind === 'ownership-unknown') {
      return cleanupResult('ownership-unknown', cleanedHandleIds, removedObjectIds, [])
    }
    if (observation.kind === 'retryable-failure') {
      return cleanupResult('retryable-failure', cleanedHandleIds, removedObjectIds, [])
    }
    cleanedHandleIds.push(target.handleId)
    if (observation.kind === 'removed') removedObjectIds.push(target.ownedObjectId)
  }

  let checkpoints: WorkspaceCheckpointCleanupObservation
  try {
    checkpoints = await input.port.removeFileCheckpoints({ operationId, receiveIntentDigest })
  } catch {
    return cleanupResult('retryable-failure', cleanedHandleIds, removedObjectIds, [])
  }
  if (checkpoints.kind !== 'clean') {
    return cleanupResult(checkpoints.kind, cleanedHandleIds, removedObjectIds, [])
  }
  return cleanupResult(
    'clean',
    cleanedHandleIds,
    removedObjectIds,
    snapshotSortedIdentities(checkpoints.removedRecordDigests, 'checkpoint record digest'),
  )
}

function snapshotCleanupTargets(
  input: readonly WorkspaceOwnedObjectCleanupTarget[],
): readonly WorkspaceOwnedObjectCleanupTarget[] {
  const targets = input.map((target) => {
    if (typeof target.handleId !== 'string' || target.handleId.length === 0) {
      throw new TypeError('workspace cleanup handle ID is invalid')
    }
    return Object.freeze({
      ownedObjectId: snapshotIdentity(target.ownedObjectId, 32, 'owned object ID'),
      handleId: target.handleId,
    })
  }).sort((left, right) => compareIdentity(left.ownedObjectId, right.ownedObjectId))
  const handleIds = new Set<string>()
  if (targets.some((target, index) =>
    (index > 0 && target.ownedObjectId === targets[index - 1]?.ownedObjectId) ||
    handleIds.size === handleIds.add(target.handleId).size)) {
    throw new TypeError('workspace cleanup inventory contains duplicate authority')
  }
  return Object.freeze(targets)
}

function cleanupResult(
  kind: WorkspaceCleanupExecution['kind'],
  cleanedHandleIds: readonly string[],
  removedObjectIds: readonly string[],
  removedCheckpointRecordDigests: readonly string[],
): WorkspaceCleanupExecution {
  return Object.freeze({
    kind,
    cleanedHandleIds: Object.freeze([...cleanedHandleIds]),
    removedObjectIds: Object.freeze([...removedObjectIds]),
    removedCheckpointRecordDigests: Object.freeze([...removedCheckpointRecordDigests]),
  })
}

function snapshotSortedIdentities(input: readonly string[], label: string): readonly string[] {
  const values = input.map((value) => snapshotIdentity(value, 32, label)).sort(compareIdentity)
  if (values.some((value, index) => index > 0 && value === values[index - 1])) {
    throw new TypeError(`${label} inventory contains duplicates`)
  }
  return Object.freeze(values)
}

function compareIdentity(left: string, right: string): number {
  const leftBytes = canonicalIdentity(left, 32, 'sortable identity')
  const rightBytes = canonicalIdentity(right, 32, 'sortable identity')
  for (let index = 0; index < leftBytes.length; index += 1) {
    const difference = (leftBytes[index] ?? 0) - (rightBytes[index] ?? 0)
    if (difference !== 0) return difference
  }
  return 0
}
