import { encodeBase64Url } from '../../crypto/bytes'
import type { NamedContainerEntryReservation } from '../../transfer/intent'
import {
  emitOutputTrace,
  outputTraceEvent,
  type OutputDiagnosticsPorts,
  type OutputTracePayloadByName,
} from '../diagnostics'
import {
  FILE_CHECKPOINT_ID_BYTES,
  identityBytes,
} from '../persistence/checkpoint'
import type {
  CheckpointAuthorityObservation,
  CheckpointAuthorityObserver,
  PersistentTreeTraceEvent,
} from '../persistent-tree/contracts'
import type { CompatibleNameRootRepairPreparationOptions } from './compatible-name/coordinator'
import type {
  FSAOutputTrace,
  FSAOutputTraceEvent,
  FSAReservationTraceEvent,
} from './session'

export function reservationCreated(
  reservation: NamedContainerEntryReservation,
): Extract<FSAReservationTraceEvent, { name: 'receive.reservation.created' }> {
  const nameAuthority = reservation.guarantees.nameAuthority
  if (nameAuthority !== 'application-chosen' && nameAuthority !== 'user-chosen') {
    throw new TypeError('FSA reservation name authority is invalid')
  }
  return Object.freeze({
    name: 'receive.reservation.created',
    operation_id: reservation.operationId,
    reservation_kind: 'named-container-entry',
    collision_index: reservation.collisionIndex,
    name_authority: nameAuthority,
    replacement_guarantee: 'coordinated-no-replace',
    delivery_mode: 'managed-target',
    commit_visibility: 'prefix-visible',
    rollback_guarantee: 'none',
  })
}

export function needsAttention(
  operationId: string,
): Extract<PersistentTreeTraceEvent, { name: 'receive.operation.needs_attention' }> {
  return Object.freeze({
    name: 'receive.operation.needs_attention',
    operation_id: operationId,
    prior_state: 'receiving',
    needs_attention_reason: 'target-ownership-unknown',
  })
}

type FSAOutputTraceInput =
  | Readonly<{
      eventName: 'output_reservation'
      transition: 'acquired' | 'failed'
    }>
  | Readonly<{
      eventName: 'settlement'
      transition: 'started' | 'completed' | 'failed'
    }>
  | Readonly<{
      eventName: 'reopen'
      transition: 'authorized' | 'failed'
    }>
  | Readonly<{
      eventName: 'cleanup'
      transition: 'completed' | 'failed'
    }>

export function outputTrace(
  diagnostics: OutputDiagnosticsPorts | undefined,
  input: FSAOutputTraceInput,
): void {
  emitOutputTrace(diagnostics?.trace, () =>
    outputTraceEvent(input.eventName, {
      backend: 'file_system_access',
      transition: input.transition,
    }))
}

export function checkpointAuthorityObserver(
  diagnostics: OutputDiagnosticsPorts | undefined,
): CheckpointAuthorityObserver | undefined {
  if (diagnostics?.trace === undefined) return undefined
  return observation => emitOutputTrace(diagnostics.trace, () =>
    outputTraceEvent('checkpoint', checkpointAuthorityPayload(observation)))
}

function checkpointAuthorityPayload(
  observation: CheckpointAuthorityObservation,
): Extract<OutputTracePayloadByName['checkpoint'], {
  readonly transition: 'authority_decision'
}> {
  return Object.freeze({
    backend: 'file_system_access',
    transition: 'authority_decision',
    authority: snakeCase(observation.authority),
    receive_operation_id: observation.receiveOperationId,
    transfer_job_id: observation.transferJobId,
    output_session_id: observation.outputSessionId,
    materialization_relative_path: Object.freeze([...observation.materializationRelativePath]),
    trigger: snakeCase(observation.trigger),
    ...(observation.checkpointOrdinal === undefined
      ? {}
      : { checkpoint_ordinal: observation.checkpointOrdinal }),
    prefix_copy_bytes: observation.cost.prefixCopyBytes.toString(),
    write_amplification_bytes: observation.cost.writeAmplificationBytes.toString(),
    temporary_bytes: observation.cost.temporaryBytes.toString(),
    ...(observation.remainingAutomaticWriteAmplificationBytes === undefined
      ? {}
      : {
          remaining_automatic_write_amplification_bytes:
            observation.remainingAutomaticWriteAmplificationBytes.toString(),
        }),
    decision: snakeCase(observation.decision),
    ...(observation.releaseReason === undefined
      ? {}
      : { release_reason: snakeCase(observation.releaseReason) }),
  }) as Extract<OutputTracePayloadByName['checkpoint'], {
    readonly transition: 'authority_decision'
  }>
}

function snakeCase<Value extends string>(value: Value): ReplaceHyphen<Value> {
  return value.replaceAll('-', '_') as ReplaceHyphen<Value>
}

type ReplaceHyphen<Value extends string> = Value extends `${infer Head}-${infer Tail}`
  ? `${Head}_${ReplaceHyphen<Tail>}`
  : Value

export function emitFSAOutputTrace(
  trace: FSAOutputTrace | undefined,
  event: FSAOutputTraceEvent,
): void {
  try {
    trace?.(event)
  } catch {
    // Durable destination state must never depend on an observability sink.
  }
}

export function canonicalAuthorityRef(value: string): string {
  return encodeBase64Url(identityBytes(value, FILE_CHECKPOINT_ID_BYTES, 'authority reference'))
}

export function defaultCompatibleNamePreparation(): CompatibleNameRootRepairPreparationOptions {
  const platform = globalThis.navigator?.platform
  return Object.freeze({
    platform: typeof platform === 'string' && platform.toLowerCase().startsWith('win')
      ? 'windows'
      : 'unsupported',
  })
}

export function createFSAAuthorityReference(): string {
  const value = new Uint8Array(FILE_CHECKPOINT_ID_BYTES)
  crypto.getRandomValues(value)
  return canonicalAuthorityRef(encodeBase64Url(value))
}
