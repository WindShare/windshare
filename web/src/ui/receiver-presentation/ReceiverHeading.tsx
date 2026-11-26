import { FailureNotice } from '../controls/FailureNotice'
import { shellCopy } from '../locales/shell'
import { receiverCopy } from '../locales/receiver'
import { useCopy, usePreferences } from '../preferences/use-preferences'
import { useRef, useState, useSyncExternalStore, type FormEvent, type ReactNode } from 'react'
import type { InitialJoinState } from '../../receiver/initial-join'
import type { ReceiverController } from '../controller'
import { initialJoinStatus, receiverStatus, shareConnectionLabel } from '../experience/share-presentation'
import { ConnectionDetails } from '../experience/ConnectionDetails'
import { ConnectionRecovery } from '../connection/ConnectionRecovery'
import { useCountdownSeconds } from '../connection/countdown'
import { connectedChannelCount } from '../connection/path-presentation'
import { DetailSheet } from '../controls/DetailSheet'
import { DiagnosticsEntry } from '../diagnostics/DiagnosticsEntry'
import { ReceiverIcon } from './ReceiverIcon'

export function ReceiverHeading({ controller, actions }: {
  readonly controller: ReceiverController
  readonly actions?: ReactNode
}) {
  const { language } = usePreferences()
  const shell = shellCopy[language]
  const t = receiverCopy[language].connection
  const source = controller.presentation.connection
  const browsing = controller.presentation.browsing
  const snapshot = useSyncExternalStore(source.subscribe, source.getSnapshot, source.getSnapshot)
  const { share } = useSyncExternalStore(browsing.subscribe, browsing.getSnapshot, browsing.getSnapshot)
  const [details, setDetails] = useState(false)
  const invoker = useRef<HTMLButtonElement>(null)
  const label = shareConnectionLabel(snapshot.connection, snapshot.phase, snapshot.status, t)
  const openDetails = (element: HTMLButtonElement) => {
    invoker.current = element
    controller.recordExperienceIntent('open-connection-details')
    setDetails(true)
  }
  const closeDetails = () => { controller.recordExperienceIntent('close-details'); setDetails(false) }
  const join = snapshot.connection.kind === 'idle' ? snapshot.connection.join : undefined
  const joinRetryAt = join?.kind === 'paused' || join?.kind === 'waiting' ? join.retryAt : undefined
  const title = share?.name ?? (snapshot.phase === 'awaiting-key' ? shell.openYourShare : shell.sharedFiles)
  const replacementNotice = share !== null && (
    snapshot.status === 'replacement-selected' ||
    (typeof snapshot.status === 'object' && snapshot.status !== null)
  )
    ? receiverStatus(snapshot.status, t)
    : null
  return <>
    <div className="share-hero">
      <div className="share-heading">
        <h1 title={title}>{title}</h1>
        <div className="share-metadata">
          <button className={`connection-status connection-${snapshot.connection.kind}`} type="button" onClick={event => openDetails(event.currentTarget)}>
            <span className="connection-dot" aria-hidden="true" />
            <span>{label}</span>
            {snapshot.pathActivity.lanes.length > 0 && <span className="connection-channels">· {connectedChannelCount(snapshot.pathActivity, t)}</span>}
          </button>
          <button className="encryption-note" type="button" onClick={event => openDetails(event.currentTarget)}>
            <ReceiverIcon name="lock" />{t.encrypted}
          </button>
        </div>
      </div>
      {actions}
    </div>
    {replacementNotice !== null && <p className="share-status-notice" role="status">{replacementNotice}</p>}
    {snapshot.error !== null && <div className="share-error" role="alert"><FailureNotice failure={snapshot.error} context="action" /><DiagnosticsEntry label={t.exportDiagnostics} /></div>}
    {snapshot.phase === 'awaiting-key' && <KeyForm controller={controller} />}
    {share === null && snapshot.phase === 'joining' &&
      <InitialJoinLoading key={joinRetryAt ?? 'none'} status={join ? initialJoinStatus(join, t) : receiverStatus(snapshot.status, t)} join={join} controller={controller} />}
    {snapshot.connection.kind === 'reconnecting' && <ConnectionRecovery activity={snapshot.connection.activity} retry={() => controller.requestReconnect()} />}
    {details && <DetailSheet title={t.encryptionAndConnection} onClose={closeDetails} returnFocus={invoker}>
      <ConnectionDetails status={label} path={snapshot.pathActivity} />
    </DetailSheet>}
  </>
}

function InitialJoinLoading({ status, join, controller }: {
  readonly status: string
  readonly join: InitialJoinState | undefined
  readonly controller: ReceiverController
}) {
  const t = useCopy(receiverCopy).connection
  const retryAt = join?.kind === 'paused' || join?.kind === 'waiting' ? join.retryAt : undefined
  const seconds = useCountdownSeconds(retryAt)
  const coolingDown = join?.kind === 'paused' && seconds > 0
  let countdown: string | null = null
  if (join?.kind === 'waiting' && retryAt !== undefined) {
    countdown = seconds > 0 ? t.retryIn(seconds) : t.retryShortly
  } else if (coolingDown) {
    countdown = t.retryAvailableIn(seconds)
  }
  return <div className="share-loading">
    <p role="status">{status}</p>
    {countdown !== null && <p className="connection-recovery-countdown" role="timer" aria-live="off">{countdown}</p>}
    <div className="share-loading-actions">
      {join?.kind === 'paused' && <>
        <button type="button" className="primary-action" disabled={coolingDown}
          onClick={() => controller.requestReconnect()}>{coolingDown ? t.pleaseWait : t.retryNow}</button>
        <button type="button" onClick={() => controller.continueJoinWaiting()}>{t.continueWaiting}</button>
      </>}
      <button type="button" onClick={() => controller.cancelJoin()}>{t.cancel}</button>
    </div>
  </div>
}

function KeyForm({ controller }: { readonly controller: ReceiverController }) {
  const t = useCopy(receiverCopy).connection
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const input = event.currentTarget.elements.namedItem('capability-key')
    if (!(input instanceof HTMLInputElement)) return
    const key = input.value
    input.value = ''
    controller.submitKey(key)
  }
  return <form className="key-form" onSubmit={submit}>
    <label htmlFor="capability-key">{t.shareKey}</label>
    <p id="key-help">{t.pasteKey}</p>
    <div className="key-entry">
      <input id="capability-key" name="capability-key" type="password"
        autoComplete="off" spellCheck={false} aria-describedby="key-help" required autoFocus />
      <button className="primary-action" type="submit">{t.openShare}</button>
    </div>
  </form>
}
