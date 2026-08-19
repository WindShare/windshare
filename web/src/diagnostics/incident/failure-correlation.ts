export const FAILURE_IDENTITY_KINDS = Object.freeze([
  'protocol_session',
  'protocol_operation',
  'peer_path',
  'peer_attempt',
] as const)

export type FailureIdentityKind = (typeof FAILURE_IDENTITY_KINDS)[number]

export interface FailureIdentity<Kind extends FailureIdentityKind = FailureIdentityKind> {
  readonly kind: Kind
  readonly byteLength: 16
  copyBytes(): Uint8Array
}

export interface FailureLaneCorrelation {
  readonly id: number
  readonly epoch: number
}

export interface FailureCorrelation {
  readonly protocolSessionId?: FailureIdentity<'protocol_session'>
  readonly protocolOperationId?: FailureIdentity<'protocol_operation'>
  readonly peerPathId?: FailureIdentity<'peer_path'>
  readonly peerAttemptId?: FailureIdentity<'peer_attempt'>
  readonly lane?: FailureLaneCorrelation
}

const IDENTITY_BYTE_LENGTH = 16
const UINT32_MAX = 0xffff_ffff

class FixedFailureIdentity<Kind extends FailureIdentityKind>
implements FailureIdentity<Kind> {
  readonly byteLength = IDENTITY_BYTE_LENGTH
  readonly kind: Kind
  readonly #bytes: Uint8Array

  constructor(kind: Kind, bytes: Uint8Array) {
    this.kind = kind
    this.#bytes = bytes.slice()
    Object.freeze(this)
  }

  copyBytes(): Uint8Array {
    return this.#bytes.slice()
  }
}

export function createFailureIdentity<Kind extends FailureIdentityKind>(
  kind: Kind,
  bytes: Uint8Array,
): FailureIdentity<Kind> {
  if (!isMember(FAILURE_IDENTITY_KINDS, kind)) {
    throw new RangeError('Unknown failure identity kind')
  }
  if (!(bytes instanceof Uint8Array) || bytes.byteLength !== IDENTITY_BYTE_LENGTH) {
    throw new RangeError('Failure identities must be exactly 16 bytes')
  }
  if (bytes.every((value) => value === 0)) {
    throw new RangeError('Failure identities must be non-zero')
  }
  return new FixedFailureIdentity(kind, bytes)
}

export function createFailureCorrelation(
  input: FailureCorrelation,
): FailureCorrelation {
  if (!isFailureCorrelation(input)) {
    throw new TypeError('Failure correlation is invalid')
  }
  const correlation: FailureCorrelation = {
    ...(input.protocolSessionId === undefined
      ? {}
      : { protocolSessionId: copyIdentity(input.protocolSessionId) }),
    ...(input.protocolOperationId === undefined
      ? {}
      : { protocolOperationId: copyIdentity(input.protocolOperationId) }),
    ...(input.peerPathId === undefined
      ? {}
      : { peerPathId: copyIdentity(input.peerPathId) }),
    ...(input.peerAttemptId === undefined
      ? {}
      : { peerAttemptId: copyIdentity(input.peerAttemptId) }),
    ...(input.lane === undefined
      ? {}
      : { lane: Object.freeze({ id: input.lane.id, epoch: input.lane.epoch }) }),
  }
  return Object.freeze(correlation)
}

export function isFailureCorrelation(value: unknown): value is FailureCorrelation {
  if (!isRecord(value) || !hasOnlyKeys(value, [
    'protocolSessionId',
    'protocolOperationId',
    'peerPathId',
    'peerAttemptId',
    'lane',
  ])) {
    return false
  }
  const presentCount = Object.values(value).filter((entry) => entry !== undefined).length
  if (presentCount === 0) return false
  if (value.protocolOperationId !== undefined && value.protocolSessionId === undefined) {
    return false
  }
  if (value.peerAttemptId !== undefined && value.peerPathId === undefined) return false
  if (
    !optionalIdentity(value.protocolSessionId, 'protocol_session') ||
    !optionalIdentity(value.protocolOperationId, 'protocol_operation') ||
    !optionalIdentity(value.peerPathId, 'peer_path') ||
    !optionalIdentity(value.peerAttemptId, 'peer_attempt')
  ) {
    return false
  }
  return value.lane === undefined || isFailureLaneCorrelation(value.lane)
}

export function failureCorrelationsEqual(
  left: FailureCorrelation,
  right: FailureCorrelation,
): boolean {
  return (
    identitiesEqual(left.protocolSessionId, right.protocolSessionId) &&
    identitiesEqual(left.protocolOperationId, right.protocolOperationId) &&
    identitiesEqual(left.peerPathId, right.peerPathId) &&
    identitiesEqual(left.peerAttemptId, right.peerAttemptId) &&
    (
      left.lane === undefined
        ? right.lane === undefined
        : right.lane !== undefined &&
          left.lane.id === right.lane.id &&
          left.lane.epoch === right.lane.epoch
    )
  )
}

function isFailureLaneCorrelation(value: unknown): value is FailureLaneCorrelation {
  return (
    isRecord(value) &&
    hasExactKeys(value, ['id', 'epoch']) &&
    isIntegerBetween(value.id, 0, UINT32_MAX) &&
    isIntegerBetween(value.epoch, 0, UINT32_MAX)
  )
}

function optionalIdentity<Kind extends FailureIdentityKind>(
  value: unknown,
  kind: Kind,
): value is FailureIdentity<Kind> | undefined {
  return value === undefined || isIdentity(value, kind)
}

function isIdentity<Kind extends FailureIdentityKind>(
  value: unknown,
  kind: Kind,
): value is FailureIdentity<Kind> {
  if (
    !isRecord(value) ||
    value.kind !== kind ||
    value.byteLength !== IDENTITY_BYTE_LENGTH ||
    typeof value.copyBytes !== 'function'
  ) {
    return false
  }
  try {
    const first = value.copyBytes()
    if (
      !(first instanceof Uint8Array) ||
      first.byteLength !== IDENTITY_BYTE_LENGTH ||
      first.every((entry) => entry === 0)
    ) {
      return false
    }
    const second = value.copyBytes()
    return (
      second instanceof Uint8Array &&
      second.byteLength === IDENTITY_BYTE_LENGTH &&
      first !== second &&
      first.every((entry, index) => entry === second[index])
    )
  } catch {
    return false
  }
}

function copyIdentity<Kind extends FailureIdentityKind>(
  identity: FailureIdentity<Kind>,
): FailureIdentity<Kind> {
  return createFailureIdentity(identity.kind, identity.copyBytes())
}

function identitiesEqual(
  left: FailureIdentity | undefined,
  right: FailureIdentity | undefined,
): boolean {
  if (left === undefined || right === undefined) return left === right
  if (left.kind !== right.kind) return false
  const leftBytes = left.copyBytes()
  const rightBytes = right.copyBytes()
  return leftBytes.every((value, index) => value === rightBytes[index])
}

function isIntegerBetween(value: unknown, minimum: number, maximum: number): boolean {
  return (
    typeof value === 'number' &&
    Number.isInteger(value) &&
    value >= minimum &&
    value <= maximum
  )
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

function hasOnlyKeys(
  value: Record<string, unknown>,
  allowed: readonly string[],
): boolean {
  return Object.keys(value).every((key) => allowed.includes(key))
}
