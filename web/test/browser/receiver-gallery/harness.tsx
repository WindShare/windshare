import { createRoot, type Root } from 'react-dom/client'
import { flushSync } from 'react-dom'
import { PortalApp } from '../../../src/ui/portal/PortalApp'
import { V2ReceiverApp } from '../../../src/ui/V2ReceiverApp'
import type { V2ReceiverController } from '../../../src/ui/v2-controller'
import type { V2ReceiverSnapshot } from '../../../src/ui/v2-model'
import { EMPTY_V2_PREVIEW } from '../../../src/ui/v2-model'
import { gallerySnapshot, photoPreview, ROOT_ROWS, SCENARIOS, type Scenario } from './fixtures'
import { syntheticVideo } from './video'
import '../../../src/index.css'
import '../../../src/App.css'

let root: Root | undefined
let active: GalleryController | undefined

class GalleryController {
  #snapshot: V2ReceiverSnapshot
  readonly #listeners = new Set<() => void>()
  readonly intents: string[] = []
  #videoUrl: string | undefined
  close = () => { if (this.#videoUrl !== undefined) URL.revokeObjectURL(this.#videoUrl) }
  constructor(snapshot: V2ReceiverSnapshot) { this.#snapshot = snapshot }
  getSnapshot = () => this.#snapshot
  subscribe = (listener: () => void) => {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }
  #publish(patch: Partial<V2ReceiverSnapshot>) {
    this.#snapshot = { ...this.#snapshot, ...patch }
    for (const listener of this.#listeners) listener()
  }
  recordExperienceIntent = (action: string) => { this.intents.push(action) }
  submitKey = () => { this.intents.push('submit-key') }
  toggleSelection = (id: string) => {
    this.intents.push('toggle:' + id)
    const rows = this.#snapshot.rows.map(row => row.id === id
      ? { ...row, selection: row.selection === 'selected' ? 'unselected' as const : 'selected' as const } : row)
    this.#publish({ rows, draft: { ...this.#snapshot.draft, mode: 'selection', scope: 'selected',
      empty: !rows.some(row => row.selection !== 'unselected'), summary: 'Selected items across this share' } })
  }
  enterSelectionMode = () => this.#publish({ draft: { ...this.#snapshot.draft, mode: 'selection', scope: 'selected' } })
  exitSelectionMode = () => this.#publish({ draft: { ...this.#snapshot.draft, mode: 'scope', scope: 'current-folder', empty: false } })
  clearSelection = () => this.#publish({
    rows: this.#snapshot.rows.map(row => ({ ...row, selection: 'unselected' })),
    draft: { ...this.#snapshot.draft, mode: 'selection', scope: 'selected', empty: true, summary: 'Select items to download' },
  })
  selectPage = () => this.#publish({
    rows: this.#snapshot.rows.map(row => ({ ...row, selection: 'selected' })),
    draft: { ...this.#snapshot.draft, mode: 'selection', scope: 'selected', empty: false },
  })
  openDirectory = (id: string) => {
    this.intents.push('open-directory:' + id)
    this.#publish({ breadcrumbs: [{ id: 'root', name: 'Summer photos' }, { id, name: 'Selected folder' }],
      rows: [{ id: 'child', kind: 'file', name: 'Portrait.png', expectedSize: 524_288n, selection: 'selected' }], pageIndex: 0, pageCount: 1 })
  }
  openBreadcrumb = () => this.#publish({ breadcrumbs: [{ id: 'root', name: 'Summer photos' }], rows: ROOT_ROWS, pageCount: 2 })
  showPage = (pageIndex: number) => {
    this.intents.push('page:' + pageIndex)
    this.#publish({ pageIndex, rows: ROOT_ROWS.map(row => ({ ...row, id: row.id + '-page-' + pageIndex })) })
  }
  retryDirectory = () => this.#publish({ browse: { kind: 'ready', status: 'Ready', error: null } })
  previewFile = (id: string) => {
    this.intents.push('preview:' + id)
    if (this.#snapshot.share?.kind === 'video') {
      const name = this.#snapshot.share.name
      this.#publish({ preview: { state: 'loading', fileId: id, name } })
      syntheticVideo().then(({ url, mimeType }) => {
        this.#videoUrl = url
        this.#publish({ preview: { state: 'video', fileId: id, name, url, presentationId: 2,
          mimeType, width: 640, height: 360, durationSeconds: 0.3, positionSeconds: 0, seeking: false } })
      }).catch(() => this.previewMediaFailed())
      return
    }
    this.#publish({ preview: this.#snapshot.share?.kind === 'file'
      ? { state: 'error', fileId: id, name: this.#snapshot.share.name, message: 'This format cannot be previewed in this browser.' }
      : photoPreview(true, id) })
  }
  cancelPreview = () => this.#publish({ preview: EMPTY_V2_PREVIEW })
  seekPreview = (seconds: number) => { this.intents.push('seek:' + seconds) }
  previewMediaPresented = () => undefined
  previewMediaFailed = () => {
    this.#publish({ preview: { state: 'error', fileId: 'photo', name: 'Photo', message: 'The browser could not decode this preview.' } })
  }
  chooseArtifact = (id: string) => { this.intents.push('choose:' + id) }
  retryOutputConfirmation = () => { this.intents.push('retry-output') }
  cancelPreparing = () => { this.intents.push('cancel-preparing') }
  performLifecycleAction = () => { this.intents.push('task-action') }
  retainedActionAdmission = () => ({ allowed: true, reason: null })
  activeLifecycleActionAdmission = () => ({ allowed: true, reason: null })
  canRetainCurrentOperation = false
  retainCurrentOperation = async () => false
  performRetainedAction = () => { this.intents.push('retained-action') }
  catchUpStoppedCompatibleNames = () => undefined
  startNewReceiveOperation = () => undefined
  prepareReplacementDownload = () => undefined
}

export async function mountGallery(scenario: Scenario = 'folder'): Promise<void> {
  root?.unmount()
  active?.close()
  active = new GalleryController(await gallerySnapshot(scenario))
  // Vite may give dynamic imports distinct module URLs; bind evidence to the mounted controller.
  Object.assign(window, { windshareGalleryEvidence: galleryEvidence })
  const container = document.createElement('div')
  container.dataset.galleryScenario = scenario
  const toolbar = document.createElement('nav')
  toolbar.setAttribute('aria-label', 'Synthetic fixture gallery')
  toolbar.style.cssText = 'padding:8px 16px;background:#edf2ef;font:13px system-ui'
  const label = document.createElement('label')
  label.textContent = 'Synthetic scenario: '
  const select = document.createElement('select')
  select.setAttribute('aria-label', 'Synthetic scenario')
  for (const value of SCENARIOS) select.add(new Option(value, value, false, scenario === value))
  select.onchange = () => { void mountGallery(select.value as Scenario) }
  label.append(select)
  toolbar.append(label)
  document.body.replaceChildren(toolbar, container)
  root = createRoot(container)
  // Test data supplies snapshots and records intents; every rendered control is
  // the production receiver, with no networking or destination authority.
  const Surface = scenario.startsWith('portal') ? PortalApp : V2ReceiverApp
  flushSync(() => root!.render(<Surface controller={active as unknown as V2ReceiverController} />))
}

export function galleryEvidence() {
  const snapshot = active!.getSnapshot()
  return {
    intents: [...active!.intents],
    taskId: snapshot.output.lifecycle?.operationId ?? null,
    taskLabel: snapshot.taskDisplay?.objectLabel ?? null,
    taskBytes: snapshot.progress.writtenBytes.toString(),
    draftEmpty: snapshot.draft.empty,
  }
}
