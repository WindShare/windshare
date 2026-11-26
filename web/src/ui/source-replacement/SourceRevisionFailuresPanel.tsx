import { useEffect, useRef, useState } from 'react'
import { hasRetainedReplacementOutput } from '../../application/receive/admission'
import type {
  ReceiveAdmission,
  ReceiveBlockReason,
  RetainedTaskSnapshot,
} from '../../application/receive/contracts'
import type { SourceRevisionFailure } from '../../transfer/state'
import type { ReplacementActionCallbacks } from '../controller/failures'
import { FailureNotice } from '../controls/FailureNotice'
import { receiverCopy } from '../locales/receiver'
import type { ReceiverFailure } from '../model'
import { useCopy } from '../preferences/use-preferences'

type SummaryReason = 'source-busy' | 'source-changed' | 'mixed'

interface PendingLookup {
  readonly taskId: string
  readonly path: string
  readonly nonce: number
}

interface LocalReplacementFailure {
  readonly taskId: string
  readonly failure: ReceiverFailure
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

function blockedReasonCopy(reason: ReceiveBlockReason, copy: typeof receiverCopy.en): string {
  return reason === 'matchingShare'
    ? copy.result.openOriginalShareForReplacement
    : copy.admission[reason]
}

export function SourceRevisionFailuresPanel(props: {
  readonly task: RetainedTaskSnapshot
  readonly admission: ReceiveAdmission
  readonly prepareReplacement: (
    failure: SourceRevisionFailure,
    callbacks?: ReplacementActionCallbacks,
  ) => void
  readonly onPrepared?: () => void
}) {
  const copy = useCopy(receiverCopy)
  const t = copy.result
  const [localFailure, setLocalFailure] = useState<LocalReplacementFailure | null>(null)
  const [pending, setPending] = useState<PendingLookup | null>(null)
  const mountedRef = useRef(true)
  const visibleTaskRef = useRef<string | null>(props.task.taskId)
  const requestNonceRef = useRef(0)

  useEffect(() => {
    mountedRef.current = true
    return () => { mountedRef.current = false }
  }, [])

  useEffect(() => {
    visibleTaskRef.current = props.task.taskId
    return () => { visibleTaskRef.current = null }
  }, [props.task.taskId])

  const failures = props.task.sourceRevisionFailures
  // Replacement downloads only add value when the original task kept partial files or progress;
  // zero-output failed tasks can simply be downloaded again directly.
  if (failures === undefined || !hasRetainedReplacementOutput(props.task)) return null

  const reason = summarizeReasons(failures.files)
  const hidden = failures.count - BigInt(failures.files.length)
  const blockedReason = props.admission.allowed
    ? null
    : blockedReasonCopy(props.admission.reason ?? 'actionUnavailable', copy)
  const activeFailure = localFailure?.taskId === props.task.taskId ? localFailure.failure : null
  const activePending = pending?.taskId === props.task.taskId ? pending : null
  const disabled = !props.admission.allowed || activePending !== null

  const handleSelect = (failure: SourceRevisionFailure) => {
    if (disabled) return
    const taskId = props.task.taskId
    const nonce = ++requestNonceRef.current
    setLocalFailure(null)
    setPending({ taskId, path: failure.path.join('/'), nonce })
    props.prepareReplacement(failure, {
      onPrepared: () => {
        if (visibleTaskRef.current === taskId) props.onPrepared?.()
      },
      onAsyncFailure: mapped => {
        if (visibleTaskRef.current !== taskId) return false
        setLocalFailure({ taskId, failure: mapped })
        return true
      },
      onSettled: () => {
        if (mountedRef.current) {
          setPending(current => (current?.nonce === nonce ? null : current))
        }
      },
    })
  }

  return <section className="source-replacement-panel" aria-label={t.replacementFiles}>
    <p>{summaryLine(reason, failures.count, props.task, t)}</p>
    <p>{reason === 'source-busy' ? t.busyReplacementHint : t.replacementHint}</p>
    {blockedReason !== null && <p className="action-reason" role="status">{blockedReason}</p>}
    {blockedReason === null && activePending !== null && (
      <p className="action-reason" role="status">{copy.connection.findingReplacement(activePending.path)}</p>
    )}
    {activeFailure !== null && (
      <div className="task-action-error" role="alert">
        <FailureNotice failure={activeFailure} context="action" />
      </div>
    )}
    <ul>{failures.files.map(failure => {
      const path = failure.path.join('/')
      return <li key={failure.entryId}>
        <span>{path}</span>{' '}
        <button
          type="button"
          disabled={disabled}
          title={blockedReason ?? undefined}
          onClick={() => handleSelect(failure)}
        >
          {failure.reason === 'source-changed' ? t.downloadCurrent(path) : t.retryFile(path)}
        </button>
      </li>
    })}</ul>
    {hidden > 0n && <p>{t.moreSources(hidden)}</p>}
  </section>
}
