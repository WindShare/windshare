import { V2SelectionPolicy } from '../../catalog/v2-selection'
import type { V2BrowsePage } from '../v2-gateway'
import type { V2SelectionDraft, V2ShareIdentity } from '../v2-model'

export const EMPTY_SELECTION_DRAFT: V2SelectionDraft = Object.freeze({
  mode: 'scope', scope: 'whole-share', label: 'Shared files', summary: 'All shared items', empty: false,
})

export function scopeSelection(page: V2BrowsePage): V2SelectionPolicy {
  const directory = page.directory
  const root = directory.path.length === 0
  const selection = new V2SelectionPolicy(root)
  if (!root) {
    // The directory identity and ancestry came from the authenticated navigation route.
    selection.set({ kind: 'directory', id: directory.id, idText: directory.idText, name: directory.name },
      directory.ancestry.slice(0, -1), true)
  }
  return selection
}

export function projectDraft(
  mode: V2SelectionDraft['mode'],
  page: V2BrowsePage,
  selection: V2SelectionPolicy,
  share: V2ShareIdentity | null,
): V2SelectionDraft {
  if (mode === 'scope') {
    const root = page.directory.path.length === 0
    return Object.freeze({
      mode, scope: root ? 'whole-share' : 'current-folder',
      label: root ? share?.name ?? 'Shared files' : page.directory.name,
      summary: root ? 'All shared items' : 'This folder and its contents', empty: false,
    })
  }
  const intent = selection.intentSummary()
  const selected = [itemCount(intent.selectedFolders, 'folder'), itemCount(intent.selectedFiles, 'file')]
    .filter(Boolean).join(', ')
  const label = intent.allSelected ? 'All shared items' : selected || 'Selected items'
  const exclusion = intent.excludedItems > 0 ? ', excluding ' + itemCount(intent.excludedItems, 'item') : ''
  const summary = intent.empty ? 'Select items to download' : label + ' selected' + exclusion
  return Object.freeze({ mode, scope: 'selected', label, empty: intent.empty, summary })
}

function itemCount(count: number, noun: string): string {
  if (count === 0) return ''
  return `${count} ${noun}${count === 1 ? '' : 's'}`
}

function filePresentationKind(name: string): 'photo' | 'video' | 'file' {
  if (/\.(png|jpe?g|webp)$/i.test(name)) return 'photo'
  if (/\.mp4$/i.test(name)) return 'video'
  return 'file'
}

export function shareIdentityFromRoot(page: V2BrowsePage, shareInstance: string): V2ShareIdentity {
  const sole = page.entryCount === 1 && page.omittedCount === 0n && page.entries.length === 1
    ? page.entries[0] : undefined
  if (sole?.kind === 'file') {
    const kind = filePresentationKind(sole.name)
    return Object.freeze({ kind, shareInstance, name: sole.name,
      file: Object.freeze({ id: sole.idText, kind: 'file', name: sole.name,
        expectedSize: sole.expectedSize, selection: 'selected' }) })
  }
  return Object.freeze({ kind: 'browser', shareInstance,
    name: sole?.name ?? 'Shared files', homeDirectoryId: sole?.idText ?? page.directory.idText,
    singleFolder: sole?.kind === 'directory' })
}
