import { receiverCopy } from '../locales/receiver'
import { useCopy } from '../preferences/use-preferences'
import type { SourceRevisionFailure } from '../../transfer/state'
import type { RetainedTaskSnapshot } from '../../application/receive/contracts'

type SummaryReason = 'source-busy' | 'source-changed' | 'mixed'

function hasRetainedOutput(task: RetainedTaskSnapshot): boolean {
  if (task.lifecycle.kind === 'partial') return true
  return task.lifecycle.kind === 'paused' &&
    (task.lifecycle.completedFiles > 0n || task.lifecycle.retainedBytes > 0n)
}

function summarizeReasons(files: readonly SourceRevisionFailure[]): SummaryReason {
  if (files.length === 0) return 'mixed'
  if (files.every(file => file.reason === 'source-busy')) return 'source-busy'
  if (files.every(file => file.reason === 'source-changed')) return 'source-changed'
  return 'mixed'
}

function summaryLine(
  reason: SummaryReason,
  count: bigint,
  task: RetainedTaskSnapshot,
  t: typeof receiverCopy.en.result,
): string {
  let cause = t.unavailableSources(count)
  if (reason === 'source-busy') cause = t.busySources(count)
  else if (reason === 'source-changed') cause = t.staleSources(count)
  const paused = task.lifecycle.kind === 'paused'
  let retained = ''
  if (task.backend === 'opfs') retained = paused ? t.retainedPausedZip : t.retainedPartialZip
  else if (task.backend === 'fsa') retained = paused ? t.retainedPausedFolder : t.retainedPartialFolder
  return retained.length === 0 ? cause : t.combineSummary(cause, retained)
}

export function SourceRevisionFailuresPanel(props: {
  readonly task: RetainedTaskSnapshot
  readonly busy: boolean
  readonly prepareReplacement: (failure: SourceRevisionFailure) => void
}) {
  const t = useCopy(receiverCopy).result
  const failures = props.task.sourceRevisionFailures
  // Replacement downloads only add value when the original task kept partial files or progress;
  // zero-output failed tasks can simply be downloaded again directly.
  if (failures === undefined || !hasRetainedOutput(props.task)) return null
  const reason = summarizeReasons(failures.files)
  const hidden = failures.count - BigInt(failures.files.length)
  return <section aria-label={t.replacementFiles}>
    <p>{summaryLine(reason, failures.count, props.task, t)}</p>
    <p>{reason === 'source-busy' ? t.busyReplacementHint : t.replacementHint}</p>
    <ul>{failures.files.map(failure => {
      const path = failure.path.join('/')
      return <li key={failure.entryId}>
        <span>{path}</span>{' '}
        <button type="button" disabled={props.busy} onClick={() => props.prepareReplacement(failure)}>
          {failure.reason === 'source-changed' ? t.downloadCurrent(path) : t.retryFile(path)}
        </button>
      </li>
    })}</ul>
    {hidden > 0n && <p>{t.moreSources(hidden)}</p>}
  </section>
}
