import type { ReceiveRuntimeSnapshot, RetainedTaskSnapshot } from '../../../src/application/receive/contracts'
import { encodeBase64Url } from '../../../src/crypto/bytes'
import { planArtifactOffers } from '../../../src/output/backends/contract'
import { safeRenamedSummary } from '../../../src/transfer/state'
import { EMPTY_PREVIEW, EMPTY_PROGRESS, type BrowseRow, type PreviewSnapshot, type ReceiverSnapshot } from '../../../src/ui/model'
import { EMPTY_RECEIVE_RUNTIME } from '../../../src/ui/output'
import { TASK_FIXTURES } from '../../../src/ui/tasks/fixtures'

export const SCENARIOS = [
  'portal', 'portal-empty', 'portal-loading', 'portal-failed', 'portal-saved',
  'folder', 'exact-progress', 'long-names', 'full-directory', 'portrait', 'landscape',
  'video', 'unsupported', 'reconnecting', 'capacity', 'verifying',
  'partial-ready', 'browser-handoff', 'saved-cleanup',
] as const

export type Scenario = typeof SCENARIOS[number]
export type GallerySnapshot = Omit<ReceiverSnapshot, 'output'> & { readonly output: ReceiveRuntimeSnapshot }
export type CompletedDownloadScenario = 'published' | 'download-started' | 'workspace'

export const ROOT_ROWS: readonly BrowseRow[] = [
  { id: 'photos', kind: 'directory', name: 'Summer photos', selection: 'mixed' },
  {
    id: 'notes',
    kind: 'file',
    name: 'A very long project notes filename describing the entire summer holiday and its carefully preserved original details.txt',
    expectedSize: 16_384n,
    selection: 'selected',
  },
  ...Array.from({ length: 7 }, (_, index): BrowseRow => ({
    id: 'item-' + index,
    kind: 'file',
    name: 'Landscape ' + (index + 1) + '.png',
    expectedSize: 524_288n,
    selection: 'unselected',
  })),
]

export const LONG_NAME = '????????? ? ' + 'UnbrokenProjectName'.repeat(7) + '.png'

const CATALOG_PAGE_ENTRY_LIMIT = 256
export const FULL_DIRECTORY_ROWS: readonly BrowseRow[] = Array.from({ length: CATALOG_PAGE_ENTRY_LIMIT }, (_, index) => ({
  id: 'full-item-' + index,
  kind: 'file',
  name: index === CATALOG_PAGE_ENTRY_LIMIT - 1 ? 'Photo ' + (index + 1) + '.png' : 'Document ' + (index + 1) + '.txt',
  expectedSize: 16_384n,
  selection: 'unselected',
}))

export function syntheticPhoto(width: number, height: number): string {
  const canvas = document.createElement('canvas')
  canvas.width = width
  canvas.height = height
  const context = canvas.getContext('2d')!
  context.fillStyle = '#dce9df'
  context.fillRect(0, 0, width, height)
  context.fillStyle = '#93bba5'
  context.beginPath()
  context.moveTo(0, height * 0.75)
  context.lineTo(width * 0.58, height * 0.3)
  context.lineTo(width, height * 0.8)
  context.lineTo(width, height)
  context.lineTo(0, height)
  context.fill()
  context.fillStyle = '#ffffff'
  context.beginPath()
  context.arc(width * 0.24, height * 0.24, width * 0.07, 0, Math.PI * 2)
  context.fill()
  return canvas.toDataURL('image/png')
}

export function photoPreview(portrait: boolean, fileId = 'photo'): PreviewSnapshot {
  const width = portrait ? 400 : 800
  const height = portrait ? 700 : 450
  return {
    state: 'image',
    fileId,
    name: portrait ? 'Portrait.png' : 'Landscape.png',
    url: syntheticPhoto(width, height),
    presentationId: 1,
    mimeType: 'image/png',
    width,
    height,
  }
}

function galleryId(seed: number): string {
  return encodeBase64Url(new Uint8Array(16).fill(seed))
}

async function portalInventorySnapshot(scenario: Scenario): Promise<GallerySnapshot> {
  const snapshot = await gallerySnapshot(scenario === 'portal-saved' ? 'saved-cleanup' : 'folder')
  let retained: ReceiverSnapshot['retained'] = { kind: 'empty', tasks: [], pending: null }
  if (scenario === 'portal-saved') return { ...snapshot, retained }
  if (scenario === 'portal-loading') retained = { ...retained, kind: 'loading', pending: null }
  if (scenario === 'portal-failed') retained = { ...retained, kind: 'failed', pending: null }
  return { ...snapshot, retained, output: EMPTY_RECEIVE_RUNTIME, taskDisplay: null, progress: EMPTY_PROGRESS }
}

function galleryTaskOutput(
  scenario: Scenario,
  offers: ReceiveRuntimeSnapshot['offers'],
  facts: (typeof TASK_FIXTURES)[string],
): ReceiveRuntimeSnapshot {
  return Object.freeze({
    ...EMPTY_RECEIVE_RUNTIME,
    capabilitiesState: 'ready',
    offers,
    activeTaskId: facts.lifecycle.kind === 'downloading' ? facts.lifecycle.taskId : null,
    activeSelectionKey: 'gallery-selection',
    backend: facts.backend,
    lifecycle: facts.lifecycle,
    actions: facts.lifecycle.kind === 'downloading'
      ? Object.freeze(['pause' as const, 'stop' as const])
      : Object.freeze([]),
    safeRenamed: scenario === 'saved-cleanup'
      ? safeRenamedSummary({
          count: 1,
          sample: [{ logicalPath: ['project', 'settings.cfg'], physicalName: 'settings-cfg.windshare-a1b2c3d4' }],
        })
      : null,
  })
}

export async function gallerySnapshot(scenario: Scenario): Promise<GallerySnapshot> {
  if (scenario.startsWith('portal-')) return portalInventorySnapshot(scenario)
  const single = ['portrait', 'landscape', 'video', 'unsupported'].includes(scenario)
  const shareInstance = galleryId(1)
  const names = {
    video: 'Summer afternoon.mp4',
    unsupported: 'Project archive.7z',
    portrait: 'Portrait.png',
    landscape: 'Landscape.png',
  }
  const name = names[scenario as keyof typeof names] ?? 'Summer photos'
  const kind = scenario === 'video' ? 'video' : 'photo'
  const file: BrowseRow = {
    id: 'photo',
    kind: 'file',
    name,
    expectedSize: 524_288n,
    selection: 'selected',
  }
  const offers = planArtifactOffers({
    epoch: 1n,
    selectionKey: 'gallery-selection',
    shape: single
      ? {
          kind: 'single-file',
          file: { kind: 'file', id: new Uint8Array(16).fill(2), idText: 'photo', name, expectedSize: 524_288n },
          sourcePath: [name],
          exactBytes: 524_288n,
        }
      : {
          kind: 'tree',
          suggestedName: 'Summer photos',
          rootPath: ['Summer photos'],
          knownFileCount: 12,
          knownBytes: 6_710_886n,
          complete: true,
        },
    capabilities: {
      indexedDb: true,
      fsa: true,
      opfs: !single,
      portable: true,
      availableStorageBytes: 512n * 1024n * 1024n,
    },
  })
  const facts = TASK_FIXTURES[scenario === 'capacity' ? 'sender-capacity' : scenario] ?? TASK_FIXTURES['open-discovery']!
  const output = single
    ? { ...EMPTY_RECEIVE_RUNTIME, capabilitiesState: 'ready' as const, offers }
    : galleryTaskOutput(scenario, offers, facts)
  const regularRows = scenario === 'long-names'
    ? ROOT_ROWS.map(row => row.id === 'notes' ? { ...row, name: LONG_NAME } : row)
    : ROOT_ROWS
  const directoryRows = scenario === 'full-directory' ? FULL_DIRECTORY_ROWS : regularRows
  const directoryEntryCount = scenario === 'full-directory' ? CATALOG_PAGE_ENTRY_LIMIT : 18

  const retainedTasks: RetainedTaskSnapshot[] = [0, 1].map(index => ({
    taskId: 'saved-gallery-' + index,
    shareInstance,
    selectionKey: 'gallery-other-selection-' + index,
    backend: 'opfs',
    lifecycle: {
      kind: 'ready-to-save',
      taskId: 'saved-gallery-' + index,
      generation: 1n,
      size: 6_710_886n,
      fileName: 'Summer photos.zip',
    },
    display: {
      objectLabel: 'Summer photos',
      destinationLabel: index === 0 ? 'Downloads' : 'Archive',
      createdAtMilliseconds: 1_700_000_000_000 + index * 86_400_000,
    },
    actions: ['save', 'discard'],
    activeElsewhere: false,
    canSavePartial: false,
  }))

  return {
    share: single
      ? { kind: scenario === 'unsupported' ? 'file' : kind, shareInstance, name: file.name, file }
      : { kind: 'browser', shareInstance, name: 'Summer photos', homeDirectoryId: 'root', singleFolder: true },
    browse: { kind: 'ready', error: null },
    draft: { mode: 'scope', scope: 'current-folder', label: 'Summer photos', excludedItems: 2, empty: false },
    activeReceiveOperationId: output.activeTaskId,
    startAdmission: { allowed: single, reason: single ? null : 'downloading', canReleaseCurrent: false, localChoice: null },
    taskDisplay: single ? null : facts.display,
    connection: scenario === 'reconnecting'
      ? { kind: 'reconnecting', activity: { kind: 'connecting' } }
      : { kind: 'connected' },
    phase: 'browsing',
    status: null,
    error: null,
    pathActivity: {
      lanes: scenario === 'reconnecting'
        ? []
        : [
            { laneId: 1, laneEpoch: 0, route: 'application-relay', recentContent: false },
            { laneId: 3, laneEpoch: 1, route: 'direct', recentContent: !single },
          ],
    },
    rows: single ? [file] : directoryRows,
    breadcrumbs: [{ id: 'root', name: 'Summer photos' }],
    pageIndex: 0,
    pageCount: single || scenario === 'full-directory' ? 1 : 2,
    entryCount: single ? 1 : directoryEntryCount,
    omittedCount: 0n,
    selectedVisibleFiles: 1,
    selectedVisibleBytes: 16_384n,
    directoryRetryable: false,
    progress: {
      ...EMPTY_PROGRESS,
      ...facts.progress,
      ...(scenario === 'exact-progress' ? { discovery: 'complete' as const } : {}),
      ...(scenario === 'capacity' ? { capacityWaitVisible: true, capacityWaitingFiles: 2 } : {}),
      ...(scenario === 'partial-ready' ? { fileErrors: 1 } : {}),
    },
    preview: scenario === 'portrait' || scenario === 'landscape' ? photoPreview(scenario === 'portrait') : EMPTY_PREVIEW,
    output,
    retained: { kind: 'available', pending: null, tasks: retainedTasks },
  }
}
