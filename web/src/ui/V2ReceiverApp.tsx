import { useEffect, useRef, useSyncExternalStore, type FormEvent } from 'react'

import { PORTABLE_DOWNLOAD_MAXIMUM_BYTES } from '../output/portable/browser-download'
import type { V2BrowseRow, V2PreviewSnapshot } from './v2-model'
import type { V2ReceiverController } from './v2-controller'

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB'] as const

function formatBytes(bytes: bigint): string {
  let value = bytes
  let unit = 0
  let divisor = 1n
  while (value >= 1024n && unit < BYTE_UNITS.length - 1) {
    value /= 1024n
    divisor *= 1024n
    unit += 1
  }
  if (unit === 0) return `${bytes} B`
  const tenths = (bytes * 10n) / divisor
  return `${tenths / 10n}.${tenths % 10n} ${BYTE_UNITS[unit]}`
}

function downloadCapabilityDescription(capabilities: {
  readonly nativeSave: boolean
  readonly portableDownload: boolean
  readonly originPrivateStaging: boolean
}): string {
  if (capabilities.nativeSave) {
    return capabilities.originPrivateStaging
      ? 'Native save stream; progressive archives stage privately before export.'
      : 'Native save stream without browser-level restart recovery.'
  }
  if (capabilities.portableDownload) {
    return `Buffered download up to ${formatBytes(BigInt(PORTABLE_DOWNLOAD_MAXIMUM_BYTES))}; no reload recovery.`
  }
  return 'Browser downloads are unavailable.'
}

function SelectionCheckbox(props: {
  readonly row: V2BrowseRow
  readonly disabled: boolean
  readonly onToggle: () => void
}) {
  const input = useRef<HTMLInputElement>(null)
  useEffect(() => {
    if (input.current !== null) input.current.indeterminate = props.row.selection === 'mixed'
  }, [props.row.selection])
  return (
    <input
      ref={input}
      type="checkbox"
      aria-label={`Select ${props.row.name}`}
      checked={props.row.selection !== 'unselected'}
      disabled={props.disabled}
      onChange={props.onToggle}
    />
  )
}

function KeyForm({ controller }: { readonly controller: V2ReceiverController }) {
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const input = event.currentTarget.elements.namedItem('capability-key')
    if (!(input instanceof HTMLInputElement)) return
    const key = input.value
    input.value = ''
    controller.submitKey(key)
  }
  return (
    <form className="key-form" onSubmit={submit}>
      <label htmlFor="capability-key">Separate key</label>
      <p id="key-help">Paste the suite-02 key or the complete WindShare link.</p>
      <div className="key-entry">
        <input
          id="capability-key"
          name="capability-key"
          type="password"
          autoComplete="off"
          spellCheck={false}
          aria-describedby="key-help"
          required
          autoFocus
        />
        <button type="submit">Open share</button>
      </div>
    </form>
  )
}

function formatTime(seconds: number): string {
  const whole = Math.max(0, Math.floor(seconds))
  const minutes = Math.floor(whole / 60)
  const remainder = whole % 60
  return `${minutes}:${remainder.toString().padStart(2, '0')}`
}

function VideoPreview(props: {
  readonly preview: Extract<V2PreviewSnapshot, { state: 'video' }>
  readonly controller: V2ReceiverController
}) {
  const position = () => {
    if (video.current !== null) video.current.currentTime = props.preview.positionSeconds
  }
  const video = useRef<HTMLVideoElement>(null)
  return (
    <>
      <video
        key={props.preview.url}
        ref={video}
        className="preview-media"
        src={props.preview.url}
        aria-label={`Video preview of ${props.preview.name}`}
        muted
        playsInline
        preload="auto"
        onLoadedMetadata={position}
        onLoadedData={position}
        onError={() => props.controller.previewMediaFailed(props.preview.url)}
      />
      <label className="preview-seek">
        <span>
          {formatTime(props.preview.positionSeconds)} / {formatTime(props.preview.durationSeconds)}
          {props.preview.seeking ? ' · seeking…' : ''}
        </span>
        <input
          type="range"
          min={0}
          max={props.preview.durationSeconds}
          step={Math.max(0.1, props.preview.durationSeconds / 1_000)}
          value={props.preview.positionSeconds}
          aria-label={`Seek ${props.preview.name}`}
          aria-busy={props.preview.seeking}
          onChange={(event) => props.controller.seekPreview(event.currentTarget.valueAsNumber)}
        />
      </label>
    </>
  )
}

function ImagePreview(props: {
  readonly preview: Extract<V2PreviewSnapshot, { state: 'image' }>
  readonly controller: V2ReceiverController
}) {
  return (
    <img
      className="preview-media"
      src={props.preview.url}
      alt={`Preview of ${props.preview.name}`}
      onError={() => props.controller.previewMediaFailed(props.preview.url)}
    />
  )
}

function PreviewPanel(props: {
  readonly preview: V2PreviewSnapshot
  readonly controller: V2ReceiverController
}) {
  if (props.preview.state === 'idle') return null
  const details = props.preview.state === 'image' || props.preview.state === 'video'
    ? `${props.preview.width} × ${props.preview.height}`
    : undefined
  return (
    <section className="preview-panel" aria-label="File preview" aria-live="polite">
      <header>
        <div>
          <strong>{props.preview.name}</strong>
          {details !== undefined && <small>{details}</small>}
        </div>
        <button type="button" onClick={() => props.controller.cancelPreview()}>Close preview</button>
      </header>
      {props.preview.state === 'loading' && <p>Opening a bounded preview…</p>}
      {props.preview.state === 'error' && <p role="alert">{props.preview.message}</p>}
      {props.preview.state === 'image' && (
        <ImagePreview preview={props.preview} controller={props.controller} />
      )}
      {props.preview.state === 'video' && (
        <VideoPreview preview={props.preview} controller={props.controller} />
      )}
    </section>
  )
}

export function V2ReceiverApp({ controller }: { readonly controller: V2ReceiverController }) {
  const snapshot = useSyncExternalStore(
    controller.subscribe,
    controller.getSnapshot,
    controller.getSnapshot,
  )
  const active = snapshot.phase === 'acquiring-output' ||
    snapshot.phase === 'discovering' || snapshot.phase === 'transferring' ||
    snapshot.phase === 'aborting'
  const selectionLocked = active || snapshot.phase !== 'browsing'
  const status = useRef<HTMLParagraphElement>(null)
  const alert = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (snapshot.phase === 'failed') alert.current?.focus()
    else if (snapshot.phase === 'completed' || snapshot.phase === 'completed-errors' ||
      snapshot.phase === 'aborted') status.current?.focus()
  }, [snapshot.phase])

  return (
    <main className="receiver-shell">
      <header className="brand-header">
        <a className="brand" href="/" aria-label="WindShare home">
          <span className="brand-mark" aria-hidden="true">W</span>
          <span>WindShare</span>
        </a>
        <span className="privacy-note">End-to-end encrypted · suite 02</span>
      </header>

      <section className="receiver-card" aria-labelledby="receiver-title">
        <div className="card-heading">
          <p className="eyebrow">Receive securely</p>
          <h1 id="receiver-title">Browse and save shared files</h1>
           <p className="intro">
            Directory pages are authenticated on demand. Content is opened only after an explicit
            preview or receive action.
          </p>
        </div>

        <p ref={status} className="status-line" role="status" aria-live="polite" tabIndex={-1}>
          <span className={`status-dot status-${snapshot.phase}`} aria-hidden="true" />
          {snapshot.status}
        </p>

        {snapshot.error !== null && (
          <div ref={alert} className="error-banner" role="alert" tabIndex={-1}>
            {snapshot.error}
            {snapshot.directoryRetryable && (
              <button type="button" onClick={() => controller.retryDirectory()}>Retry directory</button>
            )}
          </div>
        )}

        {snapshot.phase === 'awaiting-key' && <KeyForm controller={controller} />}

        {snapshot.breadcrumbs.length > 0 && (
          <div className="download-layout">
            <section className="selection-panel" aria-label="Shared files">
              <nav className="selection-pagination" aria-label="Current directory">
                {snapshot.breadcrumbs.map((crumb, index) => (
                  <button
                    type="button"
                    key={crumb.id}
                    disabled={index === snapshot.breadcrumbs.length - 1 || active}
                    onClick={() => controller.openBreadcrumb(index)}
                  >
                    {crumb.name}
                  </button>
                ))}
              </nav>
              <p className="selection-summary">
                {snapshot.selectionTotalKnown
                  ? `${snapshot.selectedVisibleFiles} file(s), ${formatBytes(snapshot.selectedVisibleBytes)}`
                  : `${snapshot.selectedVisibleFiles} visible file(s), at least ${formatBytes(snapshot.selectedVisibleBytes)}; recursive total unknown`}
              </p>
              <ul className="selection-list">
                {snapshot.rows.map((row) => (
                  <li key={row.id}>
                    <div className="selection-row">
                      <SelectionCheckbox
                        row={row}
                        disabled={selectionLocked}
                        onToggle={() => controller.toggleSelection(row.id)}
                      />
                      <span className={`entry-icon entry-icon-${row.kind}`} aria-hidden="true" />
                      <span className="entry-name">{row.name}</span>
                      <span className="entry-kind">
                        {row.kind === 'file' && row.expectedSize !== undefined
                          ? formatBytes(row.expectedSize)
                          : 'Folder'}
                      </span>
                      {row.kind === 'directory' && (
                        <button
                          type="button"
                          disabled={active}
                          onClick={() => controller.openDirectory(row.id)}
                        >
                          Open
                        </button>
                      )}
                      {row.kind === 'file' && (
                        <button
                          className="preview-action"
                          type="button"
                          onClick={() => controller.previewFile(row.id)}
                        >
                          Preview
                        </button>
                      )}
                    </div>
                  </li>
                ))}
              </ul>
              <PreviewPanel preview={snapshot.preview} controller={controller} />
              {snapshot.pageCount > 1 && (
                <nav className="selection-pagination" aria-label="Directory pages">
                  <button
                    type="button"
                    disabled={snapshot.pageIndex === 0 || active}
                    onClick={() => controller.showPage(snapshot.pageIndex - 1)}
                  >Previous</button>
                  <span>Page {snapshot.pageIndex + 1} of {snapshot.pageCount}</span>
                  <button
                    type="button"
                    disabled={snapshot.pageIndex + 1 >= snapshot.pageCount || active}
                    onClick={() => controller.showPage(snapshot.pageIndex + 1)}
                  >Next</button>
                </nav>
              )}
            </section>

            <aside className="save-panel" aria-label="Output and transfer">
              <fieldset disabled={active || snapshot.phase !== 'browsing'}>
                <legend>Save to</legend>
                <label className="output-option">
                  <input
                    type="radio"
                    checked={snapshot.outputIntent === 'directory'}
                    disabled={!snapshot.outputCapabilities.nativeDirectory}
                    onChange={() => controller.chooseOutput('directory')}
                  />
                  <span><strong>Folder tree</strong><small>Durable checkpoints and restart recovery.</small></span>
                </label>
                <label className="output-option">
                  <input
                    type="radio"
                    checked={snapshot.outputIntent === 'download'}
                    disabled={
                      !snapshot.outputCapabilities.nativeSave &&
                      !snapshot.outputCapabilities.portableDownload
                    }
                    onChange={() => controller.chooseOutput('download')}
                  />
                  <span>
                    <strong>Browser download</strong>
                    <small>{downloadCapabilityDescription(snapshot.outputCapabilities)}</small>
                  </span>
                </label>
              </fieldset>

              {(active || snapshot.phase === 'completed' || snapshot.phase === 'completed-errors' ||
                snapshot.phase === 'aborted') && (
                <div className="progress-panel">
                  <strong>{formatBytes(snapshot.progress.writtenBytes)} received</strong>
                  <p>
                    {snapshot.progress.discoveryComplete
                      ? `${snapshot.progress.discoveredFiles} file(s), ${formatBytes(snapshot.progress.discoveredBytes)} total`
                      : `${snapshot.progress.discoveredFiles} file(s) discovered; total still unknown`}
                  </p>
                  <p>{snapshot.progress.contentLanes} authenticated content lane(s)</p>
                </div>
              )}

              {!active && snapshot.phase === 'browsing' && (
                <button
                  className="primary-action"
                  type="button"
                  disabled={!snapshot.canStart}
                  onClick={() => controller.startDownload()}
                >Receive selected</button>
              )}
              {active && snapshot.phase !== 'aborting' && (
                <button className="abort-action" type="button" onClick={() => controller.abortDownload()}>
                  Stop transfer
                </button>
              )}
            </aside>
          </div>
        )}
      </section>

      <footer>Secrets are removed from the address bar before any network or storage work.</footer>
    </main>
  )
}
