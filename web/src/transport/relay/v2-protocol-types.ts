import type {
  V2ChallengePurpose,
  V2RegistrationMode,
  V2RelayErrorCode,
} from './v2-protocol'

export interface V2RegisterInit {
  readonly mode: V2RegistrationMode
  readonly shareId: Uint8Array
  readonly shareInstance: Uint8Array
  readonly pkHash: Uint8Array
  readonly descriptorDigest: Uint8Array
  readonly resumeTokenHash: Uint8Array
}

export interface V2Challenge {
  readonly purpose: V2ChallengePurpose
  readonly id: Uint8Array
  readonly nonce: Uint8Array
  readonly expiresAtUnixSeconds: bigint
}

export interface V2RegisterProof {
  readonly mode: V2RegistrationMode
  readonly senderPublicKey: Uint8Array
  readonly signature: Uint8Array
}

export interface V2StopInit {
  readonly shareId: Uint8Array
  readonly shareInstance: Uint8Array
  readonly pkHash: Uint8Array
  readonly relayIdentity: Uint8Array
  readonly stopId: Uint8Array
}

export interface V2StopProof {
  readonly senderPublicKey: Uint8Array
  readonly signature: Uint8Array
}

export interface V2Registered {
  readonly shareId: Uint8Array
  readonly shareInstance: Uint8Array
  readonly descriptorDigest: Uint8Array
}

export interface V2DescriptorDelivery {
  readonly relaySessionId: Uint8Array
  readonly object: Uint8Array
}

export interface V2OpaqueRoute {
  readonly relaySessionId: Uint8Array
  readonly ciphertext: Uint8Array
}

export interface V2SessionRetired {
  readonly relaySessionId: Uint8Array
}

export interface V2RelayErrorFrame {
  readonly code: V2RelayErrorCode
  readonly retryAfterMilliseconds: number
}
