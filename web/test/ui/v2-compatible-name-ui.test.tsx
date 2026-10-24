import { TaskDetails } from '../../src/ui/tasks/TaskView'
import { presentTask, retainedTaskFacts } from '../../src/ui/tasks'
import { experienceSnapshot } from './receiver-experience-fixture'
import { renderToString } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'

import type {
  CompatibleNameRepairSummary,
} from '../../src/transfer/lifecycle/name-repair'
import type { ReceiveLifecycleState } from '../../src/output/workspace'
import type { ArtifactSpec } from '../../src/transfer/intent'
import { ReceiverApp } from '../../src/ui/ReceiverApp'
import type { ReceiverController } from '../../src/ui/controller'
import { presentReceiveLifecycle } from '../../src/ui/lifecycle-presentation'
import { EMPTY_PREVIEW, EMPTY_PROGRESS, type ReceiverSnapshot } from '../../src/ui/model'
import {
  EMPTY_OUTPUT_PRESENTATION,
  type OutputPresentationSnapshot,
} from '../../src/ui/output'

const TREE = {
  kind: 'directory-tree',
  layout: { kind: 'result-root', root: { name: 'shared-folder' } },
} as ArtifactSpec

describe('compatible-name receiver UI', () => {
  it('renders only an adjusted-name notice during a live receive', () => {
    const lifecycle = receiveLifecycle('receiving')
    const repairSummary = summary('active', false)
    const lifecyclePresentation = presentReceiveLifecycle({
      state: lifecycle,
      artifact: TREE,
      plan: Object.freeze({ kind: 'direct-tree' }) as NonNullable<OutputPresentationSnapshot['plan']>,
      repairSummary,
    })
    const output = Object.freeze({
      ...EMPTY_OUTPUT_PRESENTATION,
      resolvedArtifact: TREE,
      plan: Object.freeze({
        kind: 'direct-tree',
      }) as OutputPresentationSnapshot['plan'],
      lifecycle,
      repairSummary,
      lifecyclePresentation,
    })
    const html = renderToString(
      <ReceiverApp controller={controller(snapshot({
        output,
        retained: {
          kind: 'ready', error: null, pending: null,
          operations: [{
            operationId: lifecycle.operationId,
            receiveIntentDigest: lifecycle.receiveIntentDigest,
            lifecycleGeneration: lifecycle.generation,
            lifecycle,
            continuation: 'resume-receive',
            actions: ['continue', 'catch-up'],
            repairSummary,
          }],
        },
      }))} />,
    )

    expect(html).toContain('role="status"')
    expect(html).toContain('adjusted for this device.')
    expect(html).not.toContain('restore.windshare-abc234.ps1')
    expect(html).not.toContain('powershell.exe')
    expect(html).not.toContain('Copy restoration command')
    expect(html).not.toMatch(/names (?:are|were) restored/iu)
  })

  it('reconstructs the routine restoration action from a retained terminal summary', () => {
    const lifecycle = receiveLifecycle('published')
    const repairSummary = summary('completed', false)
    const retained: ReceiverSnapshot['retained'] = Object.freeze({
      kind: 'ready',
      error: null,
      pending: null,
      operations: Object.freeze([Object.freeze({
        operationId: lifecycle.operationId,
        receiveIntentDigest: lifecycle.receiveIntentDigest,
        lifecycleGeneration: lifecycle.generation,
        lifecycle,
        continuation: 'retry-cleanup',
        actions: Object.freeze([]),
        repairSummary,
      })]),
    })
    const html = renderToString(
      <TaskDetails task={presentTask(retainedTaskFacts(retained.operations[0]!, 'local'))} actions={{ perform: vi.fn(), catchUp: vi.fn() }} />,
    )

    expect(html).toContain('Restore the original names')
    expect(html).toContain('terminal sidecar checkpoint is complete')
    expect(html).toContain('restore.windshare-abc234.ps1')
    expect(html).toContain('restore.windshare-abc234.data')
  })

  it('requires retained local catch-up without exposing restoration as a runnable command', () => {
    const lifecycle = receiveLifecycle('receiving')
    const repairSummary = summary('active', true)
    const retained: ReceiverSnapshot['retained'] = Object.freeze({
      kind: 'ready',
      error: null,
      pending: null,
      operations: Object.freeze([Object.freeze({
        operationId: lifecycle.operationId,
        receiveIntentDigest: lifecycle.receiveIntentDigest,
        lifecycleGeneration: lifecycle.generation,
        lifecycle,
        continuation: 'pending-catch-up',
        actions: Object.freeze(['catch-up'] as const),
        repairSummary,
      })]),
    })
    const html = renderToString(
      <TaskDetails task={presentTask(retainedTaskFacts(retained.operations[0]!, 'local'))} actions={{ perform: vi.fn(), catchUp: vi.fn() }} />,
    )

    expect(html).toContain('Finish local restoration catch-up')
    expect(html).toContain('Restoration tool catch-up required')
    expect(html).toContain('Do not run the restoration tool yet')
    expect(html).not.toContain('Run command')
    expect(html).not.toContain('powershell.exe -NoProfile -ExecutionPolicy Bypass -File')
  })
})

function snapshot(
  patch: Partial<ReceiverSnapshot>,
): ReceiverSnapshot {
  return Object.freeze({
    ...experienceSnapshot(),
    pathActivity: { lanes: [] },
    phase: 'browsing',
    status: 'Ready.',
    error: null,
    rows: Object.freeze([]),
    breadcrumbs: Object.freeze([{ id: 'root', name: 'Shared files' }]),
    pageIndex: 0,
    pageCount: 1,
    entryCount: 0,
    omittedCount: 0n,
    selectedVisibleFiles: 0,
    selectedVisibleBytes: 0n,
    directoryRetryable: false,
    progress: EMPTY_PROGRESS,
    preview: EMPTY_PREVIEW,
    output: EMPTY_OUTPUT_PRESENTATION,
    retained: Object.freeze({
      kind: 'ready',
      operations: Object.freeze([]),
      error: null,
      pending: null,
    }),
    ...patch,
  })
}

function controller(value: ReceiverSnapshot): ReceiverController {
  return {
    subscribe: vi.fn(() => () => undefined),
    getSnapshot: vi.fn(() => value),
    retainedActionAdmission: vi.fn(() => ({ allowed: true, reason: null })),
    activeLifecycleActionAdmission: vi.fn(() => ({ allowed: true, reason: null })),
  } as unknown as ReceiverController
}

function receiveLifecycle(
  kind: 'receiving' | 'published',
): ReceiveLifecycleState {
  const common = {
    operationId: 'operation',
    receiveIntentDigest: 'intent',
    generation: 2n,
  }
  return kind === 'receiving'
    ? Object.freeze({ ...common, kind, activeLeaseId: 'lease' })
    : Object.freeze({
        ...common,
        kind,
        receiptDigest: 'receipt',
        cleanupState: 'clean',
      })
}

function summary(
  footerState: NonNullable<CompatibleNameRepairSummary['latestObservedFooter']>['state'],
  sidecarPending: boolean,
): CompatibleNameRepairSummary {
  const terminalSettlement = sidecarPending ? 'pending' : 'complete'
  return Object.freeze({
    committedCount: 2,
    logicalPathSample: Object.freeze([
      Object.freeze(['folder', 'pyvenv.cfg']),
      Object.freeze(['folder', 'nested']),
    ]),
    pairDisplayNames: Object.freeze({
      script: 'restore.windshare-abc234.ps1',
      sidecar: 'restore.windshare-abc234.data',
    }),
    placement: 'inside-logical-root',
    latestObservedFooter: Object.freeze({ committedCount: 2, state: footerState }),
    sidecarSync: sidecarPending ? 'pending' : 'current',
    terminalSettlement: footerState === 'active' ? 'none' : terminalSettlement,
  })
}
