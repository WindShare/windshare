import { useEffect, useRef, useState } from 'react'
import type { BrowseRow, Breadcrumb, ReceiverSnapshot } from '../model'
import { canPreviewFile } from '../../preview/capability'
import { formatBytes } from '../progress-presentation'
import { fileDisplayInfo } from './file-presentation'
import { ReceiverIcon } from '../receiver-presentation/ReceiverIcon'

type ExplorerViewMode = 'list' | 'grid'

const EXPLORER_VIEW_KEY = 'windshare.explorer.view'

function readSavedViewMode(): ExplorerViewMode {
  try {
    return typeof window !== 'undefined' && window.localStorage.getItem(EXPLORER_VIEW_KEY) === 'grid'
      ? 'grid' : 'list'
  } catch {
    return 'list'
  }
}

function saveViewMode(mode: ExplorerViewMode): void {
  try { window.localStorage.setItem(EXPLORER_VIEW_KEY, mode) } catch {
    // Restricted storage still allows switching between list and grid in memory.
  }
}

export interface ExplorerActions {
  readonly openDirectory: (id: string) => void
  readonly openBreadcrumb: (index: number) => void
  readonly showPage: (page: number) => void
  readonly preview: (id: string, invoker: HTMLButtonElement) => void
  readonly toggle: (id: string) => void
  readonly enterSelection: () => void
  readonly exitSelection: () => void
  readonly selectPage: () => void
  readonly clearSelection: () => void
  readonly retry: () => void
}

function SelectionCheckbox({ row, toggle, active }: { readonly row: BrowseRow; readonly toggle: () => void; readonly active: boolean }) {
  const input = useRef<HTMLInputElement>(null)
  const mixed = active && row.selection === 'mixed'
  const checked = active && row.selection === 'selected'
  useEffect(() => { if (input.current) input.current.indeterminate = mixed }, [mixed])
  return <label className="entry-selection"><input ref={input} type="checkbox" aria-label={`Select ${row.name}`}
    checked={checked} aria-checked={mixed ? 'mixed' : checked}
    onChange={toggle} /></label>
}

function entrySize(row: BrowseRow): string {
  return row.expectedSize === undefined ? 'Size unknown' : formatBytes(row.expectedSize)
}

function EntryBody({ row, isDirectory, previewable }: {
  readonly row: BrowseRow
  readonly isDirectory: boolean
  readonly previewable: boolean
}) {
  const display = isDirectory ? null : fileDisplayInfo(row.name)
  const icon = display === null ? 'folder' : display.icon
  const meta = isDirectory ? 'Folder' : entrySize(row)
  return <>
    <span className="entry-main">
      <span className={`entry-icon-badge entry-icon-${icon}`}>
        <ReceiverIcon name={icon} className="entry-icon" />
        {display?.badge !== null && display !== null &&
          <span className="entry-ext-tag" aria-hidden="true">{display.badge}</span>}
      </span>
      <span className="entry-label">
        <span className="entry-filename">{row.name}</span>
        <span className="entry-mobile-meta">{meta}</span>
      </span>
    </span>
    <span className="entry-metadata">{meta}</span>
    <span className={`entry-action-indicator${previewable ? ' entry-action-preview' : ''}${isDirectory ? ' entry-action-folder' : ''}`}
      aria-hidden="true">
      {isDirectory && <><span className="entry-action-label">Open</span><ReceiverIcon name="chevron-right" /></>}
      {previewable && <><ReceiverIcon name="eye" /><span className="entry-action-label">Preview</span></>}
    </span>
  </>
}

function ExplorerRow({ row, draftMode, actions }: {
  readonly row: BrowseRow
  readonly draftMode: ReceiverSnapshot['draft']['mode']
  readonly actions: ExplorerActions
}) {
  const isDirectory = row.kind === 'directory'
  const previewable = !isDirectory && canPreviewFile(row.name, row.expectedSize)
  const interactive = isDirectory || previewable
  return <li className={`explorer-row${interactive ? ' explorer-row-interactive' : ''}`}
    data-selection={draftMode === 'selection' ? row.selection : 'unselected'}>
    {draftMode === 'selection' && (
      <SelectionCheckbox row={row} active toggle={() => actions.toggle(row.id)} />
    )}
    {interactive ? (
      <button type="button" className="entry-trigger" title={row.name} aria-label={row.name}
        onClick={event => isDirectory ? actions.openDirectory(row.id) : actions.preview(row.id, event.currentTarget)}>
        <EntryBody row={row} isDirectory={isDirectory} previewable={previewable} />
      </button>
    ) : (
      <div className="entry-trigger entry-trigger-static" title={row.name}>
        <EntryBody row={row} isDirectory={false} previewable={false} />
      </div>
    )}
  </li>
}

export function Explorer({ rows, breadcrumbs, pageIndex, pageCount, omittedCount, browse, draft, actions }: {
  readonly rows: readonly BrowseRow[]
  readonly breadcrumbs: readonly Breadcrumb[]
  readonly pageIndex: number
  readonly pageCount: number
  readonly omittedCount: bigint
  readonly browse: ReceiverSnapshot['browse']
  readonly draft: ReceiverSnapshot['draft']
  readonly actions: ExplorerActions
}) {
  const [viewMode, setViewMode] = useState<ExplorerViewMode>(readSavedViewMode)
  const selectView = (next: ExplorerViewMode) => {
    setViewMode(next)
    saveViewMode(next)
  }
  const selecting = draft.mode === 'selection'
  return <section className="explorer" aria-label="Shared files" aria-busy={browse.kind === 'loading'}>
    <div className="explorer-surface" data-view={viewMode} data-selection-mode={selecting ? 'active' : 'idle'}>
      <div className="explorer-toolbar">
        <nav className="breadcrumbs" aria-label="Current directory">
          {breadcrumbs.map((crumb, index) => <span key={crumb.id}>
            {index === 0
              ? <span className="breadcrumbs-icon" aria-hidden="true"><ReceiverIcon name="folder" /></span>
              : <span className="breadcrumb-separator" aria-hidden="true"><ReceiverIcon name="chevron-right" /></span>}
            <button type="button" title={crumb.name} disabled={index === breadcrumbs.length - 1}
              aria-current={index === breadcrumbs.length - 1 ? 'location' : undefined}
              onClick={() => actions.openBreadcrumb(index)}>{crumb.name}</button>
          </span>)}
        </nav>
        <div className="explorer-toolbar-actions">
          <div className="explorer-view-toggle" role="group" aria-label="View layout">
            <button type="button" className={viewMode === 'list' ? 'view-option-active' : ''}
              aria-label="List view" title="List view" aria-pressed={viewMode === 'list'}
              onClick={() => selectView('list')}>
              <ReceiverIcon name="view-list" />
            </button>
            <button type="button" className={viewMode === 'grid' ? 'view-option-active' : ''}
              aria-label="Grid view" title="Grid view" aria-pressed={viewMode === 'grid'}
              onClick={() => selectView('grid')}>
              <ReceiverIcon name="view-grid" />
            </button>
          </div>
          <button className={`selection-mode-action${draft.mode === 'selection' ? ' selection-mode-active' : ''}`}
            type="button" onClick={draft.mode === 'selection' ? actions.exitSelection : actions.enterSelection}>
            <ReceiverIcon name={draft.mode === 'selection' ? 'check' : 'select'} />
            {draft.mode === 'selection' ? 'Done selecting' : 'Select items'}
          </button>
        </div>
      </div>
      {draft.mode === 'selection' && <div className="selection-toolbar">
        <p>{draft.summary}</p>
        <div>
          <button type="button" onClick={actions.selectPage}>Select this page</button>
          <button type="button" onClick={actions.clearSelection} disabled={draft.empty}>Clear selection</button>
        </div>
      </div>}
      {browse.kind === 'loading' && <p className="directory-notice" role="status">{browse.status || 'Loading this folder…'}</p>}
      {browse.error !== null && <div className="directory-notice directory-error" role="alert">
        <p>{browse.error}</p><button type="button" onClick={actions.retry}>Retry directory</button>
      </div>}
      {rows.length === 0 && browse.kind === 'ready' && <p className="directory-empty">
        {omittedCount > 0n ? 'No items are available in this folder.' : 'This folder is empty.'}
      </p>}
      {rows.length > 0 && <>
        <div className="explorer-columns" aria-hidden="true">
          {selecting && <span />}
          <div className="explorer-columns-main">
            <span>Name</span>
            <span className="explorer-columns-size">Size</span>
            <span />
          </div>
        </div>
        <ul className="explorer-list" tabIndex={0} aria-label="Folder contents">
          {rows.map(row => <ExplorerRow key={row.id} row={row} draftMode={draft.mode} actions={actions} />)}
        </ul>
      </>}
      {pageCount > 1 && <nav className="directory-pagination" aria-label="Directory pages">
        <button type="button" disabled={pageIndex === 0} onClick={() => actions.showPage(pageIndex - 1)}><ReceiverIcon name="chevron-left" />Previous</button>
        <span>Page {pageIndex + 1} of {pageCount}</span>
        <button type="button" disabled={pageIndex + 1 >= pageCount} onClick={() => actions.showPage(pageIndex + 1)}>Next<ReceiverIcon name="chevron-right" /></button>
      </nav>}
    </div>
    {omittedCount > 0n && <p className="directory-notice directory-omitted">
      {omittedCount.toString()} {omittedCount === 1n ? 'item couldn’t be shared and was' : 'items couldn’t be shared and were'} skipped. Available items can still be downloaded.
    </p>}
  </section>
}
