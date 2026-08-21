import { encodeBase64Url } from '../../crypto/bytes'
import type { NamedContainerEntryReservation } from '../../transfer/intent'
import {
  emitOutputTrace,
  outputTraceEvent,
  type OutputDiagnosticsPorts,
} from '../diagnostics'
import {
  FILE_CHECKPOINT_ID_BYTES,
  identityBytes,
} from '../persistence/checkpoint'
import type { PersistentTreeTraceEvent } from '../persistent-tree/contracts'
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
