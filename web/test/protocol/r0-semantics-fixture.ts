import { loadVectorFile, type VectorCase } from '../vectors'

export interface ConnectionSizeCase {
  readonly files: string
  readonly bytes: string
  readonly terminal: boolean
  readonly failed: boolean
  readonly class: string
}

export interface CheckpointCut {
  readonly cut: string
  readonly newGenerationSelectable: boolean
}

export interface SemanticsVector extends VectorCase {
  readonly values?: Readonly<Record<string, string>>
  readonly cases?: readonly ConnectionSizeCase[] | readonly CheckpointCut[]
  readonly cuts?: readonly CheckpointCut[]
  readonly order?: readonly string[]
  readonly publishOnlyAfter?: readonly string[]
  readonly preCommitCrashVisible?: boolean
  readonly explicitStopUsesCrashGrace?: boolean
}

export type ReceiveLifecycleTerminalStateName =
  | 'published'
  | 'download-started'
  | 'partial-directory'
  | 'restart-required'
  | 'discarded'
  | 'expired'
  | 'needs-attention'

export type ReceiveLifecyclePlanName =
  | 'direct-tree'
  | 'direct-atomic'
  | 'workspace-then-publish'
  | 'portable-handoff'

export interface ReceiveLifecycleTerminalState {
  readonly state: ReceiveLifecycleTerminalStateName
  readonly byte: number
  readonly plans: readonly ReceiveLifecyclePlanName[]
}

export interface ReceiveLifecycleSemanticsVector extends SemanticsVector {
  readonly name: 'receive-lifecycle-terminal-states'
  readonly states: readonly ReceiveLifecycleTerminalState[]
  readonly deadlineWritingStates: readonly (
    | 'resumable-receive'
    | 'resumable-package'
    | 'waiting-to-save'
  )[]
  readonly publishedCleanupPendingRemains: 'published'
  readonly handoffNeverMeans: 'published'
  readonly completeArtifactsExclude: readonly 'partial-directory'[]
}

export const semantics = loadVectorFile(
  new URL('../../../core/testvectors/v2-semantics.json', import.meta.url),
).cases as SemanticsVector[]

export function classifyConnectionSize(value: ConnectionSizeCase): string {
  if (BigInt(value.files) >= 30n || BigInt(value.bytes) >= 8n << 20n) {
    return 'large'
  }
  if (!value.terminal || value.failed) {
    return 'unknown'
  }
  return 'small'
}

const RECEIVE_TERMINAL_STATES = new Set<string>([
  'published',
  'download-started',
  'partial-directory',
  'restart-required',
  'discarded',
  'expired',
  'needs-attention',
])

const RECEIVE_PLAN_NAMES = new Set<string>([
  'direct-tree',
  'direct-atomic',
  'workspace-then-publish',
  'portable-handoff',
])

const DEADLINE_WRITING_STATES = new Set<string>([
  'resumable-receive',
  'resumable-package',
  'waiting-to-save',
])

export function requireReceiveLifecycleSemanticsVector(
  value: SemanticsVector | undefined,
): ReceiveLifecycleSemanticsVector {
  if (value?.name !== 'receive-lifecycle-terminal-states') {
    throw new Error('receive lifecycle terminal-state vector is missing')
  }
  if (!isArrayOf(value.states, isReceiveLifecycleTerminalState)) {
    throw new Error('receive lifecycle terminal states are malformed')
  }
  if (!isArrayOf(value.deadlineWritingStates, isDeadlineWritingState)) {
    throw new Error('receive lifecycle deadline-writing states are malformed')
  }
  if (value.publishedCleanupPendingRemains !== 'published' ||
      value.handoffNeverMeans !== 'published' ||
      !isArrayOf(value.completeArtifactsExclude, isPartialDirectoryState)) {
    throw new Error('receive lifecycle terminal projections are malformed')
  }
  return value as ReceiveLifecycleSemanticsVector
}

function isReceiveLifecycleTerminalState(value: unknown): value is ReceiveLifecycleTerminalState {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false
  const state = value as Record<string, unknown>
  return typeof state.state === 'string' && RECEIVE_TERMINAL_STATES.has(state.state) &&
    typeof state.byte === 'number' && Number.isInteger(state.byte) &&
    isArrayOf(state.plans, isReceivePlanName)
}

function isReceivePlanName(value: unknown): value is ReceiveLifecyclePlanName {
  return typeof value === 'string' && RECEIVE_PLAN_NAMES.has(value)
}

function isDeadlineWritingState(
  value: unknown,
): value is ReceiveLifecycleSemanticsVector['deadlineWritingStates'][number] {
  return typeof value === 'string' && DEADLINE_WRITING_STATES.has(value)
}

function isPartialDirectoryState(value: unknown): value is 'partial-directory' {
  return value === 'partial-directory'
}

function isArrayOf<T>(
  value: unknown,
  predicate: (candidate: unknown) => candidate is T,
): value is readonly T[] {
  return Array.isArray(value) && value.every(predicate)
}
