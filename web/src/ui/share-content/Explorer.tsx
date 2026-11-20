import { useEffect, useRef } from 'react'
import type { BrowseRow, Breadcrumb, ReceiverSnapshot } from '../model'
import { canPreviewFile } from '../../preview/capability'
import { formatBytes } from '../progress-presentation'
import { fileDisplayInfo } from './file-presentation'
import { ReceiverIcon } from '../receiver-presentation/ReceiverIcon'

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

function EntryContent({ row }: { readonly row: BrowseRow }) {
  const icon = row.kind === 'directory' ? 'folder' : fileDisplayInfo(row.name).icon
  return <>
    <span className={`entry-icon-badge entry-icon-${icon}`}>
      <ReceiverIcon name={icon} className="entry-icon" />
    </span>
    <span className="entry-label">
      <span className="entry-filename">{row.name}</span>
      <span className="entry-mobile-meta">
        {row.kind === 'directory' ? 'Folder' : entrySize(row)}
      </span>
    </span>
  </>
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
  return <section className="explorer" aria-label="Shared files" aria-busy={browse.kind === 'loading'}>
    <div className="explorer-toolbar">
      <nav className="breadcrumbs" aria-label="Current directory">
        {breadcrumbs.map((crumb, index) => <span key={crumb.id}>
          {index > 0 && <span className="breadcrumb-separator" aria-hidden="true">/</span>}
          <button type="button" title={crumb.name} disabled={index === breadcrumbs.length - 1}
            aria-current={index === breadcrumbs.length - 1 ? 'location' : undefined}
            onClick={() => actions.openBreadcrumb(index)}>{crumb.name}</button>
        </span>)}
      </nav>
      <button className={`quiet-action selection-mode-action${draft.mode === 'selection' ? ' selection-mode-active' : ''}`}
        type="button" onClick={draft.mode === 'selection' ? actions.exitSelection : actions.enterSelection}>
        <ReceiverIcon name={draft.mode === 'selection' ? 'check' : 'select'} />
        {draft.mode === 'selection' ? 'Done selecting' : 'Select items'}
      </button>
    </div>
    <div className="explorer-card">
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
      {rows.length > 0 && <ul className="explorer-list" tabIndex={0} aria-label="Folder contents">
        {rows.map(row => {
          const isDirectory = row.kind === 'directory'
          const previewable = !isDirectory && canPreviewFile(row.name, row.expectedSize)
          return <li key={row.id} className={`explorer-row${isDirectory || previewable ? ' explorer-row-interactive' : ''}`}
            data-selection={draft.mode === 'selection' ? row.selection : 'unselected'}>
            <SelectionCheckbox row={row} active={draft.mode === 'selection'} toggle={() => actions.toggle(row.id)} />
            {isDirectory || previewable ? (
              <button type="button" className="entry-name" title={row.name} aria-label={row.name}
                onClick={event => isDirectory ? actions.openDirectory(row.id) : actions.preview(row.id, event.currentTarget)}>
                <EntryContent row={row} />
              </button>
            ) : (
              <div className="entry-name entry-name-static" title={row.name}>
                <EntryContent row={row} />
              </div>
            )}
            <span className="entry-metadata">{isDirectory ? 'Folder' : entrySize(row)}</span>
            <span className={`entry-action-hint${previewable ? ' entry-action-preview' : ''}${isDirectory ? ' entry-action-folder' : ''}`}
              aria-hidden="true">
              {isDirectory && <>Open<ReceiverIcon name="chevron-right" /></>}
              {previewable && <><ReceiverIcon name="eye" />Preview</>}
            </span>
          </li>
        })}
      </ul>}
    </div>
    {omittedCount > 0n && <p className="directory-notice directory-omitted">
      {omittedCount.toString()} {omittedCount === 1n ? 'item couldn’t be shared and was' : 'items couldn’t be shared and were'} skipped. Available items can still be downloaded.
    </p>}
    {pageCount > 1 && <nav className="directory-pagination" aria-label="Directory pages">
      <button type="button" disabled={pageIndex === 0} onClick={() => actions.showPage(pageIndex - 1)}><ReceiverIcon name="chevron-left" />Previous</button>
      <span>Page {pageIndex + 1} of {pageCount}</span>
      <button type="button" disabled={pageIndex + 1 >= pageCount} onClick={() => actions.showPage(pageIndex + 1)}>Next<ReceiverIcon name="chevron-right" /></button>
    </nav>}
  </section>
}
