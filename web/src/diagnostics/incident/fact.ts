import {
  CheckpointFaultCode,
  OutputFaultCode,
  isFault,
  type CheckpointFaultCode as CheckpointFaultCodeValue,
  type Fault,
  type OutputFaultCode as OutputFaultCodeValue,
} from '../../transfer/fault'
import type {
  NeedsAttentionReason,
  PartialDirectoryReason,
  ReceiveLifecycleState,
  RestartRequiredReason,
} from '../../output/workspace/state'
import {
  V2_CONNECTIVITY_FAILURE_SCOPES,
  V2_TYPED_PEER_ERROR_CODES,
  type V2ConnectivityFailureScope,
  type V2TypedPeerErrorCode,
} from '../../connectivity/diagnostics'
import {
  createFailureCorrelation,
  failureCorrelationsEqual,
  isFailureCorrelation,
  type FailureCorrelation,
} from './failure-correlation'
import {
  createProtocolFailure,
  isProtocolFailure,
  type ProtocolFailure,
} from './protocol-failure'

export {
  FAILURE_IDENTITY_KINDS,
  createFailureCorrelation,
  createFailureIdentity,
  isFailureCorrelation,
  type FailureCorrelation,
  type FailureIdentity,
  type FailureIdentityKind,
  type FailureLaneCorrelation,
} from './failure-correlation'
export {
  PROTOCOL_FAILURE_SCOPES,
  PROTOCOL_MESSAGE_KINDS_V1,
  PROTOCOL_REQUEST_KINDS_V1,
  createProtocolFailure,
  isProtocolFailure,
  type ProtocolFailure,
  type ProtocolFailureInput,
  type ProtocolFailureScope,
  type ProtocolMessageKindV1,
  type ProtocolRequestKindV1,
  type ProtocolSettlement,
} from './protocol-failure'

export const FAILURE_FACT_KINDS = Object.freeze([
  'fault',
  'protocol_failure',
  'peer_failure',
  'native_output_failure',
  'lifecycle_failure',
  'unclassified',
] as const)

export type FailureFactKind = (typeof FAILURE_FACT_KINDS)[number]

export const RECOVERY_DISPOSITIONS = Object.freeze([
  'none',
  'retryable',
  'resumable_receive',
  'resumable_package',
  'restart_required',
  'needs_attention',
  'terminal',
] as const)

export type RecoveryDisposition = (typeof RECOVERY_DISPOSITIONS)[number]

export const FAILURE_STAGES = Object.freeze([
  'join',
  'browse',
  'preview_open',
  'preview_seek',
  'preview_media',
  'projection',
  'authority_selection',
  'authority_activation',
  'protocol_operation',
  'peer_attempt',
  'peer_recovery',
  'content_read',
  'output_reservation',
  'output_write',
  'output_commit',
  'checkpoint',
  'settlement',
  'publication',
  'continuation',
  'reopen',
  'detach',
  'cleanup',
  'retained_inventory',
  'retained_action',
  'lifecycle_action',
] as const)

export type FailureStage = (typeof FAILURE_STAGES)[number]

export const NATIVE_FAILURE_CLASSES = Object.freeze([
  'abort',
  'timeout',
  'not_allowed',
  'not_found',
  'invalid_state',
  'quota_exceeded',
  'data',
  'security',
  'not_supported',
  'no_modification_allowed',
  'unknown',
] as const)

export type NativeFailureClass = (typeof NATIVE_FAILURE_CLASSES)[number]

type ReceiveLifecycleKind = ReceiveLifecycleState['kind']
type LifecycleFailureReason =
  | PartialDirectoryReason
  | RestartRequiredReason
  | NeedsAttentionReason

export interface FailureFactByKind {
  readonly fault: Readonly<{ fault: Fault }>
  readonly protocol_failure: Readonly<{ protocolFailure: ProtocolFailure }>
  readonly peer_failure: Readonly<{
    peerFailure: Readonly<{
      scope: V2ConnectivityFailureScope
      code: V2TypedPeerErrorCode
      retryable: boolean
    }>
  }>
  readonly native_output_failure: Readonly<{
    nativeOutputFailure: Readonly<{
      nativeClass: NativeFailureClass
      code?: OutputFaultCodeValue | CheckpointFaultCodeValue
    }>
  }>
  readonly lifecycle_failure: Readonly<{
    lifecycleFailure: Readonly<{
      kind: ReceiveLifecycleKind
      reason?: LifecycleFailureReason
    }>
  }>
  readonly unclassified: Readonly<{ unclassified: Readonly<Record<never, never>> }>
}

export type FailureFact<Kind extends FailureFactKind = FailureFactKind> =
  Kind extends FailureFactKind
    ? Readonly<{
        kind: Kind
        stage: FailureStage
        recoveryDisposition: RecoveryDisposition
        correlation?: FailureCorrelation
        payload: FailureFactByKind[Kind]
      }>
    : never

const lifecycleKinds = [
  'intent-frozen',
  'preparing',
  'receiving',
  'resumable-receive',
  'finalizing-tree',
  'committing-atomic',
  'materialization-sealed',
  'packaging',
  'resumable-package',
  'artifact-sealed',
  'waiting-to-save',
  'publishing-managed',
  'handing-off',
  'published',
  'download-started',
  'partial-directory',
  'restart-required',
  'discarded',
  'expired',
  'needs-attention',
  'authorization-required',
  'target-verification-required',
  'destination-space-required',
] as const satisfies readonly ReceiveLifecycleKind[]

const lifecycleReasonsByKind = {
  'partial-directory': ['failures', 'stopped'],
  'restart-required': [
    'direct-atomic-rolled-back',
    'portable-aborted',
    'source-revision-changed',
    'preparation-invalidated',
    'content-session-ended',
    'target-deleted',
  ],
  'needs-attention': [
    'target-ownership-unknown',
    'publication-unknown',
    'cleanup-unknown',
  ],
} as const

const outputFaultCodes = Object.values(OutputFaultCode) as readonly OutputFaultCodeValue[]
const checkpointFaultCodes =
  Object.values(CheckpointFaultCode) as readonly CheckpointFaultCodeValue[]

export function faultFailureFact(input: {
  readonly stage: FailureStage
  readonly recoveryDisposition: RecoveryDisposition
  readonly fault: Fault
  readonly correlation?: FailureCorrelation
}): FailureFact<'fault'> {
  if (!isClosedImmutableFault(input.fault)) {
    throw new TypeError('Fault failure facts require an immutable normalized fault')
  }
  return createFact(
    'fault',
    input.stage,
    input.recoveryDisposition,
    Object.freeze({ fault: input.fault }),
    input.correlation,
  )
}

export function protocolFailureFact(input: {
  readonly stage: FailureStage
  readonly recoveryDisposition: RecoveryDisposition
  readonly protocolFailure: ProtocolFailure
}): FailureFact<'protocol_failure'> {
  const protocolFailure = createProtocolFailure(input.protocolFailure)
  return createFact(
    'protocol_failure',
    input.stage,
    input.recoveryDisposition,
    Object.freeze({ protocolFailure }),
    protocolFailure.correlation,
  )
}

export function peerFailureFact(input: {
  readonly stage: FailureStage
  readonly recoveryDisposition: RecoveryDisposition
  readonly scope: V2ConnectivityFailureScope
  readonly code: V2TypedPeerErrorCode
  readonly retryable: boolean
  readonly correlation?: FailureCorrelation
}): FailureFact<'peer_failure'> {
  if (
    !isMember(V2_CONNECTIVITY_FAILURE_SCOPES, input.scope) ||
    !isMember(V2_TYPED_PEER_ERROR_CODES, input.code)
  ) {
    throw new TypeError('Peer failure is invalid')
  }
  return createFact(
    'peer_failure',
    input.stage,
    input.recoveryDisposition,
    Object.freeze({
      peerFailure: Object.freeze({
        scope: input.scope,
        code: input.code,
        retryable: input.retryable,
      }),
    }),
    input.correlation,
  )
}

export function nativeOutputFailureFact(input: {
  readonly stage: FailureStage
  readonly recoveryDisposition: RecoveryDisposition
  readonly nativeClass: NativeFailureClass
  readonly code?: OutputFaultCodeValue | CheckpointFaultCodeValue
  readonly correlation?: FailureCorrelation
}): FailureFact<'native_output_failure'> {
  if (!isMember(NATIVE_FAILURE_CLASSES, input.nativeClass)) {
    throw new TypeError('Native output failure class is invalid')
  }
  if (
    input.code !== undefined &&
    !isNativeOutputCodeForStage(input.stage, input.code)
  ) {
    throw new TypeError('Native output failure code is invalid for its stage')
  }
  return createFact(
    'native_output_failure',
    input.stage,
    input.recoveryDisposition,
    Object.freeze({
      nativeOutputFailure: Object.freeze({
        nativeClass: input.nativeClass,
        ...(input.code === undefined ? {} : { code: input.code }),
      }),
    }),
    input.correlation,
  )
}

export function lifecycleFailureFact(input: {
  readonly stage: FailureStage
  readonly recoveryDisposition: RecoveryDisposition
  readonly kind: ReceiveLifecycleKind
  readonly reason?: LifecycleFailureReason
  readonly correlation?: FailureCorrelation
}): FailureFact<'lifecycle_failure'> {
  if (!isMember(lifecycleKinds, input.kind) || !isLifecycleReason(input.kind, input.reason)) {
    throw new TypeError('Lifecycle failure is invalid')
  }
  return createFact(
    'lifecycle_failure',
    input.stage,
    input.recoveryDisposition,
    Object.freeze({
      lifecycleFailure: Object.freeze({
        kind: input.kind,
        ...(input.reason === undefined ? {} : { reason: input.reason }),
      }),
    }),
    input.correlation,
  )
}

export function unclassifiedFailureFact(input: {
  readonly stage: FailureStage
  readonly recoveryDisposition: RecoveryDisposition
  readonly correlation?: FailureCorrelation
}): FailureFact<'unclassified'> {
  return createFact(
    'unclassified',
    input.stage,
    input.recoveryDisposition,
    Object.freeze({ unclassified: Object.freeze({}) }),
    input.correlation,
  )
}

export function isFailureFact(value: unknown): value is FailureFact {
  if (
    !isRecord(value) ||
    !Object.isFrozen(value) ||
    !hasExactOptionalKeys(
      value,
      ['kind', 'stage', 'recoveryDisposition', 'payload'],
      ['correlation'],
    )
  ) {
    return false
  }
  if (
    !isMember(FAILURE_FACT_KINDS, value.kind) ||
    !isMember(FAILURE_STAGES, value.stage) ||
    !isMember(RECOVERY_DISPOSITIONS, value.recoveryDisposition) ||
    (
      value.correlation !== undefined &&
      (
        !Object.isFrozen(value.correlation) ||
        !isFailureCorrelation(value.correlation)
      )
    ) ||
    !isRecord(value.payload) ||
    !Object.isFrozen(value.payload)
  ) {
    return false
  }
  switch (value.kind) {
    case 'fault':
      return (
        hasExactKeys(value.payload, ['fault']) &&
        isClosedImmutableFault(value.payload.fault)
      )
    case 'protocol_failure':
      return (
        hasExactKeys(value.payload, ['protocolFailure']) &&
        isRecord(value.payload.protocolFailure) &&
        Object.isFrozen(value.payload.protocolFailure) &&
        Object.isFrozen(value.payload.protocolFailure.settlement) &&
        Object.isFrozen(value.payload.protocolFailure.correlation) &&
        isProtocolFailure(value.payload.protocolFailure) &&
        value.correlation !== undefined &&
        failureCorrelationsEqual(value.correlation, value.payload.protocolFailure.correlation)
      )
    case 'peer_failure':
      return (
        hasExactKeys(value.payload, ['peerFailure']) &&
        isRecord(value.payload.peerFailure) &&
        Object.isFrozen(value.payload.peerFailure) &&
        hasExactKeys(value.payload.peerFailure, ['scope', 'code', 'retryable']) &&
        isMember(V2_CONNECTIVITY_FAILURE_SCOPES, value.payload.peerFailure.scope) &&
        isMember(V2_TYPED_PEER_ERROR_CODES, value.payload.peerFailure.code) &&
        typeof value.payload.peerFailure.retryable === 'boolean'
      )
    case 'native_output_failure':
      return isNativeOutputFailurePayload(value.payload, value.stage)
    case 'lifecycle_failure':
      return isLifecycleFailurePayload(value.payload)
    case 'unclassified':
      return (
        hasExactKeys(value.payload, ['unclassified']) &&
        isRecord(value.payload.unclassified) &&
        Object.isFrozen(value.payload.unclassified) &&
        Object.keys(value.payload.unclassified).length === 0
      )
  }
}

function isClosedImmutableFault(value: unknown): value is Fault {
  return (
    isRecord(value) &&
    Object.isFrozen(value) &&
    hasExactKeys(value, ['domain', 'scope', 'code']) &&
    isFault(value)
  )
}

function createFact<Kind extends FailureFactKind>(
  kind: Kind,
  stage: FailureStage,
  recoveryDisposition: RecoveryDisposition,
  payload: FailureFactByKind[Kind],
  correlation?: FailureCorrelation,
): FailureFact<Kind> {
  if (!isMember(FAILURE_STAGES, stage)) throw new RangeError('Unknown failure stage')
  if (!isMember(RECOVERY_DISPOSITIONS, recoveryDisposition)) {
    throw new RangeError('Unknown recovery disposition')
  }
  const fact = Object.freeze({
    kind,
    stage,
    recoveryDisposition,
    ...(correlation === undefined
      ? {}
      : { correlation: createFailureCorrelation(correlation) }),
    payload,
  }) as FailureFact<Kind>
  if (!isFailureFact(fact)) throw new TypeError('Failure fact is invalid')
  return fact
}

function isNativeOutputFailurePayload(
  payload: Record<string, unknown>,
  stage: FailureStage,
): boolean {
  if (
    !hasExactKeys(payload, ['nativeOutputFailure']) ||
    !isRecord(payload.nativeOutputFailure) ||
    !Object.isFrozen(payload.nativeOutputFailure) ||
    !hasExactOptionalKeys(payload.nativeOutputFailure, ['nativeClass'], ['code']) ||
    !isMember(NATIVE_FAILURE_CLASSES, payload.nativeOutputFailure.nativeClass)
  ) {
    return false
  }
  const code = payload.nativeOutputFailure.code
  return code === undefined || isNativeOutputCodeForStage(stage, code)
}

function isNativeOutputCodeForStage(
  stage: FailureStage,
  code: unknown,
): code is OutputFaultCodeValue | CheckpointFaultCodeValue {
  return stage === 'checkpoint'
    ? isMember(checkpointFaultCodes, code)
    : isMember(outputFaultCodes, code)
}

function isLifecycleFailurePayload(
  payload: Record<string, unknown>,
): boolean {
  if (
    !hasExactKeys(payload, ['lifecycleFailure']) ||
    !isRecord(payload.lifecycleFailure) ||
    !Object.isFrozen(payload.lifecycleFailure) ||
    !hasExactOptionalKeys(payload.lifecycleFailure, ['kind'], ['reason']) ||
    !isMember(lifecycleKinds, payload.lifecycleFailure.kind)
  ) {
    return false
  }
  return isLifecycleReason(
    payload.lifecycleFailure.kind,
    payload.lifecycleFailure.reason,
  )
}

function isLifecycleReason(
  kind: ReceiveLifecycleKind,
  reason: unknown,
): reason is LifecycleFailureReason | undefined {
  if (kind === 'partial-directory') {
    return isMember(lifecycleReasonsByKind[kind], reason)
  }
  if (kind === 'restart-required') {
    return isMember(lifecycleReasonsByKind[kind], reason)
  }
  if (kind === 'needs-attention') {
    return isMember(lifecycleReasonsByKind[kind], reason)
  }
  return reason === undefined
}

function isMember<const Value extends string>(
  values: readonly Value[],
  value: unknown,
): value is Value {
  return typeof value === 'string' && values.includes(value as Value)
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function hasExactKeys(
  value: Record<string, unknown>,
  expected: readonly string[],
): boolean {
  const keys = Object.keys(value)
  return keys.length === expected.length && expected.every((key) => keys.includes(key))
}

function hasExactOptionalKeys(
  value: Record<string, unknown>,
  required: readonly string[],
  optional: readonly string[],
): boolean {
  const keys = Object.keys(value)
  return (
    required.every((key) => keys.includes(key)) &&
    keys.every((key) => required.includes(key) || optional.includes(key))
  )
}
