export const DIAGNOSTIC_IDENTITY_BYTES = 16
const UINT64_MAX = 0xffff_ffff_ffff_ffffn

export const DIAGNOSTIC_IDENTITY_KINDS = Object.freeze([
  'protocol_session',
  'protocol_operation',
  'peer_path',
  'peer_attempt',
] as const)

export type DiagnosticIdentityKind = (typeof DIAGNOSTIC_IDENTITY_KINDS)[number]
export type DiagnosticBytesIdentityKind = Exclude<DiagnosticIdentityKind, 'protocol_operation'>

export interface DiagnosticBytesIdentity<
  Kind extends DiagnosticBytesIdentityKind = DiagnosticBytesIdentityKind,
> {
  readonly kind: Kind
  readonly byteLength: typeof DIAGNOSTIC_IDENTITY_BYTES
  copyBytes(): Uint8Array<ArrayBuffer>
}

export interface ProtocolOperationIdentity {
  readonly kind: 'protocol_operation'
  readonly value: bigint
  toDecimalString(): string
}

export type DiagnosticIdentity<
  Kind extends DiagnosticIdentityKind = DiagnosticIdentityKind,
> = Kind extends 'protocol_operation'
  ? ProtocolOperationIdentity
  : DiagnosticBytesIdentity<Extract<Kind, DiagnosticBytesIdentityKind>>

export type ProtocolSessionIdentity = DiagnosticIdentity<'protocol_session'>
export type PeerPathIdentity = DiagnosticIdentity<'peer_path'>
export type PeerAttemptIdentity = DiagnosticIdentity<'peer_attempt'>

export interface ProtocolLaneCorrelation {
  readonly id: number
  readonly epoch: number
}

export interface ProtocolCorrelation {
  readonly protocolSessionId?: ProtocolSessionIdentity
  readonly protocolOperationId?: ProtocolOperationIdentity
  readonly peerPathId?: PeerPathIdentity
  readonly peerAttemptId?: PeerAttemptIdentity
  readonly lane?: ProtocolLaneCorrelation
}

class FixedDiagnosticIdentity<Kind extends DiagnosticBytesIdentityKind>
implements DiagnosticBytesIdentity<Kind> {
  readonly kind: Kind
  readonly byteLength = DIAGNOSTIC_IDENTITY_BYTES
  readonly #bytes: Uint8Array<ArrayBuffer>

  constructor(kind: Kind, bytes: Uint8Array) {
    this.kind = kind
    this.#bytes = bytes.slice()
    Object.freeze(this)
  }

  copyBytes(): Uint8Array<ArrayBuffer> {
    return this.#bytes.slice()
  }
}

class FixedProtocolOperationIdentity implements ProtocolOperationIdentity {
  readonly kind = 'protocol_operation' as const
  readonly value: bigint

  constructor(value: bigint) {
    this.value = value
    Object.freeze(this)
  }

  toDecimalString(): string {
    return this.value.toString(10)
  }

  toJSON(): Readonly<{ kind: 'protocol_operation'; value: string }> {
    return { kind: this.kind, value: this.value.toString(10) }
  }
}

export function createDiagnosticIdentity(
  kind: 'protocol_operation',
  value: bigint,
): ProtocolOperationIdentity
export function createDiagnosticIdentity<Kind extends DiagnosticBytesIdentityKind>(
  kind: Kind,
  bytes: Uint8Array,
): DiagnosticBytesIdentity<Kind>
export function createDiagnosticIdentity(
  kind: DiagnosticIdentityKind,
  input: Uint8Array | bigint,
): DiagnosticIdentity {
  if (!DIAGNOSTIC_IDENTITY_KINDS.includes(kind)) {
    throw new RangeError('Unknown v2 diagnostic identity kind')
  }
  if (kind === 'protocol_operation') {
    if (typeof input !== 'bigint' || input < 1n || input > UINT64_MAX) {
      throw new RangeError('V2 protocol operation identities must be nonzero uint64 values')
    }
    return new FixedProtocolOperationIdentity(input)
  }
  if (
    !(input instanceof Uint8Array) ||
    input.byteLength !== DIAGNOSTIC_IDENTITY_BYTES ||
    !input.some((value) => value !== 0)
  ) {
    throw new RangeError('V2 diagnostic identities must be nonzero 16-byte values')
  }
  return new FixedDiagnosticIdentity(kind, input)
}

export function createProtocolSessionIdentity(
  bytes: Uint8Array,
): ProtocolSessionIdentity {
  return createDiagnosticIdentity('protocol_session', bytes)
}

export function createProtocolOperationIdentity(
  value: bigint,
): ProtocolOperationIdentity {
  return createDiagnosticIdentity('protocol_operation', value)
}

export function createPeerPathIdentityValue(bytes: Uint8Array): PeerPathIdentity {
  return createDiagnosticIdentity('peer_path', bytes)
}

export function createPeerAttemptIdentity(bytes: Uint8Array): PeerAttemptIdentity {
  return createDiagnosticIdentity('peer_attempt', bytes)
}

export function equalDiagnosticIdentities(
  left: DiagnosticIdentity,
  right: DiagnosticIdentity,
): boolean {
  if (left.kind !== right.kind) return false
  if (left.kind === 'protocol_operation') {
    return right.kind === 'protocol_operation' && left.value === right.value
  }
  if (right.kind === 'protocol_operation') return false
  if (left.byteLength !== right.byteLength) return false
  const leftBytes = left.copyBytes()
  const rightBytes = right.copyBytes()
  if (
    leftBytes.byteLength !== DIAGNOSTIC_IDENTITY_BYTES ||
    rightBytes.byteLength !== DIAGNOSTIC_IDENTITY_BYTES
  ) return false
  return leftBytes.every((value, index) => value === rightBytes[index])
}
