import { receiverCopy } from '../locales/receiver'
import { usePreferences } from '../preferences/use-preferences'
import type { ReceiverFailure } from '../model'
import type { SaveUnavailableReason } from '../../application/receive/contracts'

function saveUnavailableReasonCopy(
  reason: SaveUnavailableReason,
  saving: typeof receiverCopy.en.saving,
): string {
  if (reason === 'storage-full') return saving.storageFull
  if (reason === 'portable-limit-exceeded') return saving.tooLarge
  return saving.cannotSave
}

export function FailureNotice({ failure, context }: {
  readonly failure: ReceiverFailure
  readonly context: 'open' | 'action' | 'directory' | 'preview' | 'frame'
}) {
  const { language } = usePreferences()
  const t = receiverCopy[language].failures
  if (typeof failure !== 'string' && failure.kind === 'blocked') return <p>{receiverCopy[language].admission[failure.reason]}</p>
  if (typeof failure !== 'string' && failure.kind === 'save-unavailable') {
    const saving = receiverCopy[language].saving
    return <div><p>{saveUnavailableReasonCopy(failure.reason, saving)}</p><p>{saving.desktopGuidance}</p></div>
  }
  if (typeof failure !== 'string' && failure.kind === 'source-changed') return <p>{t.sourceChangedHint}</p>
  const message = t[typeof failure === 'string' ? context : failure.kind]
  const detail = typeof failure === 'string' ? failure : failure.detail
  return <div><p>{message}</p><details><summary>{t.technical}</summary>{detail}</details></div>
}
