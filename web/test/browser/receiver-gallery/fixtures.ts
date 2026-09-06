import { offerArtifacts } from '../../../src/output/planning'
import { createSelectionSpec, createZipArchiveArtifact, createCompleteDirectoryResultRoot, createWorkspaceBinding, createWorkspaceThenPublishPlan } from '../../../src/transfer/intent'
import { EMPTY_V2_OUTPUT_PRESENTATION } from '../../../src/ui/v2-output'
import { EMPTY_V2_PREVIEW, EMPTY_V2_PROGRESS, type V2BrowseRow, type V2PreviewSnapshot, type V2ReceiverSnapshot } from '../../../src/ui/v2-model'
import { presentReceiveLifecycle } from '../../../src/ui/v2-lifecycle-presentation'
import { TASK_FIXTURES } from '../../../src/ui/tasks/fixtures'
import { COMPLETE_DISCOVERY, environment, fsaTarget, handoffTarget, identity, portableOffer, projection, singleFileProof, treeProof, workspaceOffer } from '../../output/planning/fixture'

export const SCENARIOS = ['portal', 'portal-empty', 'portal-loading', 'portal-failed', 'portal-saved', 'folder', 'exact-progress', 'long-names', 'full-directory', 'portrait', 'landscape', 'video', 'unsupported', 'reconnecting', 'capacity', 'verifying', 'partial-ready', 'browser-handoff', 'saved-cleanup'] as const
export type Scenario = typeof SCENARIOS[number]

export const ROOT_ROWS: readonly V2BrowseRow[] = [
  { id: 'photos', kind: 'directory', name: 'Summer photos', selection: 'mixed' },
  { id: 'notes', kind: 'file', name: 'A very long project notes filename describing the entire summer holiday and its carefully preserved original details.txt', expectedSize: 16_384n, selection: 'selected' },
  ...Array.from({ length: 7 }, (_, index): V2BrowseRow => ({
    id: 'item-' + index, kind: 'file', name: 'Landscape ' + (index + 1) + '.png',
    expectedSize: 524_288n, selection: 'unselected',
  })),
]

export const LONG_NAME = '????????? ? ' + 'UnbrokenProjectName'.repeat(7) + '.txt'

const CATALOG_PAGE_ENTRY_LIMIT = 256
export const FULL_DIRECTORY_ROWS: readonly V2BrowseRow[] = Array.from({ length: CATALOG_PAGE_ENTRY_LIMIT }, (_, index) => ({
  id: 'full-item-' + index, kind: 'file', name: 'Document ' + (index + 1) + '.txt',
  expectedSize: 16_384n, selection: 'unselected',
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

export function photoPreview(portrait: boolean, fileId = 'photo'): V2PreviewSnapshot {
  const width = portrait ? 400 : 800
  const height = portrait ? 700 : 450
  return { state: 'image', fileId, name: portrait ? 'Portrait.png' : 'Landscape.png',
    url: syntheticPhoto(width, height), presentationId: 1, mimeType: 'image/png', width, height }
}

async function portalInventorySnapshot(scenario: Scenario): Promise<V2ReceiverSnapshot> {
  const snapshot = await gallerySnapshot(scenario === 'portal-saved' ? 'saved-cleanup' : 'folder')
  let retained: V2ReceiverSnapshot['retained'] = { kind: 'ready', operations: [], pending: null, error: null }
  if (scenario === 'portal-saved') return { ...snapshot, retained }
  if (scenario === 'portal-loading') retained = { ...retained, kind: 'loading', pending: null }
  if (scenario === 'portal-failed') retained = { ...retained, kind: 'failed', pending: null, error: 'Stored receive tasks could not be loaded.' }
  return { ...snapshot, retained, output: EMPTY_V2_OUTPUT_PRESENTATION, taskDisplay: null, progress: EMPTY_V2_PROGRESS }
}

export async function gallerySnapshot(scenario: Scenario): Promise<V2ReceiverSnapshot> {
  if (scenario.startsWith('portal-')) return portalInventorySnapshot(scenario)
  const single = ['portrait', 'landscape', 'video', 'unsupported'].includes(scenario)
  const selection = await createSelectionSpec({
    shareInstance: identity(1), syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const names = { video: 'Summer afternoon.mp4', unsupported: 'Project archive.7z', portrait: 'Portrait.png', landscape: 'Landscape.png' }
  const name = names[scenario as keyof typeof names] ?? 'Summer photos'
  const kind = scenario === 'video' ? 'video' : 'photo'
  const proof = single ? singleFileProof() : treeProof()
  const offers = await offerArtifacts(projection(selection, proof, 128n), COMPLETE_DISCOVERY,
    environment({ targets: [fsaTarget(), handoffTarget()], portable: portableOffer(), workspace: single ? null : workspaceOffer() }))
  const facts = TASK_FIXTURES[scenario === 'capacity' ? 'sender-capacity' : scenario] ?? TASK_FIXTURES['open-discovery']!
  const file: V2BrowseRow = { id: 'photo', kind: 'file',
    name,
    expectedSize: 524_288n, selection: 'selected' }
  const artifact = await createZipArchiveArtifact(createCompleteDirectoryResultRoot(identity(30), 'Summer photos'))
  const binding = await createWorkspaceBinding({ operationId: identity(40), workspaceId: identity(41),
    repositoryRef: identity(42, 32), artifact })
  const plan = await createWorkspaceThenPublishPlan(artifact, binding)
  const lifecyclePresentation = presentReceiveLifecycle({
    state: facts.lifecycle, artifact, plan, nowMilliseconds: 1_700_000_000_000,
    activeControls: facts.lifecycle.kind === 'receiving' ? ['pause', 'stop'] : [],
    repairSummary: scenario === 'saved-cleanup' ? {
      committedCount: 1, logicalPathSample: [['project', 'settings.cfg']],
      pairDisplayNames: { script: 'restore.windshare-abc234.ps1', sidecar: 'restore.windshare-abc234.data' },
      placement: 'inside-logical-root', sidecarSync: 'current', terminalSettlement: 'complete',
      latestObservedFooter: { committedCount: 1, state: 'completed' },
    } : null,
  })
  const output = single ? { ...EMPTY_V2_OUTPUT_PRESENTATION, offers } : {
    ...EMPTY_V2_OUTPUT_PRESENTATION, offers, lifecycle: facts.lifecycle, directZipProgress: facts.directZipProgress,
    plan, resolvedArtifact: artifact, lifecyclePresentation,
  }
  const regularRows = scenario === 'long-names' ? ROOT_ROWS.map(row => row.id === 'notes' ? { ...row, name: LONG_NAME } : row) : ROOT_ROWS
  const directoryRows = scenario === 'full-directory' ? FULL_DIRECTORY_ROWS : regularRows
  const directoryEntryCount = scenario === 'full-directory' ? CATALOG_PAGE_ENTRY_LIMIT : 18
  return {
    share: single ? { kind: scenario === 'unsupported' ? 'file' : kind,
      shareInstance: identity(1), name: file.name, file }
      : { kind: 'browser', shareInstance: identity(1), name: 'Summer photos', homeDirectoryId: 'root', singleFolder: true },
    browse: { kind: 'ready', status: 'Ready', error: null },
    draft: { mode: 'scope', scope: 'current-folder', label: 'Summer photos', summary: '1 folder selected, excluding 2 items', empty: false },
    startAdmission: { allowed: single, reason: single ? null : 'Pause the current download before starting another.', canReleaseCurrent: false },
    taskDisplay: single ? null : facts.display,
    connection: { kind: scenario === 'reconnecting' ? 'reconnecting' : 'connected' },
    phase: 'browsing', status: 'Sender connected', error: null,
    pathActivity: { directConnected: true, content: single ? 'idle' : 'direct' },
    rows: single ? [file] : directoryRows, breadcrumbs: [{ id: 'root', name: 'Summer photos' }],
    pageIndex: 0, pageCount: single || scenario === 'full-directory' ? 1 : 2,
    entryCount: single ? 1 : directoryEntryCount,
    omittedCount: 0n, selectedVisibleFiles: 1, selectedVisibleBytes: 16_384n, directoryRetryable: false,
    progress: { ...EMPTY_V2_PROGRESS, ...facts.progress,
      ...(scenario === 'exact-progress' ? { discovery: 'complete' as const } : {}),
      ...(scenario === 'capacity' ? { capacityWaitVisible: true, capacityWaitingFiles: 2 } : {}),
      ...(scenario === 'partial-ready' ? { fileErrors: 1 } : {}) },
    preview: scenario === 'portrait' || scenario === 'landscape' ? photoPreview(scenario === 'portrait') : EMPTY_V2_PREVIEW,
    output,
    retained: { kind: 'ready', pending: null, error: null, operations: [0, 1].map(index => ({
      operationId: 'saved-gallery-' + index, receiveIntentDigest: 'fixture-intent', lifecycleGeneration: 1n,
      lifecycle: { kind: 'waiting-to-save', operationId: 'saved-gallery-' + index,
        receiveIntentDigest: 'fixture-intent', generation: 1n, packageDigest: 'fixture-package' },
      display: { objectLabel: 'Summer photos', destinationLabel: index === 0 ? 'Downloads' : 'Archive',
        createdAtMilliseconds: 1_700_000_000_000 + index * 86_400_000 },
      continuation: 'save-artifact', actions: ['save', 'delete'], shareInstance: identity(1),
    })) },
  }
}
