import { useEffect, useRef, useSyncExternalStore, type FormEvent } from 'react'

import {
  activationLocksSelection,
  type PresentedArtifactChoice,
} from './v2-artifact-presentation'
import {
  presentCompatibleNameRepair,
  type CompatibleNameRepairPresentation,
  type LifecycleActionPresentation,
} from './v2-lifecycle-presentation'
import type {
  V2BrowseRow,
  V2PreviewSnapshot,
  V2RetainedReceiveInventorySnapshot,
} from './v2-model'
import type {
  V2RetainedReceiveAction,
  V2RetainedReceiveOperation,
} from './v2-receive-runtime'
import type { V2ReceiverController } from './v2-controller'
import type { V2OutputPresentationSnapshot } from './v2-output'
import {
  completionProgressDescription,
  discoveryProgressDescription,
  formatBytes,
} from './v2-progress-presentation'

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

function retentionTimestamp(expiresAt: number): Readonly<{
  label: string
  dateTime: string | null
}> {
  const date = new Date(expiresAt)
  if (Number.isNaN(date.getTime())) {
    return Object.freeze({ label: `${expiresAt} ms since Unix epoch`, dateTime: null })
  }
  return Object.freeze({ label: date.toLocaleString(), dateTime: date.toISOString() })
}

function retainedOperationCopy(operation: V2RetainedReceiveOperation): Readonly<{
  title: string
  description: string
}> {
  switch (operation.continuation) {
    case 'pending-catch-up':
      return Object.freeze({
        title: 'Compatible-name finalization needs catch-up',
        description: 'WindShare can finish the local sidecar and lifecycle without contacting the sender.',
      })
    case 'restoration-available':
      return Object.freeze({
        title: 'Compatible-name restoration is ready',
        description: 'The terminal sidecar was validated; the restoration command is ready to run.',
      })
    case 'resume-receive':
      return Object.freeze({
        title: 'Receive can continue',
        description: 'File checkpoints are retained for this task.',
      })
    case 'resume-package':
      return Object.freeze({
        title: 'Packaging can continue',
        description: 'The completed materialization is retained for packaging.',
      })
    case 'save-artifact':
      return Object.freeze({
        title: 'Ready to save',
        description: 'The packaged result is retained and waiting for a save location.',
      })
    case 'retry-download':
      return Object.freeze({
        title: 'Ready to download again',
        description: 'The retained package can start another browser download.',
      })
    case 'cleanup-expired':
      return Object.freeze({
        title: 'Expired task needs cleanup',
        description: 'Its retention deadline has ended; only owned storage cleanup is safe.',
      })
    case 'retry-cleanup':
      return Object.freeze({
        title: 'Cleanup needs another attempt',
        description: 'Publication finished, but owned temporary storage still needs cleanup.',
      })
    case 'needs-attention':
      return Object.freeze({
        title: 'Needs attention',
        description: 'WindShare cannot safely infer target ownership or cleanup completion.',
      })
  }
}

function retainedActionLabel(
  operation: V2RetainedReceiveOperation,
  action: V2RetainedReceiveAction,
): string {
  switch (action) {
    case 'catch-up':
      return 'Finish local restoration catch-up'
    case 'continue':
      return 'Continue'
    case 'save':
      return 'Save'
    case 'redownload':
      return 'Download again'
    case 'discard':
      return 'Discard task and delete retained content'
    case 'delete':
      if (operation.continuation === 'cleanup-expired') return 'Delete expired data'
      if (operation.continuation === 'retry-cleanup' ||
          (operation.lifecycle.kind === 'published' &&
           operation.lifecycle.cleanupState === 'cleanup-pending')) return 'Retry cleanup'
      return 'Delete retained result'
  }
}

function CompatibleNameRepairPanel(props: {
  readonly repair: CompatibleNameRepairPresentation | null
}) {
  const repair = props.repair
  if (repair === null) return null
  return (
    <section
      className={`compatible-name-repair compatible-name-repair-${repair.actionMode}`}
      aria-label="Compatible-name restoration"
      role="status"
    >
      <strong>{repair.noticeTitle}</strong>
      <p>{repair.noticeDescription}</p>
      <p>{repair.replacementCountLabel}</p>
      {repair.replacementCount === 0 && (
        <p>The count updates only after a folder is ownership-verified or a file commits.</p>
      )}
      {repair.logicalPathSample.length > 0 && (
        <>
          <p>Logical paths awaiting restoration:</p>
          <ul>
            {repair.logicalPathSample.map(path => <li key={path}><code>{path}</code></li>)}
          </ul>
        </>
      )}
      {repair.omittedLogicalPathCount > 0 && (
        <p>{repair.omittedLogicalPathCount} more replacement(s) are not shown.</p>
      )}
      <dl>
        <dt>Restoration script</dt>
        <dd><code>{repair.scriptName}</code></dd>
        <dt>Restoration sidecar</dt>
        <dd><code>{repair.sidecarName}</code></dd>
        <dt>Location</dt>
        <dd>{repair.placementLabel}</dd>
        {repair.runCommand !== null && (
          <>
            <dt>Run command</dt>
            <dd><code>{repair.runCommand}</code></dd>
          </>
        )}
      </dl>
      <strong>{repair.actionTitle}</strong>
      <p>{repair.actionDescription}</p>
    </section>
  )
}

function RetainedReceivePanel(props: {
  readonly inventory: V2RetainedReceiveInventorySnapshot
  readonly controller: V2ReceiverController
}) {
  if (props.inventory.kind === 'loading') return null
  if (props.inventory.kind === 'failed') {
    return (
      <section className="retained-receive-panel" aria-live="polite">
        <strong>Stored receive tasks are unavailable</strong>
        <p>{props.inventory.error}</p>
      </section>
    )
  }
  if (props.inventory.operations.length === 0) return null
  return (
    <section
      className="retained-receive-panel"
      aria-labelledby="retained-receive-title"
      aria-busy={props.inventory.pending !== null}
    >
      <strong id="retained-receive-title">Stored receive tasks</strong>
      <p>Actions reopen and verify the exact saved operation before changing owned data.</p>
      <ul className="retained-receive-list">
        {props.inventory.operations.map((operation) => {
          const copy = retainedOperationCopy(operation)
          const retention = operation.expiresAt === undefined
            ? null
            : retentionTimestamp(operation.expiresAt)
          const repair = operation.repairSummary === undefined
            ? null
            : presentCompatibleNameRepair({
                state: operation.lifecycle,
                summary: operation.repairSummary,
                context: operation.continuation === 'pending-catch-up' &&
                  operation.repairSummary.pendingCatchUp
                  ? 'pending-catch-up'
                  : 'receive-lifecycle',
              })
          return (
            <li key={operation.operationId} className="retained-receive-item">
              <strong>{copy.title}</strong>
              <p>{copy.description}</p>
              {retention !== null && (
                <small>
                  {operation.continuation === 'cleanup-expired' ? 'Retention ended at ' : 'Retained until '}
                  <time {...(retention.dateTime === null ? {} : { dateTime: retention.dateTime })}>
                    {retention.label}
                  </time>
                </small>
              )}
              {operation.unavailableReason !== undefined && <p>{operation.unavailableReason}</p>}
              <CompatibleNameRepairPanel repair={repair} />
              {operation.actions.length > 0 && (
                <div className="lifecycle-actions">
                  {operation.actions.map((action) => (
                    <button
                      key={action}
                      className={action === 'discard' || action === 'delete'
                        ? 'abort-action'
                        : undefined}
                      type="button"
                      disabled={props.inventory.pending !== null}
                      onClick={() => props.controller.performRetainedAction(operation, action)}
                    >
                      {retainedActionLabel(operation, action)}
                    </button>
                  ))}
                </div>
              )}
            </li>
          )
        })}
      </ul>
    </section>
  )
}

function VideoPreview(props: {
  readonly preview: Extract<V2PreviewSnapshot, { state: 'video' }>
  readonly controller: V2ReceiverController
}) {
  const video = useRef<HTMLVideoElement>(null)
  const position = () => {
    if (video.current !== null) video.current.currentTime = props.preview.positionSeconds
  }
  const presented = () =>
    props.controller.previewMediaPresented(props.preview.presentationId)
  return (
    <>
      <video
        key={props.preview.presentationId}
        ref={video}
        className="preview-media"
        src={props.preview.url}
        aria-label={`Video preview of ${props.preview.name}`}
        muted
        playsInline
        preload="auto"
        onLoadedMetadata={position}
        onLoadedData={() => {
          position()
          presented()
        }}
        onSeeked={presented}
        onError={() => props.controller.previewMediaFailed(props.preview.presentationId)}
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

function PreviewPanel(props: {
  readonly preview: V2PreviewSnapshot
  readonly controller: V2ReceiverController
}) {
  if (props.preview.state === 'idle') return null
  const imagePreview = props.preview.state === 'image' ? props.preview : null
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
      {imagePreview !== null && (
        <img
          className="preview-media"
          src={imagePreview.url}
          alt={`Preview of ${imagePreview.name}`}
          onLoad={() => props.controller.previewMediaPresented(imagePreview.presentationId)}
          onError={() => props.controller.previewMediaFailed(imagePreview.presentationId)}
        />
      )}
      {props.preview.state === 'video' && (
        <VideoPreview preview={props.preview} controller={props.controller} />
      )}
    </section>
  )
}

function ArtifactChoiceButton(props: {
  readonly presented: PresentedArtifactChoice
  readonly controller: V2ReceiverController
  readonly disabled: boolean
}) {
  return (
    <li className="artifact-action-card">
      <button
        className={props.presented.importance === 'primary' ? 'primary-action' : undefined}
        type="button"
        disabled={props.disabled}
        onClick={() => props.controller.chooseArtifact(props.presented.operation)}
      >{props.presented.label}</button>
      <p>{props.presented.description}</p>
      {props.presented.packageExplanation !== null && (
        <small>{props.presented.packageExplanation}</small>
      )}
    </li>
  )
}

function ArtifactOfferPanel(props: {
  readonly output: V2OutputPresentationSnapshot
  readonly controller: V2ReceiverController
  readonly disabled: boolean
}) {
  const activation = props.output.activationPresentation
  if (activation !== null) {
    return (
      <div className="output-guidance">
        <ul className="artifact-action-list" aria-label="Selected result">
          <ArtifactChoiceButton
            presented={activation.choice}
            controller={props.controller}
            disabled
          />
        </ul>
        <div role="status">
          <strong>{activation.title}</strong>
          <p>{activation.description}</p>
        </div>
        {activation.kind === 'retry' && (
          <button
            type="button"
            disabled={props.disabled}
            onClick={() => props.controller.retryOutputConfirmation()}
          >
            {activation.label}
          </button>
        )}
      </div>
    )
  }
  const presentation = props.output.offerPresentation
  if (presentation === null) {
    return props.output.projection === null
      ? <p className="output-guidance">Select content to see the available results.</p>
      : <p className="output-guidance" role="status">Updating available results…</p>
  }
  if (presentation.kind === 'status') {
    return (
      <div className="output-guidance" role="status">
        <strong>{presentation.title}</strong>
        <p>{presentation.description}</p>
      </div>
    )
  }
  if (presentation.kind === 'retry') {
    return (
      <div className="output-guidance">
        <strong>{presentation.title}</strong>
        <p>{presentation.description}</p>
        <button
          type="button"
          disabled={props.disabled}
          onClick={() => props.controller.retryOutputConfirmation()}
        >
          {presentation.label}
        </button>
      </div>
    )
  }
  return (
    <ul className="artifact-action-list" aria-label="Choose the result to receive">
      <ArtifactChoiceButton
        presented={presentation.primary}
        controller={props.controller}
        disabled={props.disabled}
      />
      {presentation.alternatives.map((alternative) => (
        <ArtifactChoiceButton
          key={`${alternative.operation}:${alternative.choice.artifactKind}`}
          presented={alternative}
          controller={props.controller}
          disabled={props.disabled}
        />
      ))}
    </ul>
  )
}

function LifecycleActionButton(props: {
  readonly action: LifecycleActionPresentation
  readonly controller: V2ReceiverController
}) {
  return (
    <button
      className={props.action.destructive ? 'abort-action' : undefined}
      type="button"
      onClick={() => props.controller.performLifecycleAction(props.action.kind)}
    >{props.action.label}</button>
  )
}

function LifecyclePanel(props: {
  readonly output: V2OutputPresentationSnapshot
  readonly controller: V2ReceiverController
}) {
  const presentation = props.output.lifecyclePresentation
  if (presentation === null) return null
  const retention = presentation.retention
  const timestamp = retention === null
    ? null
    : retentionTimestamp(retention.expiresAt)
  return (
    <section className={`lifecycle-panel lifecycle-${presentation.tone}`} aria-live="polite">
      <strong>{presentation.title}</strong>
      <p>{presentation.description}</p>
      {retention !== null && timestamp !== null && (
        <p>
          {retention.elapsed ? 'Retention ended at ' : 'Available until '}
          <time {...(timestamp.dateTime === null ? {} : { dateTime: timestamp.dateTime })}>
            {timestamp.label}
          </time>
        </p>
      )}
      {presentation.usage !== null && <p>{presentation.usage.label}</p>}
      {presentation.actions.length > 0 && (
        <div className="lifecycle-actions">
          {presentation.actions.map((action) => (
            <LifecycleActionButton key={action.kind} action={action} controller={props.controller} />
          ))}
        </div>
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
  const retainedActionPending = snapshot.retained.pending !== null
  const receiveLocked = retainedActionPending ||
    activationLocksSelection(snapshot.output.activation) ||
    snapshot.output.receiveIntent !== null
  const selectionLocked = receiveLocked || snapshot.phase !== 'browsing'
  const alert = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (snapshot.phase === 'failed') alert.current?.focus()
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
            Directory pages are authenticated on demand. Content opens only after an explicit
            preview or artifact action.
          </p>
        </div>

        <p className="status-line" role="status" aria-live="polite">
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

        <RetainedReceivePanel inventory={snapshot.retained} controller={controller} />

        {snapshot.phase === 'awaiting-key' && <KeyForm controller={controller} />}

        {snapshot.breadcrumbs.length > 0 && (
          <div className="download-layout">
            <section className="selection-panel" aria-label="Shared files">
              <nav className="selection-pagination" aria-label="Current directory">
                {snapshot.breadcrumbs.map((crumb, index) => (
                  <button
                    type="button"
                    key={crumb.id}
                    disabled={index === snapshot.breadcrumbs.length - 1 || receiveLocked}
                    onClick={() => controller.openBreadcrumb(index)}
                  >{crumb.name}</button>
                ))}
              </nav>
              <p className="selection-summary">
                {snapshot.selectedVisibleFiles} selected file(s) on this page · {formatBytes(snapshot.selectedVisibleBytes)}
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
                          disabled={receiveLocked}
                          onClick={() => controller.openDirectory(row.id)}
                        >Open</button>
                      )}
                      {row.kind === 'file' && (
                        <button
                          className="preview-action"
                          type="button"
                          onClick={() => controller.previewFile(row.id)}
                        >Preview</button>
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
                    disabled={snapshot.pageIndex === 0 || receiveLocked}
                    onClick={() => controller.showPage(snapshot.pageIndex - 1)}
                  >Previous</button>
                  <span>Page {snapshot.pageIndex + 1} of {snapshot.pageCount}</span>
                  <button
                    type="button"
                    disabled={snapshot.pageIndex + 1 >= snapshot.pageCount || receiveLocked}
                    onClick={() => controller.showPage(snapshot.pageIndex + 1)}
                  >Next</button>
                </nav>
              )}
            </section>

            <aside className="save-panel" aria-label="Result and receive status">
              <h2>Receive as</h2>
              <ArtifactOfferPanel
                output={snapshot.output}
                controller={controller}
                disabled={retainedActionPending}
              />
              <LifecyclePanel output={snapshot.output} controller={controller} />
              <CompatibleNameRepairPanel
                repair={snapshot.output.lifecyclePresentation?.compatibleNameRepair ?? null}
              />
              {snapshot.output.transferResultPresentation !== null && (
                <div className={`transfer-result transfer-result-${snapshot.output.transferResultPresentation.tone}`}>
                  <strong>{snapshot.output.transferResultPresentation.title}</strong>
                  {snapshot.output.transferResultPresentation.lines.map((line) => (
                    <p key={line}>{line}</p>
                  ))}
                </div>
              )}
              {snapshot.progress.transferJobId.length > 0 && (
                <div className="progress-panel">
                  <strong>{completionProgressDescription(snapshot.progress)}</strong>
                  <p>{discoveryProgressDescription(snapshot.progress)}</p>
                  {snapshot.progress.fileErrors > 0 && snapshot.output.transferResultPresentation === null && (
                    <p>{snapshot.progress.fileErrors} file error(s)</p>
                  )}
                  {snapshot.progress.selectionErrors > 0 && (
                    <p>{snapshot.progress.selectionErrors} selected target(s) unavailable</p>
                  )}
                </div>
              )}
            </aside>
          </div>
        )}
      </section>

      <footer>Secrets are removed from the address bar before any network or storage work.</footer>
    </main>
  )
}
