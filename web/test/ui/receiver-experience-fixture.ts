import { vi } from 'vitest'
import type { V2ReceiverController } from '../../src/ui/v2-controller'
import { EMPTY_V2_PREVIEW, EMPTY_V2_PROGRESS, EMPTY_V2_RETAINED_INVENTORY, type V2ReceiverSnapshot } from '../../src/ui/v2-model'
import { EMPTY_V2_OUTPUT_PRESENTATION } from '../../src/ui/v2-output'

export function experienceSnapshot(patch: Partial<V2ReceiverSnapshot> = {}): V2ReceiverSnapshot {
  return {
    share: null,
    connection: { kind: 'connected' },
    browse: { kind: 'ready', status: 'Ready.', error: null },
    draft: { mode: 'scope', scope: 'whole-share', label: 'Shared files', summary: 'All items', empty: false },
    startAdmission: { allowed: true, reason: null, canReleaseCurrent: false },
    activeReceiveOperationId: patch.output?.lifecycle?.operationId ?? null,
    taskDisplay: null,
    pathActivity: { directConnected: false, content: 'idle' },
    phase: 'awaiting-key', status: 'Waiting for a link.', error: null,
    rows: [], breadcrumbs: [], pageIndex: 0, pageCount: 0, entryCount: 0, omittedCount: 0n,
    selectedVisibleFiles: 0, selectedVisibleBytes: 0n, directoryRetryable: false,
    progress: EMPTY_V2_PROGRESS, preview: EMPTY_V2_PREVIEW,
    output: EMPTY_V2_OUTPUT_PRESENTATION, retained: EMPTY_V2_RETAINED_INVENTORY,
    ...patch,
  }
}

export function experienceController(snapshot: V2ReceiverSnapshot): V2ReceiverController {
  return {
    subscribe: vi.fn(() => () => undefined), getSnapshot: vi.fn(() => snapshot),
    submitKey: vi.fn(), toggleSelection: vi.fn(), openDirectory: vi.fn(), openBreadcrumb: vi.fn(),
    showPage: vi.fn(), retryDirectory: vi.fn(), previewFile: vi.fn(), cancelPreview: vi.fn(),
    seekPreview: vi.fn(), previewMediaPresented: vi.fn(), previewMediaFailed: vi.fn(),
    chooseArtifact: vi.fn(), retryOutputConfirmation: vi.fn(), performLifecycleAction: vi.fn(),
    performRetainedAction: vi.fn(), enterSelectionMode: vi.fn(), exitSelectionMode: vi.fn(),
    selectPage: vi.fn(), clearSelection: vi.fn(), cancelPreparing: vi.fn(),
    startNewReceiveOperation: vi.fn(), catchUpStoppedCompatibleNames: vi.fn(),
    prepareReplacementDownload: vi.fn(), recordExperienceIntent: vi.fn(),
    retainedActionAdmission: vi.fn(() => ({ allowed: true, reason: null })),
    activeLifecycleActionAdmission: vi.fn(() => ({ allowed: true, reason: null })),
    canRetainCurrentOperation: false, retainCurrentOperation: vi.fn(),
  } as unknown as V2ReceiverController
}
