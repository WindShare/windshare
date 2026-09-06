import { describe, expect, it } from 'vitest'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { projectDraft, scopeSelection, shareIdentityFromRoot } from '../../src/ui/draft/model'
import type { V2BrowsePage } from '../../src/ui/v2-gateway'

const id = (value: number) => new Uint8Array(16).fill(value)
const root = { id: id(1), idText: encodeBase64Url(id(1)), name: 'Shared files', path: [], ancestry: [encodeBase64Url(id(1))] }
const folder = { kind: 'directory' as const, id: id(2), idText: encodeBase64Url(id(2)), name: 'Photos' }
const photo = { kind: 'file' as const, id: id(3), idText: encodeBase64Url(id(3)), name: 'portrait.jpg', expectedSize: 512n }
const page = (entries: V2BrowsePage['entries'], facts: Partial<V2BrowsePage> = {}): V2BrowsePage =>
  ({ directory: root, pageIndex: 0, pageCount: 1, entryCount: entries.length, omittedCount: 0n, entries, ...facts })

describe('authenticated share identity and selection draft', () => {
  it('uses authenticated whole-root count and omissions rather than one visible row', () => {
    expect(shareIdentityFromRoot(page([photo]), 'share')).toMatchObject({ kind: 'photo', name: photo.name })
    expect(shareIdentityFromRoot(page([photo], { entryCount: 2, pageCount: 2 }), 'share').kind).toBe('browser')
    expect(shareIdentityFromRoot(page([photo], { omittedCount: 1n }), 'share').kind).toBe('browser')
    expect(shareIdentityFromRoot(page([folder]), 'share')).toMatchObject({
      kind: 'browser', singleFolder: true, name: 'Photos', homeDirectoryId: folder.idText,
    })
  })

  it('scopes a folder by identity while preserving its authenticated hierarchy', () => {
    const child = { ...folder, path: ['Photos'], ancestry: [...root.ancestry, folder.idText] }
    const current = page([photo], { directory: child })
    const selection = scopeSelection(current)
    expect(selection.snapshot().canonicalRules).toMatchObject([{ kind: 'directory', selected: true }])
    expect(selection.selected(photo, child.ancestry)).toBe(true)
    expect(selection.selected(photo, root.ancestry)).toBe(false)
    expect(projectDraft('scope', current, selection, null)).toMatchObject({
      scope: 'current-folder', label: 'Photos', empty: false,
    })
  })

  it('keeps explicit emptiness and cross-page exclusions independent of visible file totals', () => {
    const selection = new V2SelectionPolicy(false)
    selection.set(folder, root.ancestry, true)
    selection.set(photo, [...root.ancestry, folder.idText], false)
    expect(projectDraft('selection', page([]), selection, null)).toMatchObject({
      summary: '1 folder selected, excluding 1 item', empty: false,
    })
    selection.set(folder, root.ancestry, false)
    expect(projectDraft('selection', page([photo]), selection, null)).toMatchObject({
      scope: 'selected', empty: true, summary: 'Select items to download',
    })
  })
})
