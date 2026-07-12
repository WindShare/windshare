import { useEffect, useRef, useSyncExternalStore, type FormEvent } from 'react'
import type { ReceiverController } from './controller'
import { SELECTION_PAGE_ROWS, type SelectionRow } from './model'

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB'] as const
const BYTES_PER_UNIT = 1024

function formatBytes(bytes: number): string {
  if (bytes === 0) {
    return '0 B'
  }
  let value = bytes
  let unit = 0
  while (value >= BYTES_PER_UNIT && unit < BYTE_UNITS.length - 1) {
    value /= BYTES_PER_UNIT
    unit += 1
  }
  const digits = value >= 10 || unit === 0 ? 0 : 1
  return `${value.toFixed(digits)} ${BYTE_UNITS[unit]}`
}

function SelectionCheckbox({
  row,
  disabled,
  onToggle,
}: {
  readonly row: SelectionRow
  readonly disabled: boolean
  readonly onToggle: () => void
}) {
  const input = useRef<HTMLInputElement>(null)
  useEffect(() => {
    if (input.current !== null) {
      input.current.indeterminate = row.partial
    }
  }, [row.partial])

  return (
    <label
      className="selection-row"
      style={{ '--entry-depth': row.indentLevel } as React.CSSProperties}
    >
      <input
        ref={input}
        type="checkbox"
        aria-label={row.accessibleLabel}
        checked={row.selected}
        disabled={disabled}
        onChange={onToggle}
      />
      <span className={`entry-icon entry-icon-${row.kind}`} aria-hidden="true" />
      <span className="entry-name">{row.name}</span>
      <span className="entry-kind">{row.kind === 'directory' ? 'Folder' : 'File'}</span>
    </label>
  )
}

function KeyForm({ controller }: { readonly controller: ReceiverController }) {
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const input = event.currentTarget.elements.namedItem('capability-key')
    if (!(input instanceof HTMLInputElement)) {
      return
    }
    // Keeping the field uncontrolled prevents the secret from entering React
    // snapshots; clear the only DOM copy before starting asynchronous work.
    const key = input.value
    input.value = ''
    controller.submitKey(key)
  }

  return (
    <form className="key-form" onSubmit={submit}>
      <label htmlFor="capability-key">Separate key</label>
      <p id="key-help">
        Paste the key, a key beginning with #, or the complete WindShare link.
      </p>
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

export function ReceiverApp({ controller }: { readonly controller: ReceiverController }) {
  const snapshot = useSyncExternalStore(
    controller.subscribe,
    controller.getSnapshot,
    controller.getSnapshot,
  )
  const selectionLocked = !(
    snapshot.phase === 'ready' || snapshot.phase === 'planning'
  )
  const active =
    snapshot.phase === 'preparing-output' ||
    snapshot.phase === 'transferring' ||
    snapshot.phase === 'reconnecting' ||
    snapshot.phase === 'aborting'
  const showProgress =
    active ||
    snapshot.phase === 'completed' ||
    snapshot.phase === 'aborted' ||
    (snapshot.phase === 'failed' && snapshot.progress.totalBlocks > 0)
  const progressMaximum = Math.max(snapshot.progress.totalBytes, 1)
  const firstVisibleEntry = snapshot.entries.length === 0
    ? 0
    : snapshot.selectionPageIndex * SELECTION_PAGE_ROWS + 1
  const lastVisibleEntry = snapshot.selectionPageIndex * SELECTION_PAGE_ROWS +
    snapshot.entries.length
  const status = useRef<HTMLParagraphElement>(null)
  const alert = useRef<HTMLDivElement>(null)
  const action = useRef<HTMLButtonElement>(null)
  const previousPhase = useRef(snapshot.phase)

  useEffect(() => {
    const previous = previousPhase.current
    previousPhase.current = snapshot.phase
    if (previous === snapshot.phase && snapshot.error === null) {
      return
    }
    if (
      (snapshot.phase === 'ready' && snapshot.error !== null) ||
      (snapshot.phase === 'transferring' &&
        (previous === 'preparing-output' || previous === 'ready'))
    ) {
      action.current?.focus()
      return
    }
    if (snapshot.phase === 'failed') {
      alert.current?.focus()
      return
    }
    if (
      snapshot.phase === 'aborting' ||
      snapshot.phase === 'aborted' ||
      snapshot.phase === 'completed'
    ) {
      status.current?.focus()
    }
  }, [snapshot.error, snapshot.phase])

  return (
    <main className="receiver-shell">
      <header className="brand-header">
        <a className="brand" href="/" aria-label="WindShare home">
          <span className="brand-mark" aria-hidden="true">W</span>
          <span>WindShare</span>
        </a>
        <span className="privacy-note">End-to-end encrypted</span>
      </header>

      <section className="receiver-card" aria-labelledby="receiver-title">
        <div className="card-heading">
          <p className="eyebrow">Receive securely</p>
          <h1 id="receiver-title">Save a shared download</h1>
          <p className="intro">
            The key stays in this browser. Nothing downloads until you choose a
            destination and press the download button.
          </p>
        </div>

        <p
          ref={status}
          className="status-line"
          role="status"
          aria-live="polite"
          tabIndex={-1}
        >
          <span className={`status-dot status-${snapshot.phase}`} aria-hidden="true" />
          {snapshot.status}
        </p>

        {snapshot.error !== null && (
          <div ref={alert} className="error-banner" role="alert" tabIndex={-1}>
            {snapshot.error}
          </div>
        )}

        {snapshot.phase === 'awaiting-key' && <KeyForm controller={controller} />}

        {snapshot.entries.length > 0 && (
          <div className="download-layout">
            <fieldset className="selection-panel" disabled={selectionLocked}>
              <legend>Files to download</legend>
              <p id="selection-summary" className="selection-summary">
                {snapshot.selectedEntryCount} selected · {formatBytes(snapshot.selectedBytes)} ·{' '}
                Showing {firstVisibleEntry}–{lastVisibleEntry} of {snapshot.manifestEntryCount}
              </p>
              <ul className="selection-list" aria-describedby="selection-summary">
                {snapshot.entries.map((row) => (
                  <li key={row.path}>
                    <SelectionCheckbox
                      row={row}
                      disabled={selectionLocked}
                      onToggle={() => controller.toggleSelection(row.path)}
                    />
                  </li>
                ))}
              </ul>
              {snapshot.selectionPageCount > 1 && (
                <nav className="selection-pagination" aria-label="File list pages">
                  <button
                    type="button"
                    disabled={snapshot.selectionPageIndex === 0}
                    onClick={() => controller.showSelectionPage(snapshot.selectionPageIndex - 1)}
                  >
                    Previous
                  </button>
                  <span aria-live="polite">
                    Page {snapshot.selectionPageIndex + 1} of {snapshot.selectionPageCount}
                  </span>
                  <button
                    type="button"
                    disabled={snapshot.selectionPageIndex + 1 === snapshot.selectionPageCount}
                    onClick={() => controller.showSelectionPage(snapshot.selectionPageIndex + 1)}
                  >
                    Next
                  </button>
                </nav>
              )}
            </fieldset>

            <aside className="save-panel" aria-label="Download settings">
              <fieldset
                disabled={
                  active ||
                  (snapshot.phase !== 'ready' && snapshot.phase !== 'planning')
                }
              >
                <legend>Save to</legend>
                <div className="output-options">
                  {snapshot.outputChoices.map((choice) => (
                    <label
                      className={`output-option ${choice.available ? '' : 'unavailable'}`}
                      key={choice.id}
                    >
                      <input
                        type="radio"
                        name="output-choice"
                        value={choice.id}
                        checked={snapshot.outputChoice === choice.id}
                        disabled={!choice.available || active}
                        onChange={() => controller.chooseOutput(choice.id)}
                      />
                      <span>
                        <strong>{choice.label}</strong>
                        <small>
                          {choice.available ? choice.description : 'Not available in this browser.'}
                        </small>
                      </span>
                    </label>
                  ))}
                </div>
              </fieldset>

              {showProgress && (
                <div className="progress-panel">
                  <div className="progress-heading">
                    <span>Progress</span>
                    <strong>
                      {formatBytes(snapshot.progress.writtenBytes)} /{' '}
                      {formatBytes(snapshot.progress.totalBytes)}
                    </strong>
                  </div>
                  <progress
                    aria-label="Download progress"
                    max={progressMaximum}
                    value={snapshot.progress.writtenBytes}
                  />
                  <dl className="transfer-details">
                    <div>
                      <dt>Blocks</dt>
                      <dd>
                        {snapshot.progress.completedBlocks} / {snapshot.progress.totalBlocks}
                      </dd>
                    </div>
                    <div>
                      <dt>Retries</dt>
                      <dd>{snapshot.progress.retryBlocks}</dd>
                    </div>
                    <div>
                      <dt>Connections</dt>
                      <dd>{snapshot.progress.channels}</dd>
                    </div>
                    <div>
                      <dt>Buffered</dt>
                      <dd>
                        {snapshot.progress.bufferedBlocks} /{' '}
                        {snapshot.progress.maxBufferedBlocks}
                      </dd>
                    </div>
                  </dl>
                  {snapshot.phase === 'reconnecting' && (
                    <p className="reconnect-message">
                      Reconnect attempt {snapshot.reconnectAttempt}
                    </p>
                  )}
                </div>
              )}

              {!active && snapshot.phase !== 'completed' && snapshot.phase !== 'aborted' && (
                <button
                  ref={action}
                  className="primary-action"
                  type="button"
                  disabled={!snapshot.canStart}
                  onClick={() => controller.startDownload()}
                >
                  Download selected
                </button>
              )}
              {active && snapshot.phase !== 'aborting' && (
                <button
                  ref={action}
                  className="abort-action"
                  type="button"
                  onClick={() => controller.abortDownload()}
                >
                  Stop download
                </button>
              )}
            </aside>
          </div>
        )}
      </section>

      <footer>
        Secrets are removed from the address bar before the share is contacted.
      </footer>
    </main>
  )
}
