import { receiverCopy } from '../locales/receiver'
import { useCopy } from '../preferences/use-preferences'
import type { SourceRevisionFailure } from '../../transfer/state'
import type { RetainedTaskSnapshot } from '../../application/receive/contracts'

export function SourceRevisionFailuresPanel(props: {
  readonly task: RetainedTaskSnapshot
  readonly busy: boolean
  readonly prepareReplacement: (failure: SourceRevisionFailure) => void
}) {
  const t = useCopy(receiverCopy).result
  const failures = props.task.sourceRevisionFailures
  if (failures === undefined) return null
  const hidden = failures.count - BigInt(failures.files.length)
  return <section aria-label={t.replacementFiles}>
    <p>{t.staleSources(failures.count)}</p>
    <p>{t.replacementHint}</p>
    <ul>{failures.files.map(failure => <li key={failure.entryId}>
      <span>{failure.path.join('/')}</span>{' '}
      <button type="button" disabled={props.busy} onClick={() => props.prepareReplacement(failure)}>
        {t.downloadCurrent(failure.path.join('/'))}
      </button>
    </li>)}</ul>
    {hidden > 0n && <p>{t.moreSources(hidden)}</p>}
  </section>
}
