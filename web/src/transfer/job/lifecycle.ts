import type { ReceiveLifecycleState } from '../../output/workspace/state'
import type { TransferJobOptions } from './contract'
import type {
  CompletedTransferWorkerSettlement,
  SuccessfulTransferWorkerSettlement,
  TransferWorkerSettlement,
} from '../outcome'

const DIRECT_TREE_PAUSE_STATES: ReadonlySet<ReceiveLifecycleState['kind']> = new Set([
  'resumable-receive',
  'partial-directory',
  'discarded',
  'needs-attention',
])

const DIRECT_ATOMIC_PAUSE_STATES: ReadonlySet<ReceiveLifecycleState['kind']> = new Set([
  'restart-required',
  'discarded',
  'needs-attention',
])

const DIRECT_ZIP_PAUSE_STATES: ReadonlySet<ReceiveLifecycleState['kind']> = new Set([
  'resumable-receive',
  'authorization-required',
  'target-verification-required',
  'destination-space-required',
  'restart-required',
  'discarded',
  'needs-attention',
])

const WORKSPACE_RECEIVE_PAUSE_STATES: ReadonlySet<ReceiveLifecycleState['kind']> = new Set([
  'resumable-receive',
  'discarded',
  'needs-attention',
])

const WORKSPACE_POST_MATERIALIZATION_PAUSE_STATES: ReadonlySet<
  ReceiveLifecycleState['kind']
> = new Set([
  ...WORKSPACE_RECEIVE_PAUSE_STATES,
  'resumable-package',
  'waiting-to-save',
  'download-started',
])

const PORTABLE_RECEIVE_PAUSE_STATES: ReadonlySet<ReceiveLifecycleState['kind']> = new Set([
  'restart-required',
  'discarded',
  'needs-attention',
])

const PORTABLE_POST_MATERIALIZATION_PAUSE_STATES: ReadonlySet<
  ReceiveLifecycleState['kind']
> = new Set([
  ...PORTABLE_RECEIVE_PAUSE_STATES,
  'download-started',
])

const PREPARATION_REJECTION_STATES: ReadonlySet<ReceiveLifecycleState['kind']> = new Set([
  'discarded',
  'needs-attention',
])

const WORKSPACE_POST_MATERIALIZATION_STATES: ReadonlySet<ReceiveLifecycleState['kind']> = new Set([
  'materialization-sealed',
  'packaging',
  'resumable-package',
  'artifact-sealed',
  'waiting-to-save',
  'publishing-managed',
  'handing-off',
  'published',
  'download-started',
  'restart-required',
  'discarded',
  'expired',
  'needs-attention',
])

export function requireSuccessfulWorker(
  worker: CompletedTransferWorkerSettlement,
): SuccessfulTransferWorkerSettlement {
  if (worker.status !== 'Succeeded') {
    throw new TypeError('complete artifact settlement requires successful workers')
  }
  return worker
}

export function validateCompletionLifecycle(
  intent: TransferJobOptions['intent'],
  worker: CompletedTransferWorkerSettlement,
  state: ReceiveLifecycleState,
): ReceiveLifecycleState {
  validateLifecycleIdentity(intent, state)
  switch (intent.plan.kind) {
    case 'direct-tree':
      if (worker.status === 'Succeeded' && state.kind !== 'published') {
        throw new TypeError('successful DirectTree settlement must be published')
      }
      if (worker.status === 'CompletedWithErrors' && state.kind !== 'partial-directory' &&
          state.kind !== 'resumable-receive') {
        throw new TypeError('partial DirectTree settlement must remain partial or resumable')
      }
      break
    case 'direct-atomic':
      if (state.kind !== 'published') throw new TypeError('DirectAtomic settlement must be published')
      break
    case 'direct-resumable-zip':
      if (state.kind !== 'published') {
        throw new TypeError('successful Direct ZIP settlement must be verified publication')
      }
      break
    case 'portable-handoff':
      if (state.kind !== 'download-started' || state.attemptKind !== 'portable') {
        throw new TypeError('PortableHandoff settlement can prove only portable DownloadStarted')
      }
      break
    case 'workspace-then-publish':
      if (!WORKSPACE_POST_MATERIALIZATION_STATES.has(state.kind)) {
        throw new TypeError('workspace settlement regressed before materialization completion')
      }
      break
  }
  return state
}

export function validatePauseLifecycle(
  intent: TransferJobOptions['intent'],
  worker: TransferWorkerSettlement,
  state: ReceiveLifecycleState,
): ReceiveLifecycleState {
  validateLifecycleIdentity(intent, state)
  let allowed: ReadonlySet<ReceiveLifecycleState['kind']>
  switch (intent.plan.kind) {
    case 'direct-tree':
      allowed = DIRECT_TREE_PAUSE_STATES
      break
    case 'direct-atomic':
      allowed = DIRECT_ATOMIC_PAUSE_STATES
      break
    case 'direct-resumable-zip':
      allowed = DIRECT_ZIP_PAUSE_STATES
      break
    case 'workspace-then-publish':
      allowed = worker.status === 'Succeeded'
        ? WORKSPACE_POST_MATERIALIZATION_PAUSE_STATES
        : WORKSPACE_RECEIVE_PAUSE_STATES
      break
    case 'portable-handoff':
      allowed = worker.status === 'Succeeded'
        ? PORTABLE_POST_MATERIALIZATION_PAUSE_STATES
        : PORTABLE_RECEIVE_PAUSE_STATES
      break
  }
  if (!allowed.has(state.kind)) {
    throw new TypeError('plan pause returned a lifecycle state unavailable to that plan stage')
  }
  if (state.kind === 'resumable-receive' &&
      ((intent.plan.kind === 'direct-resumable-zip') !== (state.payloadKind === 'direct-zip'))) {
    throw new TypeError('plan pause returned a checkpoint payload owned by another plan')
  }
  return state
}

export function validateStopLifecycle(
  intent: TransferJobOptions['intent'],
  state: ReceiveLifecycleState,
): ReceiveLifecycleState {
  validateLifecycleIdentity(intent, state)
  if (intent.plan.kind !== 'direct-tree' || state.kind !== 'partial-directory' ||
      state.reason !== 'stopped') {
    throw new TypeError('Stop must retain an ordinary stopped DirectTree partial result')
  }
  return state
}

export function validatePreparationRejectionLifecycle(
  intent: TransferJobOptions['intent'],
  state: ReceiveLifecycleState,
): ReceiveLifecycleState {
  validateLifecycleIdentity(intent, state)
  if ((intent.plan.kind !== 'workspace-then-publish' && intent.plan.kind !== 'portable-handoff') ||
      !PREPARATION_REJECTION_STATES.has(state.kind)) {
    throw new TypeError('preparation rejection did not prove cleanup or uncertain ownership')
  }
  return state
}

export function failedTreeOutcome(
  state: ReceiveLifecycleState,
): 'partial-directory' | 'discarded' | undefined {
  if (state.kind === 'partial-directory' || state.kind === 'resumable-receive') {
    return 'partial-directory'
  }
  return state.kind === 'discarded' ? 'discarded' : undefined
}

function validateLifecycleIdentity(
  intent: TransferJobOptions['intent'],
  state: ReceiveLifecycleState,
): void {
  if (state.operationId !== intent.operationId || state.receiveIntentDigest !== intent.digest) {
    throw new TypeError('lifecycle settlement belongs to another receive operation')
  }
}
