import type { DirectZipMilestonePayloadV1 } from '../../diagnostics/trace/model'
import type { DomainTraceSource } from '../../diagnostics/trace/ports'

export const OUTPUT_DIAGNOSTIC_BACKENDS = Object.freeze([
  'file_system_access',
  'origin_private',
  'portable',
] as const)

export type OutputDiagnosticBackend = (typeof OUTPUT_DIAGNOSTIC_BACKENDS)[number]

export interface OutputTracePayloadByName {
  readonly output_reservation: Readonly<{
    backend: OutputDiagnosticBackend
    transition: 'started' | 'acquired' | 'reopened' | 'failed'
  }>
  readonly output_write: Readonly<{
    backend: OutputDiagnosticBackend
    transition:
      | 'transaction_started'
      | 'transaction_failed'
      | 'transaction_committed'
      | 'commit_failed'
  }>
  readonly checkpoint: Readonly<{
    backend: Exclude<OutputDiagnosticBackend, 'portable'>
    transition: 'restored' | 'persisted' | 'quarantined' | 'failed'
    decision?: 'absent' | 'installed' | 'exact' | 'revision_conflict' | 'ownership_conflict' | 'invalid'
  }>
  readonly settlement: Readonly<{
    backend: OutputDiagnosticBackend
    transition: 'started' | 'completed' | 'failed' | 'ownership_unknown'
    outcome?: 'published' | 'partial_directory' | 'resumable_receive' | 'discarded' | 'needs_attention'
  }>
  readonly publication: Readonly<{
    backend: OutputDiagnosticBackend
    transition: 'started' | 'committed' | 'not_committed' | 'unknown'
  }>
  readonly continuation: Readonly<{
    backend: OutputDiagnosticBackend
    transition: 'paused' | 'resumed' | 'admission_failed'
  }>
  readonly reopen: Readonly<{
    backend: Exclude<OutputDiagnosticBackend, 'portable'>
    transition: 'started' | 'authorized' | 'failed'
  }>
  readonly cleanup: Readonly<{
    backend: OutputDiagnosticBackend
    transition: 'started' | 'completed' | 'retryable_failure' | 'ownership_unknown' | 'failed'
  }>
  readonly direct_zip_milestone: DirectZipMilestonePayloadV1
}

export type OutputTraceEventName = keyof OutputTracePayloadByName

export type OutputTraceEvent<Name extends OutputTraceEventName = OutputTraceEventName> =
  Name extends OutputTraceEventName
    ? Readonly<{
        eventName: Name
        payload: OutputTracePayloadByName[Name]
      }>
    : never

export type OutputTraceSource = DomainTraceSource<OutputTraceEvent>

export function outputTraceEvent<Name extends OutputTraceEventName>(
  eventName: Name,
  payload: OutputTracePayloadByName[Name],
): OutputTraceEvent<Name> {
  return Object.freeze({
    eventName,
    payload: Object.freeze({ ...payload }),
  }) as OutputTraceEvent<Name>
}

/**
 * The factory boundary is deliberate: disabled tracing does not construct a
 * payload, and a diagnostic observer can never acquire output authority.
 */
export function emitOutputTrace(
  source: OutputTraceSource | undefined,
  createEvent: () => OutputTraceEvent,
): void {
  const observer = source?.current
  if (observer === undefined) return
  try {
    observer(createEvent())
  } catch {
    // Output ownership and recovery remain authoritative when diagnostics fail.
  }
}
