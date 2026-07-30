import { readFile } from 'node:fs/promises'

import {
  sameNetworkMatrixControlAuthority,
  sameNetworkMatrixSampleAuthority,
  type NetworkMatrixControlAuthority,
} from '../../sample-authority.ts'
import type {
  ExternalFixtureControlCredentialReceipt,
  ExternalFixtureControlCredentialRetirementReceipt,
} from '../control-credential.ts'
import {
  authenticateSignedExternalFixtureControlCredentialLease,
  authenticateSignedExternalFixtureControlCredentialReceipt,
  parseSignedExternalFixtureControlCredentialLease,
  parseSignedExternalFixtureControlCredentialReceipt,
} from '../control-credential.ts'
import type {
  AuthenticatedCredentialLease,
  CredentialBrokerScope,
  CredentialBrokerSecretStore,
} from './contracts.ts'
import {
  invalidBrokerResponse,
  MAXIMUM_BROKER_FRAME_BYTES,
  parseBrokerPipeFrame,
} from './pipe-protocol.ts'
import { adoptCredentialSecret } from './secret-store.ts'

export interface CredentialRetirementAuthenticationContext {
  readonly controlAuthority: NetworkMatrixControlAuthority
  readonly scope: CredentialBrokerScope
  readonly authorityInstanceId: string
  readonly attestationSha256: string
  readonly leaseExpiresAt: string
  readonly releaseRequestId: string
  readonly revokeRequestId: string
}

export async function readPinnedPublicKey(path: string, signal: AbortSignal): Promise<string> {
  const bytes = await readFile(path, { signal })
  if (bytes.byteLength === 0 || bytes.byteLength > MAXIMUM_BROKER_FRAME_BYTES) {
    throw new Error('credential broker public key authority is invalid')
  }
  return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
}

export function authenticateLeaseResponse(
  bytes: Buffer,
  publicKey: string,
  secretStore: CredentialBrokerSecretStore | undefined,
): AuthenticatedCredentialLease {
  const frame = parseBrokerPipeFrame(bytes)
  const signedLease = parseSignedExternalFixtureControlCredentialLease(frame.metadata)
  const lease = authenticateSignedExternalFixtureControlCredentialLease(signedLease, publicKey)
  const credential = adoptCredentialSecret({
    payload: frame.payload,
    declaredByteLength: lease.credentialByteLength,
    secretStore,
  })
  return Object.freeze({ ...lease, signedLease, credential })
}

export function authenticateRetirementResponse(input: {
  readonly bytes: Buffer
  readonly publicKey: string
  readonly operation: ExternalFixtureControlCredentialReceipt['operation']
  readonly requestId: string
  readonly retirement: CredentialRetirementAuthenticationContext
}): ExternalFixtureControlCredentialRetirementReceipt {
  const frame = parseBrokerPipeFrame(input.bytes)
  if (frame.payload.byteLength !== 0) invalidBrokerResponse()
  const signedReceipt = parseSignedExternalFixtureControlCredentialReceipt(frame.metadata)
  const receipt = authenticateSignedExternalFixtureControlCredentialReceipt(
    signedReceipt,
    input.publicKey,
  )
  const retirement = input.retirement
  if (
    receipt.operation !== input.operation || receipt.requestId !== input.requestId ||
    receipt.releaseRequestId !== retirement.releaseRequestId ||
    receipt.revokeRequestId !== retirement.revokeRequestId ||
    !sameNetworkMatrixControlAuthority(receipt.controlAuthority, retirement.controlAuthority) ||
    !sameNetworkMatrixSampleAuthority(
      receipt.controlAuthority.sampleAuthority,
      retirement.scope.sampleAuthority,
    ) ||
    receipt.probeNonce !== retirement.scope.probeNonce ||
    receipt.authorityInstanceId !== retirement.authorityInstanceId ||
    receipt.attestationSha256 !== retirement.attestationSha256 ||
    receipt.leaseExpiresAt !== retirement.leaseExpiresAt
  ) invalidBrokerResponse()
  return Object.freeze({ receipt, signedReceipt })
}
