import { EMPTY_PROGRESS } from '../model'
import type { ReceiveLifecycleState, ReceiveStatePayload } from '../../transfer/state'
import type { TaskFacts } from './model'

const FIXTURE_TASK_ID = '1'.repeat(32)
const FIXTURE_TIME = 1_700_000_000_000

function lifecycle(payload: ReceiveStatePayload): ReceiveLifecycleState {
  const finished = ['saved', 'download-started', 'ready-to-save', 'partial'].includes(payload.kind)
  return Object.freeze({
    taskId: FIXTURE_TASK_ID,
    generation: 1n,
    ...payload,
    timing: Object.freeze({
      startedAtMilliseconds: FIXTURE_TIME,
      activeMilliseconds: finished ? 125_000 : 0,
      ...(finished ? { finishedAtMilliseconds: FIXTURE_TIME + 125_000 } : {}),
    }),
  }) as ReceiveLifecycleState
}

/** Synthetic identities and domain-shaped facts keep the gallery free of capabilities. */
export function taskFixture(overrides: Partial<TaskFacts> = {}): TaskFacts {
  return Object.freeze({
    lifecycle: lifecycle({ kind: 'downloading', phase: 'receiving' }),
    backend: 'fsa',
    display: Object.freeze({ objectLabel: 'Summer photos', destinationLabel: 'Downloads', createdAtMilliseconds: FIXTURE_TIME }),
    actions: Object.freeze([]),
    blocking: null,
    readiness: 'matching-share',
    completeness: 'unknown',
    publication: 'unpublished',
    progress: Object.freeze({
      ...EMPTY_PROGRESS,
      discoveredFiles: 12,
      discoveredBytes: 6_710_886n,
      writtenBytes: 2_000_000n,
      materializedBytes: 2_000_000n,
      completedFiles: 3,
      completedBytes: 1_500_000n,
    }),
    safeRenamed: null,
    sourceOmittedEntries: 0n,
    details: Object.freeze([]),
    interruption: null,
    outageCheckpointed: false,
    ...overrides,
  })
}

export const TASK_FIXTURES: Readonly<Record<string, TaskFacts>> = Object.freeze({
  'open-discovery': taskFixture(),
  reconnecting: taskFixture({ blocking: Object.freeze({ kind: 'reconnecting' }) }),
  'sender-capacity': taskFixture({ blocking: Object.freeze({ kind: 'sender-capacity' }) }),
  'relay-bandwidth': taskFixture({ blocking: Object.freeze({ kind: 'relay-bandwidth' }) }),
  'partial-ready': taskFixture({
    backend: 'opfs',
    lifecycle: lifecycle({ kind: 'ready-to-save', size: 1_500_000n, fileName: 'Summer photos.zip' }),
    completeness: 'partial',
  }),
  'browser-handoff': taskFixture({
    backend: 'portable',
    lifecycle: lifecycle({ kind: 'download-started', retained: false, fileName: 'Summer photos.zip' }),
    publication: 'browser-handoff',
  }),
  'saved-cleanup': taskFixture({
    backend: 'fsa',
    lifecycle: lifecycle({ kind: 'saved', savedFiles: 12n, savedBytes: 6_710_886n }),
    completeness: 'complete',
    publication: 'saved',
  }),
})
