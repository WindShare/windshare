import { FailureNotice } from '../controls/FailureNotice'
import { selectionSummary } from '../draft/presentation'
import { receiverCopy } from '../locales/receiver'
import { useCopy } from '../preferences/use-preferences'
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

function SelectionCheckbox({ row, toggle, active, t }: { readonly row: BrowseRow; readonly toggle: () => void; readonly active: boolean; readonly t: typeof receiverCopy.en.browse }) {
  const input = useRef<HTMLInputElement>(null)
  const mixed = active && row.selection === 'mixed'
  const checked = active && row.selection === 'selected'
  useEffect(() => { if (input.current) input.current.indeterminate = mixed }, [mixed])
  return <label className="entry-selection"><input ref={input} type="checkbox" aria-label={t.select(row.name)}
    checked={checked} aria-checked={mixed ? 'mixed' : checked}
    onChange={toggle} /></label>
}

function entrySize(row: BrowseRow, t: typeof receiverCopy.en.browse): string {
  return row.expectedSize === undefined ? t.sizeUnknown : formatBytes(row.expectedSize)
}

function EntryBody({ row, isDirectory, previewable, t }: {
  readonly row: BrowseRow
  readonly isDirectory: boolean
  readonly t: typeof receiverCopy.en.browse
  readonly previewable: boolean
}) {
  const display = isDirectory ? null : fileDisplayInfo(row.name)
  const icon = display === null ? 'folder' : display.icon
  const meta = isDirectory ? t.folder : entrySize(row, t)
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
      {isDirectory && <><span className="entry-action-label">{t.open}</span><ReceiverIcon name="chevron-right" /></>}
      {previewable && <><ReceiverIcon name="eye" /><span className="entry-action-label">{t.preview}</span></>}
    </span>
  </>
}

function ExplorerRow({ row, draftMode, actions, t }: {
  readonly row: BrowseRow
  readonly t: typeof receiverCopy.en.browse
  readonly draftMode: ReceiverSnapshot['draft']['mode']
  readonly actions: ExplorerActions
}) {
  const isDirectory = row.kind === 'directory'
  const previewable = !isDirectory && canPreviewFile(row.name, row.expectedSize)
  const interactive = isDirectory || previewable
  return <li className={`explorer-row${interactive ? ' explorer-row-interactive' : ''}`}
    data-selection={draftMode === 'selection' ? row.selection : 'unselected'}>
    {draftMode === 'selection' && (
      <SelectionCheckbox t={t} row={row} active toggle={() => actions.toggle(row.id)} />
    )}
    {interactive ? (
      <button type="button" className="entry-trigger" title={row.name} aria-label={row.name}
        onClick={event => isDirectory ? actions.openDirectory(row.id) : actions.preview(row.id, event.currentTarget)}>
        <EntryBody t={t} row={row} isDirectory={isDirectory} previewable={previewable} />
      </button>
    ) : (
      <div className="entry-trigger entry-trigger-static" title={row.name}>
        <EntryBody t={t} row={row} isDirectory={false} previewable={false} />
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
  const t = useCopy(receiverCopy).browse
  const [viewMode, setViewMode] = useState<ExplorerViewMode>(readSavedViewMode)
  const selectView = (next: ExplorerViewMode) => {
    setViewMode(next)
    saveViewMode(next)
  }
  const selecting = draft.mode === 'selection'
  return <section className="explorer" aria-label={t.sharedFiles} aria-busy={browse.kind === 'loading'}>
    <div className="explorer-surface" data-view={viewMode} data-selection-mode={selecting ? 'active' : 'idle'}>
      <div className="explorer-toolbar">
        <nav className="breadcrumbs" aria-label={t.currentDirectory}>
          {breadcrumbs.map((crumb, index) => <span key={crumb.id}>
            {index === 0
              ? <span className="breadcrumbs-icon" aria-hidden="true"><ReceiverIcon name="folder" /></span>
              : <span className="breadcrumb-separator" aria-hidden="true"><ReceiverIcon name="chevron-right" /></span>}
            <button type="button" title={crumb.name ?? t.sharedFiles} disabled={index === breadcrumbs.length - 1}
              aria-current={index === breadcrumbs.length - 1 ? 'location' : undefined}
              onClick={() => actions.openBreadcrumb(index)}>{crumb.name ?? t.sharedFiles}</button>
          </span>)}
        </nav>
        <div className="explorer-toolbar-actions">
          <div className="explorer-view-toggle" role="group" aria-label={t.viewLayout}>
            <button type="button" className={viewMode === 'list' ? 'view-option-active' : ''}
              aria-label={t.listView} title={t.listView} aria-pressed={viewMode === 'list'}
              onClick={() => selectView('list')}>
              <ReceiverIcon name="view-list" />
            </button>
            <button type="button" className={viewMode === 'grid' ? 'view-option-active' : ''}
              aria-label={t.gridView} title={t.gridView} aria-pressed={viewMode === 'grid'}
              onClick={() => selectView('grid')}>
              <ReceiverIcon name="view-grid" />
            </button>
          </div>
          <button className={`selection-mode-action${draft.mode === 'selection' ? ' selection-mode-active' : ''}`}
            type="button" onClick={draft.mode === 'selection' ? actions.exitSelection : actions.enterSelection}>
            <ReceiverIcon name={draft.mode === 'selection' ? 'check' : 'select'} />
            {draft.mode === 'selection' ? t.doneSelecting : t.selectItems}
          </button>
        </div>
      </div>
      {draft.mode === 'selection' && <div className="selection-toolbar">
        <p>{selectionSummary(draft, t)}</p>
        <div>
          <button type="button" onClick={actions.selectPage}>{t.selectPage}</button>
          <button type="button" onClick={actions.clearSelection} disabled={draft.empty}>{t.clearSelection}</button>
        </div>
      </div>}
      {browse.kind === 'loading' && <p className="directory-notice" role="status">{browse.discoveredEntries === undefined
        ? t.loadingDirectory(browse.directoryName ?? t.sharedFiles)
        : t.scanningDirectory(browse.directoryName ?? t.sharedFiles, browse.discoveredEntries)}</p>}
      {browse.error !== null && <div className="directory-notice directory-error" role="alert">
        <FailureNotice failure={browse.error} context="directory" /><button type="button" onClick={actions.retry}>{t.retryDirectory}</button>
      </div>}
      {rows.length === 0 && browse.kind === 'ready' && <p className="directory-empty">
        {omittedCount > 0n ? t.noAvailable : t.emptyFolder}
      </p>}
      {rows.length > 0 && <>
        <div className="explorer-columns" aria-hidden="true">
          {selecting && <span />}
          <div className="explorer-columns-main">
            <span>{t.name}</span>
            <span className="explorer-columns-size">{t.size}</span>
            <span />
          </div>
        </div>
        <ul className="explorer-list" tabIndex={0} aria-label={t.folderContents}>
          {rows.map(row => <ExplorerRow t={t} key={row.id} row={row} draftMode={draft.mode} actions={actions} />)}
        </ul>
      </>}
      {pageCount > 1 && <nav className="directory-pagination" aria-label={t.directoryPages}>
        <button type="button" disabled={pageIndex === 0} onClick={() => actions.showPage(pageIndex - 1)}><ReceiverIcon name="chevron-left" />{t.previous}</button>
        <span>{t.page(pageIndex + 1, pageCount)}</span>
        <button type="button" disabled={pageIndex + 1 >= pageCount} onClick={() => actions.showPage(pageIndex + 1)}>{t.next}<ReceiverIcon name="chevron-right" /></button>
      </nav>}
    </div>
    {omittedCount > 0n && <p className="directory-notice directory-omitted">
      {t.omitted(omittedCount)}
    </p>}
  </section>
}
