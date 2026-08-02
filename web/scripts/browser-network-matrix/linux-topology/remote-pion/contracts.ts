import type { NetworkMatrixRtcConfiguration } from '../../runtime-authority.ts'
import type { NetworkMatrixProfileId } from '../../vocabulary.ts'
import type {
  NetworkMatrixAttemptAuthority,
  NetworkMatrixControlAuthority,
  NetworkMatrixFixtureAuthorityBinding,
} from '../../sample-authority.ts'
import type {
  SignedExternalFixtureAttestation,
  VerifiedExternalFixtureAuthority,
} from '../external-fixture-attestation.ts'
import {
  EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
} from '../external-fixture-attestation.ts'
import {
  EXTERNAL_FIXTURE_MAXIMUM_ATTEMPT_LEASE_MS,
  type SignedExternalFixtureTerminalReceipt,
} from '../external-fixture-terminal-receipt.ts'

export const REMOTE_PION_PROTOCOL_VERSION = EXTERNAL_FIXTURE_CONTROL_PROTOCOL
export const REMOTE_PION_REQUEST_DEADLINE_MS = 15_000
export const REMOTE_PION_ATTESTATION_LEASE_MS = 120_000
export const REMOTE_PION_MAXIMUM_ATTEMPT_LEASE_MS =
  EXTERNAL_FIXTURE_MAXIMUM_ATTEMPT_LEASE_MS

export interface RemotePionControlOptions {
  readonly controllerOrigin: string
  readonly tlsCertificateAuthority: string | Buffer
  readonly tlsCertificateSha256: string
  readonly attestationPublicKey: string | Buffer
  /** The caller retains the only alias and erases these bytes when the lease retires. */
  readonly controlCredential: Uint8Array
  readonly controlLease: RemotePionControlLeaseBinding
  readonly request?: RemotePionRequest
  readonly now?: () => number
  readonly localInterfaceAddresses?: () => readonly string[]
}

export interface RemotePionControlLeaseBinding {
  readonly controlAuthority: NetworkMatrixControlAuthority
  readonly probeNonce: string
  readonly authorityInstanceId: string
  readonly attestationSha256: string
  readonly issuedAt: string
  readonly expiresAt: string
  readonly maxAttempts: 1
}

export interface RemotePionHttpRequest {
  readonly endpoint: URL
  readonly path: string
  readonly body: string | null
  readonly method: 'GET' | 'POST' | 'DELETE'
  readonly controlCredential: Uint8Array
  readonly controlLeaseId: string
  readonly tlsServerName: string
  readonly tlsCertificateAuthority: string | Buffer
  readonly tlsCertificateSha256: string
  readonly signal: AbortSignal
}

export interface RemotePionHttpResponse {
  readonly statusCode: number
  readonly body: string
  readonly observedRemoteAddress: string
  readonly observedTlsCertificateSha256: string
}

export type RemotePionRequest = (
  request: RemotePionHttpRequest,
) => Promise<RemotePionHttpResponse>

export interface RemotePionAuthorityBinding {
  readonly controlAuthority: NetworkMatrixControlAuthority
  readonly fixtureBinding: NetworkMatrixFixtureAuthorityBinding
}

export interface RemotePionAttemptLease {
  readonly attemptAuthority: NetworkMatrixAttemptAuthority
  readonly leaseIssuedAt: string
  readonly leaseExpiresAt: string
  readonly leaseMillis: number
}

export interface RemotePionAttemptResult {
  readonly attemptAuthority: NetworkMatrixAttemptAuthority
  readonly state: 'pending' | 'established' | 'failed'
  readonly selectedPair: unknown | null
  readonly challengeBindingSha256: string
  readonly failureCode: string | null
  readonly terminalReceipt: SignedExternalFixtureTerminalReceipt | null
}

export interface LiveRemotePionAuthority extends RemotePionAuthorityBinding {
  readonly profileId: NetworkMatrixProfileId
  readonly authorityInstanceId: string
  readonly controllerPublicIp: string
  readonly issuedAt: string
  readonly expiresAt: string
  readonly verified: VerifiedExternalFixtureAuthority
  readonly rtcConfiguration: NetworkMatrixRtcConfiguration | null
  readonly attestationPublicKeySpki: string
  readonly signedAttestation: SignedExternalFixtureAttestation
}

export interface RemotePionAttemptAuthorityState extends RemotePionAuthorityBinding {
  readonly profileId: NetworkMatrixProfileId
  readonly attemptAuthority: NetworkMatrixAttemptAuthority
  readonly leaseIssuedAt: string
  readonly leaseExpiresAt: string
  readonly leaseMillis: number
  readonly authorityIssuedAt: string
  readonly authorityExpiresAt: string
}

export interface RemotePionCallResult {
  readonly value: Record<string, unknown>
  readonly observedRemoteAddress: string
  readonly observedTlsCertificateSha256: string
}
