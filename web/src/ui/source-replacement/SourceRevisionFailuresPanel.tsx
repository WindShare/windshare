import type { SourceRevisionFailure } from '../../output/resume/source-revision-failures'
import type { V2RetainedReceiveOperation } from '../v2-receive-runtime'

export function SourceRevisionFailuresPanel(props: {
  readonly operation: V2RetainedReceiveOperation
  readonly busy: boolean
  readonly prepareReplacement: (failure: SourceRevisionFailure) => void
}) {
  const failures = props.operation.sourceRevisionFailures
  if (failures === undefined) return null
  const hidden = failures.count - BigInt(failures.files.length)
  return <section aria-label="Files requiring a new download">
    <p>{failures.count.toString()} {failures.count === 1n ? 'file stopped' : 'files stopped'} because the original source version could not be used.
      The original ZIP and its other received files are retained. Continue retries only the original versions.</p>
    <p>Download a current version as a separate task. Choose its download option after selecting it below.</p>
    <ul>{failures.files.map(failure => <li key={failure.entryId}>
      <span>{failure.path.join('/')}</span>{' '}
      <button type="button" disabled={props.busy} onClick={() => props.prepareReplacement(failure)}>
        Download current version of {failure.path.join('/')}
      </button>
    </li>)}</ul>
    {hidden > 0n && <p>{hidden.toString()} more affected files. Browse the share to download their current versions separately.</p>}
  </section>
}
