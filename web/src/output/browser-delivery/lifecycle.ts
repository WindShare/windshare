import { fileCheckpointIsComplete, fileCheckpointDigest, type FileCheckpointV2 } from '../persistence/checkpoint'
import type { BrowserDeliveryRecordV1, BrowserDeliveryState, BrowserSavePolicyV1 } from './model'
import { localMutationFields, snapshotBrowserDeliveryRecord, validateBrowserDeliveryRecord } from './records'

const NEXT_STATES: Readonly<Record<BrowserDeliveryState['kind'], readonly BrowserDeliveryState['kind'][]>> = {
  receiving: ['receiving', 'staged-complete', 'target-saved', 'discarding', 'restart-authorized'],
  'restart-authorized': ['receiving', 'discarding'],
  'staged-complete': ['copying', 'discarding'],
  copying: ['copying', 'staged-complete', 'target-saved', 'discarding'],
  'target-saved': ['cleanup-pending', 'cleaned'],
  'cleanup-pending': ['cleanup-pending', 'cleaned'],
  cleaned: [],
  discarding: ['discarded'],
  discarded: [],
}

export function advanceBrowserDeliveryRecord(
  policy: BrowserSavePolicyV1,
  previousInput: BrowserDeliveryRecordV1,
  state: BrowserDeliveryState,
): BrowserDeliveryRecordV1 {
  const previous = validateBrowserDeliveryRecord(policy, previousInput)
  const next = snapshotBrowserDeliveryRecord(policy, { ...previous, generation: previous.generation + 1n, state })
  assertBrowserDeliveryTransition(policy, previous, next)
  return next
}

/** Only this named transition permits replacing received coverage with a fresh owned-file epoch. */
export function authorizeBrowserDeliveryRestart(
  policy: BrowserSavePolicyV1, previous: BrowserDeliveryRecordV1,
  checkpoint: FileCheckpointV2, authorizationId: string,
): BrowserDeliveryRecordV1 {
  if (previous.placement !== 'direct' || previous.state.kind !== 'receiving' ||
      checkpoint.verifiedRanges.length === 0 || fileCheckpointIsComplete(checkpoint)) {
    throw new TypeError('Explicit restart requires an incomplete direct file with durable progress')
  }
  return advanceBrowserDeliveryRecord(policy, previous, { kind: 'restart-authorized', checkpoint, authorizationId })
}

export function assertBrowserDeliveryTransition(
  policy: BrowserSavePolicyV1,
  previousInput: BrowserDeliveryRecordV1,
  nextInput: BrowserDeliveryRecordV1,
): void {
  const previous = validateBrowserDeliveryRecord(policy, previousInput)
  const next = validateBrowserDeliveryRecord(policy, nextInput)
  assertImmutableFile(previous, next)
  assertDurablePhase(previous, next)
  assertStableProof(stageCheckpoint(previous.state), stageCheckpoint(next.state), 'Complete staging')
  assertStableProof(targetCheckpoint(previous.state), targetCheckpoint(next.state), 'Saved target')
  assertReceivingCoverage(previous.state, next.state)
  assertCopyAttempt(previous.state, next.state)
  assertAuthorizedRestart(previous.state, next.state)
  const beforeStage = stageCheckpoint(previous.state)
  if (next.state.kind === 'discarding' && beforeStage !== undefined &&
      (next.state.checkpoint === undefined || fileCheckpointDigest(next.state.checkpoint) !== fileCheckpointDigest(beforeStage))) {
    throw new TypeError('Explicit discard must retain the complete owned staging checkpoint until deletion')
  }
}

export function stageCheckpoint(state: BrowserDeliveryState): FileCheckpointV2 | undefined {
  return 'stage' in state ? state.stage : undefined
}

export function targetCheckpoint(state: BrowserDeliveryState): FileCheckpointV2 | undefined {
  return 'target' in state ? state.target : undefined
}

function assertImmutableFile(previous: BrowserDeliveryRecordV1, next: BrowserDeliveryRecordV1): void {
  if (next.generation !== previous.generation + 1n || next.fileId !== previous.fileId ||
      next.placement !== previous.placement || next.placementReason !== previous.placementReason ||
      next.source.fileRevision !== previous.source.fileRevision || next.source.exactSize !== previous.source.exactSize ||
      JSON.stringify(next.source.canonicalPath) !== JSON.stringify(previous.source.canonicalPath) ||
      JSON.stringify(next.materializationRelativePath) !== JSON.stringify(previous.materializationRelativePath) ||
      JSON.stringify(localMutationFields(next.localMutation)) !== JSON.stringify(localMutationFields(previous.localMutation))) {
    throw new TypeError('Browser delivery transition changed immutable placement/source or generation')
  }
}

function assertDurablePhase(previous: BrowserDeliveryRecordV1, next: BrowserDeliveryRecordV1): void {
  if (!NEXT_STATES[previous.state.kind].includes(next.state.kind)) {
    throw new TypeError('Browser delivery skipped a durable phase')
  }
  if (previous.placement !== 'staged') return
  if (previous.state.kind === 'receiving' && next.state.kind === 'target-saved') {
    throw new TypeError('Staged delivery must persist complete staging before target copy')
  }
  if (previous.state.kind === 'target-saved' && next.state.kind === 'cleaned') {
    throw new TypeError('Staging deletion requires a durable cleanup-pending cut')
  }
}

function assertStableProof(previous: FileCheckpointV2 | undefined, next: FileCheckpointV2 | undefined, label: string): void {
  if (previous !== undefined && next !== undefined && fileCheckpointDigest(previous) !== fileCheckpointDigest(next)) {
    throw new TypeError(label + ' proof cannot change during delivery cleanup')
  }
}

function assertReceivingCoverage(previous: BrowserDeliveryState, next: BrowserDeliveryState): void {
  if (previous.kind !== 'receiving' || previous.checkpoint === undefined) return
  let checkpoint = targetCheckpoint(next)
  if (next.kind === 'receiving' || next.kind === 'discarding' || next.kind === 'restart-authorized') checkpoint = next.checkpoint
  else if (next.kind === 'staged-complete') checkpoint = next.stage
  if (checkpoint === undefined || checkpoint.recordId !== previous.checkpoint.recordId ||
      checkpoint.checkpointGeneration < previous.checkpoint.checkpointGeneration ||
      !previous.checkpoint.verifiedRanges.every(range =>
        checkpoint.verifiedRanges.some(nextRange => nextRange.start <= range.start && nextRange.end >= range.end))) {
    throw new TypeError('Browser delivery cannot discard durable receiving coverage or change object ownership')
  }
}

function assertAuthorizedRestart(previous: BrowserDeliveryState, next: BrowserDeliveryState): void {
  if (previous.kind !== 'restart-authorized' || next.kind !== 'receiving') return
  const reset = next.checkpoint
  if (reset === undefined || reset.recordId !== previous.checkpoint.recordId || reset.verifiedRanges.length !== 0 ||
      reset.checkpointGeneration <= previous.checkpoint.checkpointGeneration ||
      reset.stateGeneration <= previous.checkpoint.stateGeneration) {
    throw new TypeError('Authorized restart requires the same owned file with a newer empty durable checkpoint')
  }
}

function assertCopyAttempt(previous: BrowserDeliveryState, next: BrowserDeliveryState): void {
  if (previous.kind !== 'copying') return
  if (next.kind === 'copying' && (previous.attempt.attemptId !== next.attempt.attemptId ||
      (previous.attempt.targetOwnedObjectId !== undefined &&
        previous.attempt.targetOwnedObjectId !== next.attempt.targetOwnedObjectId))) {
    throw new TypeError('Active target attempt must be drained before replacement')
  }
  if (next.kind === 'target-saved' && previous.attempt.targetOwnedObjectId !== undefined &&
      previous.attempt.targetOwnedObjectId !== next.target.ownedObjectId) {
    throw new TypeError('Saved target differs from the owned copy attempt')
  }
}
