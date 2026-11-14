import { useRef, useState, useSyncExternalStore, type FormEvent } from 'react'
import type { ReceiverController } from '../controller'
import { shareConnectionLabel } from '../experience/share-presentation'
import { ConnectionDetails } from '../experience/ConnectionDetails'
import { ConnectionRecovery } from '../connection/ConnectionRecovery'
import { connectedChannelCount } from '../connection/path-presentation'
import { DetailSheet } from '../controls/DetailSheet'
import { DiagnosticsEntry } from '../diagnostics/DiagnosticsEntry'
import { ReceiverIcon } from './ReceiverIcon'
import { ReceiverFold } from './ReceiverFold'

export function ReceiverHeading({ controller }: { readonly controller: ReceiverController }) {
  const source = controller.presentation.connection
  const browsing = controller.presentation.browsing
  const snapshot = useSyncExternalStore(source.subscribe, source.getSnapshot, source.getSnapshot)
  const { share, breadcrumbs } = useSyncExternalStore(browsing.subscribe, browsing.getSnapshot, browsing.getSnapshot)
  const [details, setDetails] = useState(false)
  const invoker = useRef<HTMLButtonElement>(null)
  const label = shareConnectionLabel(snapshot.connection, snapshot.phase, snapshot.status)
  const openDetails = (element: HTMLButtonElement) => {
    invoker.current = element
    controller.recordExperienceIntent('open-connection-details')
    setDetails(true)
  }
  const closeDetails = () => { controller.recordExperienceIntent('close-details'); setDetails(false) }
  return <>
    <div className="share-heading">
      <div className="share-heading-copy">
        <h1 title={share?.name}>{share?.name ?? (snapshot.phase === 'awaiting-key' ? 'Open your share' : 'Shared files')}</h1>
        <div className="share-metadata">
          <button className={`connection-status connection-${snapshot.connection.kind}`} type="button" onClick={event => openDetails(event.currentTarget)}>
            <ReceiverIcon name="connection" />{label}
            {snapshot.pathActivity.lanes.length > 0 && <span>· {connectedChannelCount(snapshot.pathActivity)}</span>}
          </button>
          <button className="encryption-note" type="button" onClick={event => openDetails(event.currentTarget)}>
            <ReceiverIcon name="lock" />Encrypted
          </button>
        </div>
      </div>
      <ReceiverFold compact={share?.kind === 'photo' || share?.kind === 'video'} />
    </div>
    {snapshot.error !== null && <div className="share-error" role="alert">{snapshot.error} <DiagnosticsEntry label="Export diagnostics" /></div>}
    {snapshot.phase === 'awaiting-key' && <KeyForm controller={controller} />}
    {share === null && breadcrumbs.length === 0 && snapshot.phase === 'joining' && <div className="share-loading">
      <p role="status">{snapshot.status}</p>
      {snapshot.connection.kind === 'idle' && snapshot.connection.join === 'waiting-for-choice' && <>
        <button type="button" onClick={() => controller.requestReconnect()}>Retry now</button>
        <button type="button" onClick={() => controller.continueJoinWaiting()}>Continue waiting</button>
      </>}
      <button type="button" onClick={() => controller.cancelJoin()}>Cancel</button>
    </div>}
    {snapshot.connection.kind === 'reconnecting' && <ConnectionRecovery activity={snapshot.connection.activity} retry={() => controller.requestReconnect()} />}
    {details && <DetailSheet title="Encryption and connection" onClose={closeDetails} returnFocus={invoker}>
      <ConnectionDetails status={label} path={snapshot.pathActivity} />
    </DetailSheet>}
  </>
}

function KeyForm({ controller }: { readonly controller: ReceiverController }) {
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const input = event.currentTarget.elements.namedItem('capability-key')
    if (!(input instanceof HTMLInputElement)) return
    const key = input.value
    input.value = ''
    controller.submitKey(key)
  }
  return <form className="key-form" onSubmit={submit}>
    <label htmlFor="capability-key">Separate key</label>
    <p id="key-help">Paste the key or the complete WindShare link to open this share.</p>
    <div className="key-entry"><input id="capability-key" name="capability-key" type="password"
      autoComplete="off" spellCheck={false} aria-describedby="key-help" required autoFocus />
      <button className="primary-action" type="submit">Open share</button></div>
  </form>
}
