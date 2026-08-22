import type { DirectZipMilestonePayloadV1 } from '../../../diagnostics/trace/model'
import { projectOutputException } from '../../diagnostics/exception'
import { snapshotIdentity } from '../../workspace/canonical'
import {
  DIRECT_ZIP_CHECKPOINT_PHASES,
  DIRECT_ZIP_CLEANUP_DECISIONS,
  DIRECT_ZIP_DIAGNOSTIC_MILESTONES,
  DIRECT_ZIP_DIAGNOSTIC_PLAN_KIND,
  DIRECT_ZIP_EPOCH_OFFSET_CLASSES,
  DIRECT_ZIP_IDENTITY_DECISIONS,
  DIRECT_ZIP_PEAK_SPACE_DECISIONS,
  DIRECT_ZIP_PERMISSION_DECISIONS,
  DIRECT_ZIP_PREFIX_COPY_DECISIONS,
  DIRECT_ZIP_SPACE_DECISIONS,
  type DirectZipDiagnosticDecisionSnapshot,
  type DirectZipDiagnosticMilestoneInput,
  type DirectZipLocalDiagnosticRecord,
} from './model'

export const DIRECT_ZIP_LOCAL_DIAGNOSTIC_CAPACITY = 64
export const DIRECT_ZIP_EXPORT_PROJECTION_CAPACITY = 32

export function snapshotDirectZipLocalDiagnostic(
  input: DirectZipDiagnosticMilestoneInput,
  observedAtMilliseconds: number,
): DirectZipLocalDiagnosticRecord {
  if (!Number.isSafeInteger(observedAtMilliseconds) || observedAtMilliseconds < 0) {
    throw new TypeError('direct ZIP diagnostic clock must be non-negative safe milliseconds')
  }
  assertClosedDirectZipDiagnostic(input)
  return Object.freeze({
    operationId: snapshotIdentity(input.operationId, 16, 'direct ZIP diagnostic operation ID'),
    sessionId: snapshotIdentity(input.sessionId, 16, 'direct ZIP diagnostic session ID'),
    planKind: input.planKind,
    milestone: input.milestone,
    checkpointPhase: input.checkpointPhase,
    epochOffsetClass: input.epochOffsetClass,
    decisions: Object.freeze({
      prefixCopy: input.decisions.prefixCopy,
      peakSpace: input.decisions.peakSpace,
      permission: input.decisions.permission,
      identity: input.decisions.identity,
      space: input.decisions.space,
      cleanup: input.decisions.cleanup,
    }),
    observedAtMilliseconds,
    rawFsaStageFactsObserved: Object.hasOwn(input, 'rawFsaStageFacts'),
    rawExceptionObserved: Object.hasOwn(input, 'rawException'),
    ...(Object.hasOwn(input, 'rawFsaStageFacts')
      ? { rawFsaStageFacts: input.rawFsaStageFacts }
      : {}),
    ...(Object.hasOwn(input, 'rawException') ? { rawException: input.rawException } : {}),
  })
}

/**
 * Export deliberately rebuilds the record from closed facts. Raw FSA objects,
 * paths, names, exception text, and provider-specific stage details never cross
 * this boundary even when they remain available to local troubleshooting.
 */
export function projectDirectZipDiagnosticV1(
  record: DirectZipLocalDiagnosticRecord,
): DirectZipMilestonePayloadV1 {
  assertClosedDirectZipDiagnostic(record)
  const nativeErrorClass = record.rawExceptionObserved
    ? projectOutputException(record.rawException, () => '').nativeClass
    : undefined
  return Object.freeze({
    operation_id: snapshotIdentity(record.operationId, 16, 'direct ZIP diagnostic operation ID'),
    session_id: snapshotIdentity(record.sessionId, 16, 'direct ZIP diagnostic session ID'),
    plan_kind: 'direct_resumable_zip',
    milestone: snake(record.milestone),
    checkpoint_phase: snake(record.checkpointPhase),
    epoch_offset_class: snake(record.epochOffsetClass),
    prefix_copy_decision: snake(record.decisions.prefixCopy),
    peak_space_decision: snake(record.decisions.peakSpace),
    permission_decision: snake(record.decisions.permission),
    identity_decision: snake(record.decisions.identity),
    space_decision: snake(record.decisions.space),
    cleanup_decision: snake(record.decisions.cleanup),
    ...(nativeErrorClass === undefined ? {} : { native_error_class: nativeErrorClass }),
  }) as DirectZipMilestonePayloadV1
}

export function projectDirectZipDiagnosticHistoryV1(
  records: readonly DirectZipLocalDiagnosticRecord[],
  capacity = DIRECT_ZIP_EXPORT_PROJECTION_CAPACITY,
): readonly DirectZipMilestonePayloadV1[] {
  requireBoundedCapacity(
    capacity,
    DIRECT_ZIP_EXPORT_PROJECTION_CAPACITY,
    'direct ZIP export projection',
  )
  return Object.freeze(records.slice(-capacity).map(projectDirectZipDiagnosticV1))
}

export function requireDirectZipDiagnosticCapacity(capacity: number): number {
  return requireBoundedCapacity(
    capacity,
    DIRECT_ZIP_LOCAL_DIAGNOSTIC_CAPACITY,
    'direct ZIP local diagnostic',
  )
}

function requireBoundedCapacity(capacity: number, maximum: number, label: string): number {
  if (!Number.isSafeInteger(capacity) || capacity <= 0 || capacity > maximum) {
    throw new RangeError(`${label} capacity must be between 1 and ${maximum}`)
  }
  return capacity
}

function assertClosedDirectZipDiagnostic(input: DirectZipDiagnosticMilestoneInput): void {
  requireMember([DIRECT_ZIP_DIAGNOSTIC_PLAN_KIND], input.planKind, 'plan kind')
  requireMember(DIRECT_ZIP_DIAGNOSTIC_MILESTONES, input.milestone, 'milestone')
  requireMember(DIRECT_ZIP_CHECKPOINT_PHASES, input.checkpointPhase, 'checkpoint phase')
  requireMember(DIRECT_ZIP_EPOCH_OFFSET_CLASSES, input.epochOffsetClass, 'epoch offset class')
  assertClosedDecisions(input.decisions)
}

function assertClosedDecisions(decisions: DirectZipDiagnosticDecisionSnapshot): void {
  if (typeof decisions !== 'object' || decisions === null) {
    throw new TypeError('direct ZIP diagnostic decisions are missing')
  }
  requireMember(DIRECT_ZIP_PREFIX_COPY_DECISIONS, decisions.prefixCopy, 'prefix-copy decision')
  requireMember(DIRECT_ZIP_PEAK_SPACE_DECISIONS, decisions.peakSpace, 'peak-space decision')
  requireMember(DIRECT_ZIP_PERMISSION_DECISIONS, decisions.permission, 'permission decision')
  requireMember(DIRECT_ZIP_IDENTITY_DECISIONS, decisions.identity, 'identity decision')
  requireMember(DIRECT_ZIP_SPACE_DECISIONS, decisions.space, 'space decision')
  requireMember(DIRECT_ZIP_CLEANUP_DECISIONS, decisions.cleanup, 'cleanup decision')
}

function requireMember<const Value extends string>(
  values: readonly Value[],
  value: unknown,
  label: string,
): asserts value is Value {
  if (typeof value !== 'string' || !values.includes(value as Value)) {
    throw new TypeError(`direct ZIP diagnostic ${label} is invalid`)
  }
}

function snake<Value extends string>(value: Value): SnakeCase<Value> {
  return value.replaceAll('-', '_') as SnakeCase<Value>
}

type SnakeCase<Value extends string> =
  Value extends `${infer Head}-${infer Tail}`
    ? `${Head}_${SnakeCase<Tail>}`
    : Value
