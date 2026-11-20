import { SelectionPolicy } from '../../catalog/selection'
import type { BrowseDirectory, BrowsePage } from '../../application/browser-share/gateway'
import { filePreviewCapability } from '../../preview/capability'
import type { SelectionDraft, ShareIdentity } from '../model'

export const EMPTY_SELECTION_DRAFT: SelectionDraft = Object.freeze({
  mode: 'scope', scope: 'whole-share', label: { kind: 'shared-files' as const }, excludedItems: 0, empty: false,
})

export function scopeSelection(directory: BrowseDirectory): SelectionPolicy {
  const name = directory.path.at(-1)
  const selection = new SelectionPolicy(name === undefined)
  if (name !== undefined) {
    // The directory identity and ancestry came from the authenticated navigation route.
    selection.set({ kind: 'directory', id: directory.id, idText: directory.idText, name },
      directory.ancestry.slice(0, -1), true)
  }
  return selection
}

export function projectDraft(
  mode: SelectionDraft['mode'],
  directory: BrowseDirectory,
  selection: SelectionPolicy,
  share: ShareIdentity | null,
): SelectionDraft {
  if (mode === 'scope') {
    const name = directory.path.at(-1)
    return Object.freeze({
      mode, scope: name === undefined ? 'whole-share' : 'current-folder',
      label: name ?? share?.name ?? { kind: 'shared-files' as const },
      excludedItems: 0, empty: false,
    })
  }
  const intent = selection.intentSummary()
  return Object.freeze({ mode, scope: 'selected', empty: intent.empty, excludedItems: intent.excludedItems,
    label: intent.allSelected ? { kind: 'all-selected' as const } :
      { kind: 'selection' as const, folders: intent.selectedFolders, files: intent.selectedFiles } })
}

function filePresentationKind(name: string, expectedSize?: bigint): 'photo' | 'video' | 'file' {
  const capability = filePreviewCapability(name, expectedSize)
  if (capability === 'image') return 'photo'
  if (capability === 'video') return 'video'
  return 'file'
}

export function shareIdentityFromRoot(page: BrowsePage, shareInstance: string): ShareIdentity {
  const sole = page.entryCount === 1 && page.omittedCount === 0n && page.entries.length === 1
    ? page.entries[0] : undefined
  if (sole?.kind === 'file') {
    const kind = filePresentationKind(sole.name, sole.expectedSize)
    return Object.freeze({ kind, shareInstance, name: sole.name,
      file: Object.freeze({ id: sole.idText, kind: 'file', name: sole.name,
        expectedSize: sole.expectedSize, selection: 'selected' }) })
  }
  return Object.freeze({ kind: 'browser', shareInstance,
    name: sole?.name ?? null, homeDirectoryId: sole?.idText ?? page.directory.idText,
    singleFolder: sole?.kind === 'directory' })
}
