import { renderToString } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'

import type { RecoverySummary } from '../../src/output/file-system-access/recovery-summary'
import type { ReceiveLifecycleState } from '../../src/output/workspace'
import { V2ReceiverApp } from '../../src/ui/V2ReceiverApp'
import type { V2ReceiverController } from '../../src/ui/v2-controller'
import {
  EMPTY_V2_PREVIEW,
  EMPTY_V2_PROGRESS,
  type V2ReceiverSnapshot,
} from '../../src/ui/v2-model'
import { EMPTY_V2_OUTPUT_PRESENTATION } from '../../src/ui/v2-output'

describe('DirectTree recovery UI', () => {
  it('renders reload-safe recovery costs and both retained actions without another confirmation', () => {
    const lifecycle = pausedLifecycle()
    const retained: V2ReceiverSnapshot['retained'] = Object.freeze({
      kind: 'ready',
      error: null,
      pending: null,
      operations: Object.freeze([Object.freeze({
        operationId: lifecycle.operationId,
        receiveIntentDigest: lifecycle.receiveIntentDigest,
        lifecycleGeneration: lifecycle.generation,
        lifecycle,
        continuation: 'resume-receive',
        actions: Object.freeze(['continue', 'redownload'] as const),
        recoverySummary: recoverySummary(),
      })]),
    })

    const html = renderToString(
      <V2ReceiverApp controller={controller(snapshot(retained))} />,
    )

    expect(html).toContain('Known so far (discovery incomplete)')
    expect(html).toContain('Completed: 1 file, 1.0 KiB')
    expect(html).toContain('Verified partial data: 2 files, 512 B')
    expect(html).toContain('Preserve partial files: 2.0 KiB remaining')
    expect(html).toContain('up to 384 B of temporary destination space')
    expect(html).toContain('Restart incomplete files: 2.5 KiB remaining')
    expect(html).toContain('512 B of verified data to redownload')
    expect(html).toContain('Continue and preserve partial files')
    expect(html).toContain('class="abort-action"')
    expect(html).not.toMatch(/confirm|per-file/iu)
  })
})

function snapshot(retained: V2ReceiverSnapshot['retained']): V2ReceiverSnapshot {
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
    retained,
  })
}

function controller(value: V2ReceiverSnapshot): V2ReceiverController {
  return {
    subscribe: vi.fn(() => () => undefined),
    getSnapshot: vi.fn(() => value),
  } as unknown as V2ReceiverController
}

function pausedLifecycle(): Extract<ReceiveLifecycleState, {
  kind: 'resumable-receive'
  payloadKind: 'file-set'
}> {
  return Object.freeze({
    kind: 'resumable-receive',
    payloadKind: 'file-set',
    operationId: 'operation',
    receiveIntentDigest: 'intent',
    generation: 2n,
    checkpointSetDigest: 'checkpoint-set',
    completedFileCount: 1n,
    completedBytes: 1_024n,
    expiresAt: Date.now() + 60_000,
    selectionFacts: Object.freeze({
      discoveredFileCount: 5n,
      discoveredBytes: 3_584n,
      discovery: 'failed',
    }),
  })
}

function recoverySummary(): RecoverySummary {
  return Object.freeze({
    lifecycleGeneration: 2n,
    checkpointSetDigest: 'checkpoint-set',
    discoveredFileCount: 5n,
    discoveredBytes: 3_584n,
    discovery: 'known-so-far',
    completedFileCount: 1n,
    completedBytes: 1_024n,
    incompleteFileCount: 2n,
    verifiedPartialFileCount: 2n,
    verifiedPartialBytes: 512n,
    unstartedFileCount: 2n,
    unstartedBytes: 1_024n,
    preservingRemainingBytes: 2_048n,
    restartRemainingBytes: 2_560n,
    restartRedownloadBytes: 512n,
    maximumPreservingTemporaryBytes: 384n,
  })
}
