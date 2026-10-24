import type {
  ChallengePurpose,
  RegistrationMode,
  RelayErrorCode,
} from './protocol'

export interface RegisterInit {
  readonly mode: RegistrationMode
  readonly shareId: Uint8Array
  readonly shareInstance: Uint8Array
  readonly pkHash: Uint8Array
  readonly descriptorDigest: Uint8Array
  readonly resumeTokenHash: Uint8Array
}

export interface Challenge {
  readonly purpose: ChallengePurpose
  readonly id: Uint8Array
  readonly nonce: Uint8Array
  readonly expiresAtUnixSeconds: bigint
}

export interface RegisterProof {
  readonly mode: RegistrationMode
  readonly senderPublicKey: Uint8Array
  readonly signature: Uint8Array
}

export interface StopInit {
  readonly shareId: Uint8Array
  readonly shareInstance: Uint8Array
  readonly pkHash: Uint8Array
  readonly relayIdentity: Uint8Array
  readonly stopId: Uint8Array
}

export interface StopProof {
  readonly senderPublicKey: Uint8Array
  readonly signature: Uint8Array
}

export interface Registered {
  readonly shareId: Uint8Array
  readonly shareInstance: Uint8Array
  readonly descriptorDigest: Uint8Array
}

export interface DescriptorDelivery {
  readonly relaySessionId: Uint8Array
  readonly object: Uint8Array
}

export interface OpaqueRoute {
  readonly relaySessionId: Uint8Array
  readonly ciphertext: Uint8Array
}

export interface SessionRetired {
  readonly relaySessionId: Uint8Array
}

export interface SessionAdmitted {
  readonly relaySessionId: Uint8Array
}

export interface SessionCredit {
  readonly relaySessionId: Uint8Array
  readonly frames: number
  readonly bytes: number
}

export interface RelayErrorFrame {
  readonly code: RelayErrorCode
  readonly retryAfterMilliseconds: number
}
