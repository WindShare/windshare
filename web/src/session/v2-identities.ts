export const V2_DIAGNOSTIC_IDENTITY_BYTES = 16

export const V2_DIAGNOSTIC_IDENTITY_KINDS = Object.freeze([
  'protocol_session',
  'protocol_operation',
  'peer_path',
  'peer_attempt',
] as const)

export type V2DiagnosticIdentityKind = (typeof V2_DIAGNOSTIC_IDENTITY_KINDS)[number]

export interface V2DiagnosticIdentity<
  Kind extends V2DiagnosticIdentityKind = V2DiagnosticIdentityKind,
> {
  readonly kind: Kind
  readonly byteLength: typeof V2_DIAGNOSTIC_IDENTITY_BYTES
  copyBytes(): Uint8Array<ArrayBuffer>
}

export type V2ProtocolSessionIdentity = V2DiagnosticIdentity<'protocol_session'>
export type V2ProtocolOperationIdentity = V2DiagnosticIdentity<'protocol_operation'>
export type V2PeerPathIdentity = V2DiagnosticIdentity<'peer_path'>
export type V2PeerAttemptIdentity = V2DiagnosticIdentity<'peer_attempt'>

class FixedV2DiagnosticIdentity<Kind extends V2DiagnosticIdentityKind>
implements V2DiagnosticIdentity<Kind> {
  readonly kind: Kind
  readonly byteLength = V2_DIAGNOSTIC_IDENTITY_BYTES
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

export function createV2DiagnosticIdentity<Kind extends V2DiagnosticIdentityKind>(
  kind: Kind,
  bytes: Uint8Array,
): V2DiagnosticIdentity<Kind> {
  if (!V2_DIAGNOSTIC_IDENTITY_KINDS.includes(kind)) {
    throw new RangeError('Unknown v2 diagnostic identity kind')
  }
  if (
    !(bytes instanceof Uint8Array) ||
    bytes.byteLength !== V2_DIAGNOSTIC_IDENTITY_BYTES ||
    !bytes.some((value) => value !== 0)
  ) {
    throw new RangeError('V2 diagnostic identities must be nonzero 16-byte values')
  }
  return new FixedV2DiagnosticIdentity(kind, bytes)
}

export function createV2ProtocolSessionIdentity(
  bytes: Uint8Array,
): V2ProtocolSessionIdentity {
  return createV2DiagnosticIdentity('protocol_session', bytes)
}

export function createV2ProtocolOperationIdentity(
  bytes: Uint8Array,
): V2ProtocolOperationIdentity {
  return createV2DiagnosticIdentity('protocol_operation', bytes)
}

export function createV2PeerPathIdentityValue(bytes: Uint8Array): V2PeerPathIdentity {
  return createV2DiagnosticIdentity('peer_path', bytes)
}

export function createV2PeerAttemptIdentity(bytes: Uint8Array): V2PeerAttemptIdentity {
  return createV2DiagnosticIdentity('peer_attempt', bytes)
}

export function equalV2DiagnosticIdentities(
  left: V2DiagnosticIdentity,
  right: V2DiagnosticIdentity,
): boolean {
  if (left.kind !== right.kind || left.byteLength !== right.byteLength) return false
  const leftBytes = left.copyBytes()
  const rightBytes = right.copyBytes()
  if (
    leftBytes.byteLength !== V2_DIAGNOSTIC_IDENTITY_BYTES ||
    rightBytes.byteLength !== V2_DIAGNOSTIC_IDENTITY_BYTES
  ) return false
  return leftBytes.every((value, index) => value === rightBytes[index])
}
