import { EMPTY_V2_PROGRESS } from '../v2-model'
import type { ReceiveLifecycleState, ReceiveLifecycleStatePayload } from '../../output/workspace/state'
import type { TaskFacts } from './model'

const FIXTURE_OPERATION_ID = '1'.repeat(32)
const FIXTURE_INTENT_DIGEST = '2'.repeat(64)
const FIXTURE_TIME = 1_700_000_000_000

function lifecycle(payload: ReceiveLifecycleStatePayload): ReceiveLifecycleState {
  return Object.freeze({
    operationId: FIXTURE_OPERATION_ID, receiveIntentDigest: FIXTURE_INTENT_DIGEST,
    generation: 1n, ...payload,
  })
}

/** Synthetic identities and domain-shaped facts keep the gallery free of capabilities. */
export function taskFixture(overrides: Partial<TaskFacts> = {}): TaskFacts {
  return Object.freeze({
    lifecycle: lifecycle({ kind: 'receiving', activeLeaseId: '3'.repeat(32) }),
    display: Object.freeze({ objectLabel: 'Summer photos', destinationLabel: 'Downloads', createdAtMilliseconds: FIXTURE_TIME }),
    actions: Object.freeze([]),
    blocking: null,
    readiness: 'matching-share',
    completeness: 'unknown',
    publication: 'unpublished',
    progress: Object.freeze({
      ...EMPTY_V2_PROGRESS, discoveredFiles: 12, discoveredBytes: 6_710_886n,
      writtenBytes: 2_000_000n, materializedBytes: 2_000_000n, completedFiles: 3, completedBytes: 1_500_000n,
    }),
    directZipProgress: null,
    fidelity: null,
    details: Object.freeze([]),
    interruption: null,
    execution: Object.freeze({ kind: 'active' }),
    ...overrides,
  })
}

export const TASK_FIXTURES: Readonly<Record<string, TaskFacts>> = Object.freeze({
  'open-discovery': taskFixture(),
  reconnecting: taskFixture({ blocking: Object.freeze({ kind: 'reconnecting' }) }),
  'sender-capacity': taskFixture({ blocking: Object.freeze({ kind: 'sender-capacity' }) }),
  verifying: taskFixture({
    directZipProgress: Object.freeze({
      kind: 'direct-zip', operationId: FIXTURE_OPERATION_ID, generation: 1n,
      phase: 'verifying', safeResumeBytes: 1_500_000n,
    }),
  }),
  'partial-ready': taskFixture({
    lifecycle: lifecycle({ kind: 'waiting-to-save', packageDigest: '4'.repeat(64) }),
    completeness: 'partial',
  }),
  'browser-handoff': taskFixture({
    lifecycle: lifecycle({ kind: 'download-started', attemptKind: 'portable', attemptId: '5'.repeat(32) }),
    publication: 'browser-handoff',
  }),
  'saved-cleanup': taskFixture({
    lifecycle: lifecycle({ kind: 'published', receiptDigest: '6'.repeat(64), cleanupState: 'cleanup-pending' }),
    completeness: 'complete', publication: 'saved',
  }),
})
