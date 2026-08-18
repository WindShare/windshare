import { describe, expect, it, vi } from 'vitest'
import { renderToString } from 'react-dom/server'
import App from '../../src/App'
import { PortalApp } from '../../src/ui/portal/PortalApp'
import type { V2ReceiverController } from '../../src/ui/v2-controller'
import {
  EMPTY_V2_PROGRESS,
  EMPTY_V2_PREVIEW,
  EMPTY_V2_RETAINED_INVENTORY,
  type V2ReceiverSnapshot,
} from '../../src/ui/v2-model'
import { EMPTY_V2_OUTPUT_PRESENTATION } from '../../src/ui/v2-output'
import type { V2RetainedReceiveOperation } from '../../src/ui/v2-receive-runtime'

function createMockController(snapshotOverrides: Partial<V2ReceiverSnapshot> = {}): V2ReceiverController {
  const snapshot: V2ReceiverSnapshot = {
    phase: 'awaiting-key',
    status: 'Waiting for the capability key.',
    error: null,
    rows: Object.freeze([]),
    breadcrumbs: Object.freeze([]),
    pageIndex: 0,
    pageCount: 0,
    entryCount: 0,
    omittedCount: 0n,
    selectedVisibleFiles: 0,
    selectedVisibleBytes: 0n,
    directoryRetryable: false,
    progress: EMPTY_V2_PROGRESS,
    preview: EMPTY_V2_PREVIEW,
    output: EMPTY_V2_OUTPUT_PRESENTATION,
    retained: EMPTY_V2_RETAINED_INVENTORY,
    ...snapshotOverrides,
  }

  return {
    subscribe: vi.fn(() => () => undefined),
    getSnapshot: vi.fn(() => snapshot),
    submitKey: vi.fn(),
    toggleSelection: vi.fn(),
    openDirectory: vi.fn(),
    openBreadcrumb: vi.fn(),
    showPage: vi.fn(),
    retryDirectory: vi.fn(),
    previewFile: vi.fn(),
    cancelPreview: vi.fn(),
    seekPreview: vi.fn(),
    previewMediaFailed: vi.fn(),
    chooseArtifact: vi.fn(),
    retryOutputConfirmation: vi.fn(),
    performLifecycleAction: vi.fn(),
    performRetainedAction: vi.fn(),
    dispose: vi.fn(async () => undefined),
  } as unknown as V2ReceiverController
}

describe('Portal and App mode routing', () => {
  it('renders PortalApp when in root awaiting-key phase', () => {
    const controller = createMockController({ phase: 'awaiting-key' })
    const html = renderToString(<App controller={controller} />)

    expect(html).toContain('WindShare')
    expect(html).toContain('无需上传云端')
    expect(html).toContain('极速接收 (Receive)')
    expect(html).toContain('CLI 命令行分享')
  })

  it('renders V2ReceiverApp when browsing a share link', () => {
    const controller = createMockController({
      phase: 'browsing',
      status: 'Ready to receive.',
      breadcrumbs: Object.freeze([{ id: 'root', name: 'Root Directory' }]),
    })
    const html = renderToString(<App controller={controller} />)

    expect(html).toContain('receiver-shell')
    expect(html).toContain('Root Directory')
    expect(html).toContain('Browse and save shared files')
  })

  it('renders PortalApp with retained tasks banner when local tasks exist', () => {
    const mockOp = {
      operationId: 'op-123',
      receiveIntentDigest: 'digest-123',
      lifecycleGeneration: 1,
      lifecycle: { operationId: 'op-123', receiveIntentDigest: 'digest-123', phase: 'paused' },
      continuation: 'resume-receive',
      actions: Object.freeze(['continue']),
    } as unknown as V2RetainedReceiveOperation

    const controller = createMockController({
      phase: 'awaiting-key',
      retained: {
        kind: 'ready',
        error: null,
        operations: Object.freeze([mockOp]),
      },
    })
    const html = renderToString(<PortalApp controller={controller} />)

    expect(html).toContain('发现本地未完成的接收任务')
    expect(html).toContain('一键恢复任务 →')
  })
})
