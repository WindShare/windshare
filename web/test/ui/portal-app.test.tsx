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

function buttonMarkup(html: string, label: string): string {
  const markup = html.match(new RegExp(`<button[^>]*>${label}</button>`))?.[0]
  if (markup === undefined) throw new Error(`button was not rendered: ${label}`)
  return markup
}

function inputMarkup(html: string, ariaLabel: string): string {
  const markup = html.match(new RegExp(`<input[^>]*aria-label="${ariaLabel}"[^>]*>`))?.[0]
  if (markup === undefined) throw new Error(`input was not rendered: ${ariaLabel}`)
  return markup
}

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
    expect(html).toContain('秒出任意文件分享链接')
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

  it('server-renders retained authority as busy and locks browsing actions until settlement', () => {
    const operation = Object.freeze({
      operationId: 'pending-operation',
      receiveIntentDigest: 'pending-intent',
      lifecycleGeneration: 1n,
      lifecycle: Object.freeze({}),
      continuation: 'save-artifact',
      actions: Object.freeze(['save', 'discard'] as const),
    }) as unknown as V2RetainedReceiveOperation
    const artifactChoice = Object.freeze({
      offeredChoice: null,
      choice: Object.freeze({ artifactKind: 'single-file' }) as never,
      operation: 'download-original' as const,
      label: 'Receive original',
      description: 'Receive the selected file.',
      importance: 'primary' as const,
      packageExplanation: null,
    })
    const output: V2ReceiverSnapshot['output'] = Object.freeze({
      ...EMPTY_V2_OUTPUT_PRESENTATION,
      offerPresentation: Object.freeze({
        kind: 'choices',
        interactive: true,
        primary: artifactChoice,
        alternatives: Object.freeze([]),
      }),
    })
    const base: Partial<V2ReceiverSnapshot> = {
      phase: 'browsing',
      breadcrumbs: Object.freeze([{ id: 'root', name: 'Root Directory' }]),
      rows: Object.freeze([{
        id: 'file',
        kind: 'file',
        name: 'report.txt',
        expectedSize: 1n,
        selection: 'selected',
      }]),
      output,
    }
    const pendingRetained: V2ReceiverSnapshot['retained'] = Object.freeze({
      kind: 'ready',
      error: null,
      operations: Object.freeze([operation]),
      pending: Object.freeze({ operationId: operation.operationId, action: 'save' }),
    })
    const settledRetained: V2ReceiverSnapshot['retained'] = Object.freeze({
      ...pendingRetained,
      pending: null,
    })

    const pendingHtml = renderToString(<App controller={createMockController({
      ...base,
      retained: pendingRetained,
    })} />)
    const settledHtml = renderToString(<App controller={createMockController({
      ...base,
      retained: settledRetained,
    })} />)

    expect(pendingHtml).toMatch(/class="retained-receive-panel"[^>]*aria-busy="true"/)
    expect(inputMarkup(pendingHtml, 'Select report.txt')).toContain('disabled')
    expect(buttonMarkup(pendingHtml, 'Receive original')).toContain('disabled')
    expect(buttonMarkup(pendingHtml, 'Save')).toContain('disabled')
    expect(inputMarkup(settledHtml, 'Select report.txt')).not.toContain('disabled')
    expect(buttonMarkup(settledHtml, 'Receive original')).not.toContain('disabled')
    expect(buttonMarkup(settledHtml, 'Save')).not.toContain('disabled')

    const retryOutput: V2ReceiverSnapshot['output'] = Object.freeze({
      ...EMPTY_V2_OUTPUT_PRESENTATION,
      offerPresentation: Object.freeze({
        kind: 'retry',
        interactive: true,
        title: 'Confirmation paused',
        description: 'Retry the same selection.',
        label: 'Retry confirmation',
      }),
    })
    const pendingRetryHtml = renderToString(<App controller={createMockController({
      ...base,
      output: retryOutput,
      retained: pendingRetained,
    })} />)
    const settledRetryHtml = renderToString(<App controller={createMockController({
      ...base,
      output: retryOutput,
      retained: settledRetained,
    })} />)
    expect(buttonMarkup(pendingRetryHtml, 'Retry confirmation')).toContain('disabled')
    expect(buttonMarkup(settledRetryHtml, 'Retry confirmation')).not.toContain('disabled')
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
        pending: null,
      },
    })
    const html = renderToString(<PortalApp controller={controller} />)

    expect(html).toContain('发现本地未完成的接收任务')
    expect(html).toContain('一键恢复任务 →')
  })
})
