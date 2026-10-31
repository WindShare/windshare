import { useEffect, useRef } from 'react'
import type { BrowseRow, Breadcrumb, ReceiverSnapshot } from '../model'
import { formatBytes } from '../progress-presentation'
import { ReceiverIcon, type ReceiverIconName } from '../receiver-presentation/ReceiverIcon'

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

const IMAGE_FILENAME = /\.(?:avif|gif|heic|heif|jpe?g|png|svg|webp)$/iu
const VIDEO_FILENAME = /\.(?:m4v|mkv|mov|mp4|webm)$/iu

function entryIcon(row: BrowseRow): ReceiverIconName {
  if (row.kind === 'directory') return 'folder'
  // Filename decoration never grants preview support or establishes a media type.
  if (IMAGE_FILENAME.test(row.name)) return 'image'
  if (VIDEO_FILENAME.test(row.name)) return 'video'
  return 'file'
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
      <button className="quiet-action selection-mode-action" type="button" onClick={draft.mode === 'selection' ? actions.exitSelection : actions.enterSelection}>
        <ReceiverIcon name={draft.mode === 'selection' ? 'check' : 'select'} />
        {draft.mode === 'selection' ? 'Done selecting' : 'Select items'}
      </button>
    </div>
    {draft.mode === 'selection' && <div className="selection-toolbar">
      <p>{draft.summary}</p>
      <div><button type="button" onClick={actions.selectPage}>Select this page</button>
        <button type="button" onClick={actions.clearSelection} disabled={draft.empty}>Clear selection</button></div>
    </div>}
    {browse.kind === 'loading' && <p className="directory-notice" role="status">{browse.status || 'Loading this folder…'}</p>}
    {browse.error !== null && <div className="directory-notice directory-error" role="alert">
      <p>{browse.error}</p><button type="button" onClick={actions.retry}>Retry directory</button>
    </div>}
    {rows.length === 0 && browse.kind === 'ready' && <p className="directory-empty">
      {omittedCount > 0n ? 'No supported items in this folder.' : 'This folder is empty.'}
    </p>}
    {rows.length > 0 && <ul className="explorer-list" tabIndex={0} aria-label="Folder contents">
        {rows.map(row => <li key={row.id} className="explorer-row"
          data-selection={draft.mode === 'selection' ? row.selection : 'unselected'}>
          <SelectionCheckbox row={row} active={draft.mode === 'selection'} toggle={() => actions.toggle(row.id)} />
          <button type="button" className="entry-name" title={row.name} aria-label={row.name}
            onClick={event => row.kind === 'directory' ? actions.openDirectory(row.id) : actions.preview(row.id, event.currentTarget)}>
            <ReceiverIcon name={entryIcon(row)} className="entry-icon" />
            <span className="entry-label"><span className="entry-filename">{row.name}</span>
              <span className="entry-type">{row.kind === 'directory' ? 'Folder' : 'File'}
                {row.kind === 'file' && <span className="entry-mobile-size"> · {entrySize(row)}</span>}
              </span></span>
          </button>
          <span className="entry-metadata">{row.kind === 'directory' ? '—' : entrySize(row)}</span>
          <span className="entry-action-hint" aria-hidden="true">{row.kind === 'directory' ? 'Open' : 'Preview'}</span>
        </li>)}
      </ul>}
    {omittedCount > 0n && <p className="directory-notice">{omittedCount.toString()} unsupported source items were not included. Supported items can still be downloaded.</p>}
    {pageCount > 1 && <nav className="directory-pagination" aria-label="Directory pages">
      <button type="button" disabled={pageIndex === 0} onClick={() => actions.showPage(pageIndex - 1)}><ReceiverIcon name="chevron-left" />Previous</button>
      <span>Page {pageIndex + 1} of {pageCount}</span>
      <button type="button" disabled={pageIndex + 1 >= pageCount} onClick={() => actions.showPage(pageIndex + 1)}>Next<ReceiverIcon name="chevron-right" /></button>
    </nav>}
  </section>
}
