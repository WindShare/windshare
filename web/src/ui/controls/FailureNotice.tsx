import { receiverCopy } from '../locales/receiver'
import { usePreferences } from '../preferences/use-preferences'
import type { ReceiverFailure } from '../model'
import { presentSaveUnavailable } from '../saving'

export function FailureNotice({ failure, context }: {
  readonly failure: ReceiverFailure
  readonly context: 'open' | 'action' | 'directory' | 'preview' | 'frame'
}) {
  const { language } = usePreferences()
  const t = receiverCopy[language].failures
  if (typeof failure !== 'string' && failure.kind === 'blocked') return <p>{receiverCopy[language].admission[failure.reason]}</p>
  if (typeof failure !== 'string' && failure.kind === 'save-unavailable') {
    const { description, desktopGuidance } = presentSaveUnavailable(failure.reason, receiverCopy[language].saving)
    return <div><p>{description}</p>{desktopGuidance !== null && <p>{desktopGuidance}</p>}</div>
  }
  if (typeof failure !== 'string' && failure.kind === 'source-changed') return <p>{t.sourceChangedHint}</p>
  if (typeof failure !== 'string' && failure.kind === 'restore-target-missing') {
    return <p>{t.restoreTargetMissing(failure.directoryLabel)}</p>
  }
  const message = t[typeof failure === 'string' ? context : failure.kind]
  const detail = typeof failure === 'string' ? failure : failure.detail
  return <div><p>{message}</p><details><summary>{t.technical}</summary>{detail}</details></div>
}
