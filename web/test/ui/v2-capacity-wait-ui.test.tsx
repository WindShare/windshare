import { renderToString } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'

import { V2ReceiverApp } from '../../src/ui/V2ReceiverApp'
import type { V2ReceiverController } from '../../src/ui/v2-controller'
import {
  EMPTY_V2_PREVIEW,
  EMPTY_V2_PROGRESS,
  type V2ReceiverSnapshot,
} from '../../src/ui/v2-model'
import {
  EMPTY_V2_OUTPUT_PRESENTATION,
  type V2OutputPresentationSnapshot,
} from '../../src/ui/v2-output'

describe('receiver capacity wait UI', () => {
  it('hides short waits and clears the notice after capacity becomes available', () => {
    const hidden = render(progress({ capacityWaitingFiles: 1, capacityWaitVisible: false }))
    const visible = render(progress({ capacityWaitingFiles: 1, capacityWaitVisible: true }))
    const cleared = render(progress({ capacityWaitingFiles: 0, capacityWaitVisible: false }))

    expect(hidden).not.toContain('Waiting for sender capacity')
    expect(visible).toContain('Waiting for sender capacity')
    expect(visible).toContain('capacity-wait-notice')
    expect(cleared).not.toContain('Waiting for sender capacity')
  })

  it('shows the same delayed notice beside Direct ZIP progress', () => {
    const output: V2OutputPresentationSnapshot = Object.freeze({
      ...EMPTY_V2_OUTPUT_PRESENTATION,
      directZipProgress: Object.freeze({
        kind: 'direct-zip',
        operationId: 'operation',
        generation: 1n,
        phase: 'receiving',
        receivedSelectedBytes: 32n,
        safeResumeBytes: 16n,
      }),
    })
    const html = render(
      progress({ capacityWaitingFiles: 1, capacityWaitVisible: true }),
      output,
    )

    expect(html).toContain('If interrupted, resume from')
    expect(html).toContain('32')
    expect(html).toContain('Waiting for sender capacity')
  })
})

function render(
  transferProgress: V2ReceiverSnapshot['progress'],
  output: V2OutputPresentationSnapshot = EMPTY_V2_OUTPUT_PRESENTATION,
): string {
  const snapshot: V2ReceiverSnapshot = Object.freeze({
    pathActivity: { directConnected: false, content: 'idle' as const },
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
    preview: EMPTY_V2_PREVIEW,
    output,
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
  } as unknown as V2ReceiverController
  return renderToString(<V2ReceiverApp controller={controller} />)
}

function progress(
  patch: Partial<V2ReceiverSnapshot['progress']>,
): V2ReceiverSnapshot['progress'] {
  return Object.freeze({
    ...EMPTY_V2_PROGRESS,
    transferJobId: 'job',
    ...patch,
  })
}
