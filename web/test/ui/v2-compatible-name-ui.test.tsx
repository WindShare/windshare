import { renderToString } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'

import type { CompatibleNameRepairSummary } from '../../src/output/file-system-access/compatible-name/model'
import type { ReceiveLifecycleState } from '../../src/output/workspace'
import type { ArtifactSpec } from '../../src/transfer/intent'
import { V2ReceiverApp } from '../../src/ui/V2ReceiverApp'
import type { V2ReceiverController } from '../../src/ui/v2-controller'
import { presentReceiveLifecycle } from '../../src/ui/v2-lifecycle-presentation'
import {
  EMPTY_V2_PREVIEW,
  EMPTY_V2_PROGRESS,
  type V2ReceiverSnapshot,
} from '../../src/ui/v2-model'
import {
  EMPTY_V2_OUTPUT_PRESENTATION,
  type V2OutputPresentationSnapshot,
} from '../../src/ui/v2-output'

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
      plan: Object.freeze({ kind: 'direct-tree' }) as NonNullable<V2OutputPresentationSnapshot['plan']>,
      nowMilliseconds: 1_000,
      repairSummary,
    })
    const output = Object.freeze({
      ...EMPTY_V2_OUTPUT_PRESENTATION,
      resolvedArtifact: TREE,
      plan: Object.freeze({
        kind: 'direct-tree',
      }) as V2OutputPresentationSnapshot['plan'],
      lifecycle,
      repairSummary,
      lifecyclePresentation,
    })
    const html = renderToString(
      <V2ReceiverApp controller={controller(snapshot({
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
    expect(html).toContain('Compatible names are in use')
    expect(html).toContain('2 verified/committed name replacements')
    expect(html).not.toContain('restore.windshare-abc234.ps1')
    expect(html).not.toContain('powershell.exe')
    expect(html).not.toContain('Copy restoration command')
    expect(html).not.toContain('<details')
    expect(html).not.toMatch(/names (?:are|were) restored/iu)
  })

  it('reconstructs the routine restoration action from a retained terminal summary', () => {
    const lifecycle = receiveLifecycle('published')
    const repairSummary = summary('completed', false)
    const retained: V2ReceiverSnapshot['retained'] = Object.freeze({
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
      <V2ReceiverApp controller={controller(snapshot({ retained }))} />,
    )

    expect(html).toContain('Restore the original names')
    expect(html).toContain('terminal sidecar checkpoint is complete')
    expect(html).toContain('restore.windshare-abc234.ps1')
    expect(html).toContain('restore.windshare-abc234.data')
  })

  it('requires retained local catch-up without exposing restoration as a runnable command', () => {
    const lifecycle = receiveLifecycle('receiving')
    const repairSummary = summary('active', true)
    const retained: V2ReceiverSnapshot['retained'] = Object.freeze({
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
      <V2ReceiverApp controller={controller(snapshot({ retained }))} />,
    )

    expect(html).toContain('Compatible-name finalization needs catch-up')
    expect(html).toContain('Finish local restoration catch-up')
    expect(html).toContain('Restoration tool catch-up required')
    expect(html).toContain('Do not run the restoration tool yet')
    expect(html).not.toContain('Abnormal-stop recovery only')
    expect(html).not.toContain('Run command')
    expect(html).not.toContain('powershell.exe -NoProfile -ExecutionPolicy Bypass -File')
  })
})

function snapshot(
  patch: Partial<V2ReceiverSnapshot>,
): V2ReceiverSnapshot {
  return Object.freeze({
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
    progress: EMPTY_V2_PROGRESS,
    preview: EMPTY_V2_PREVIEW,
    output: EMPTY_V2_OUTPUT_PRESENTATION,
    retained: Object.freeze({
      kind: 'ready',
      operations: Object.freeze([]),
      error: null,
      pending: null,
    }),
    ...patch,
  })
}

function controller(value: V2ReceiverSnapshot): V2ReceiverController {
  return {
    subscribe: vi.fn(() => () => undefined),
    getSnapshot: vi.fn(() => value),
  } as unknown as V2ReceiverController
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
