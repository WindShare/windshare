export const DIAGNOSTIC_IDENTITY_BYTES = 16

export const DIAGNOSTIC_IDENTITY_KINDS = Object.freeze([
  'protocol_session',
  'protocol_operation',
  'peer_path',
  'peer_attempt',
] as const)

export type DiagnosticIdentityKind = (typeof DIAGNOSTIC_IDENTITY_KINDS)[number]

export interface DiagnosticIdentity<
  Kind extends DiagnosticIdentityKind = DiagnosticIdentityKind,
> {
  readonly kind: Kind
  readonly byteLength: typeof DIAGNOSTIC_IDENTITY_BYTES
  copyBytes(): Uint8Array<ArrayBuffer>
}

export type ProtocolSessionIdentity = DiagnosticIdentity<'protocol_session'>
export type ProtocolOperationIdentity = DiagnosticIdentity<'protocol_operation'>
export type PeerPathIdentity = DiagnosticIdentity<'peer_path'>
export type PeerAttemptIdentity = DiagnosticIdentity<'peer_attempt'>

class FixedDiagnosticIdentity<Kind extends DiagnosticIdentityKind>
implements DiagnosticIdentity<Kind> {
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

export function createDiagnosticIdentity<Kind extends DiagnosticIdentityKind>(
  kind: Kind,
  bytes: Uint8Array,
): DiagnosticIdentity<Kind> {
  if (!DIAGNOSTIC_IDENTITY_KINDS.includes(kind)) {
    throw new RangeError('Unknown v2 diagnostic identity kind')
  }
  if (
    !(bytes instanceof Uint8Array) ||
    bytes.byteLength !== DIAGNOSTIC_IDENTITY_BYTES ||
    !bytes.some((value) => value !== 0)
  ) {
    throw new RangeError('V2 diagnostic identities must be nonzero 16-byte values')
  }
  return new FixedDiagnosticIdentity(kind, bytes)
}

export function createProtocolSessionIdentity(
  bytes: Uint8Array,
): ProtocolSessionIdentity {
  return createDiagnosticIdentity('protocol_session', bytes)
}

export function createProtocolOperationIdentity(
  bytes: Uint8Array,
): ProtocolOperationIdentity {
  return createDiagnosticIdentity('protocol_operation', bytes)
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
  if (left.kind !== right.kind || left.byteLength !== right.byteLength) return false
  const leftBytes = left.copyBytes()
  const rightBytes = right.copyBytes()
  if (
    leftBytes.byteLength !== DIAGNOSTIC_IDENTITY_BYTES ||
    rightBytes.byteLength !== DIAGNOSTIC_IDENTITY_BYTES
  ) return false
  return leftBytes.every((value, index) => value === rightBytes[index])
}
