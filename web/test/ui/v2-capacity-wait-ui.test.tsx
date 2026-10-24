import { experienceSnapshot } from './receiver-experience-fixture'
import { renderToString } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'

import { ReceiverApp } from '../../src/ui/ReceiverApp'
import type { ReceiverController } from '../../src/ui/controller'
import { EMPTY_PREVIEW, EMPTY_PROGRESS, type ReceiverSnapshot } from '../../src/ui/model'
import {
  EMPTY_OUTPUT_PRESENTATION,
  type OutputPresentationSnapshot,
} from '../../src/ui/output'

describe('receiver capacity wait UI', () => {
  it('hides short waits and clears the notice after capacity becomes available', () => {
    const hidden = render(progress({ capacityWaitingFiles: 1, capacityWaitVisible: false }))
    const visible = render(progress({ capacityWaitingFiles: 1, capacityWaitVisible: true }))
    const cleared = render(progress({ capacityWaitingFiles: 0, capacityWaitVisible: false }))

    expect(hidden).not.toContain('Waiting for sender capacity')
    expect(visible).toContain('Waiting for sender capacity')
    expect(cleared).not.toContain('Waiting for sender capacity')
  })

  it('shows the same delayed notice beside Direct ZIP progress', () => {
    const output: OutputPresentationSnapshot = Object.freeze({
      ...EMPTY_OUTPUT_PRESENTATION,
      directZipProgress: Object.freeze({
        kind: 'direct-zip',
        operationId: 'operation',
        generation: 1n,
        phase: 'receiving',
        receivedSelectedBytes: 32n,
        writtenSelectedBytes: 24n,
        safeResumeBytes: 16n,
      }),
    })
    const html = render(
      progress({ materializedBytes: 24n, capacityWaitingFiles: 1, capacityWaitVisible: true }),
      output,
    )

    expect(html).toContain('Download progress')
    expect(html).toContain('32 B received')
    expect(html).toContain('Waiting for sender capacity')
  })
})

function render(
  transferProgress: ReceiverSnapshot['progress'],
  output: OutputPresentationSnapshot = EMPTY_OUTPUT_PRESENTATION,
): string {
  const snapshot: ReceiverSnapshot = Object.freeze({
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
    progress: transferProgress,
    preview: EMPTY_PREVIEW,
    output: { ...output, lifecycle: { kind: 'receiving' as const, operationId: 'operation', receiveIntentDigest: 'intent', generation: 1n, activeLeaseId: 'lease' } },
    retained: Object.freeze({
      kind: 'ready',
      operations: Object.freeze([]),
      error: null,
      pending: null,
    }),
  })
  const controller = {
    subscribe: vi.fn(() => () => undefined),
    getSnapshot: vi.fn(() => snapshot),
  } as unknown as ReceiverController
  return renderToString(<ReceiverApp controller={controller} />)
}

function progress(
  patch: Partial<ReceiverSnapshot['progress']>,
): ReceiverSnapshot['progress'] {
  return Object.freeze({
    ...EMPTY_PROGRESS,
    transferJobId: 'job',
    ...patch,
  })
}
