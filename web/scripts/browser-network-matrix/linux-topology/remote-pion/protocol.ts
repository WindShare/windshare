import { networkInterfaces } from 'node:os'

import type {
  ExternalFixtureProbeResult,
} from '../../runtime-authority.ts'
import {
  networkMatrixAttemptAuthoritySha256,
  parseNetworkMatrixAttemptAuthority,
  parseNetworkMatrixControlAuthority,
  parseNetworkMatrixFixtureAuthorityBinding,
  sameNetworkMatrixAttemptAuthority,
  sameNetworkMatrixControlAuthority,
  type NetworkMatrixAttemptAuthority,
  type NetworkMatrixControlAuthority,
  type NetworkMatrixFixtureAuthorityBinding,
} from '../../sample-authority.ts'
import {
  ExternalFixtureAttestationError,
  isGlobalUnicastIpv4,
} from '../external-fixture-attestation.ts'
import {
  authenticateSignedExternalFixtureTerminalReceipt,
  parseSignedExternalFixtureTerminalReceipt,
  type SignedExternalFixtureTerminalReceipt,
} from '../external-fixture-terminal-receipt.ts'
import {
  REMOTE_PION_ATTESTATION_LEASE_MS,
  REMOTE_PION_PROTOCOL_VERSION,
  type LiveRemotePionAuthority,
  type RemotePionAttemptAuthorityState,
  type RemotePionAttemptResult,
  type RemotePionAuthorityBinding,
  type RemotePionControlLeaseBinding,
  type RemotePionHttpResponse,
} from './contracts.ts'
import {
  RemotePionAuthorityExpiredError,
  RemotePionOperationAbortedError,
  RemotePionProtocolError,
  RemotePionTransportUnavailableError,
  RemotePionUnexpectedStatusError,
} from './errors.ts'

const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const OPAQUE_ID_PATTERN = /^[A-Za-z0-9_-]{16,128}$/u
const CANONICAL_ID_PATTERN = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/u
const MINIMUM_CONTROL_CREDENTIAL_BYTES = 32
const MAXIMUM_CONTROL_CREDENTIAL_BYTES = 512
const CANONICAL_UTC_TIMESTAMP_PATTERN =
  /^\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d\.\d{3}Z$/u

export function parseRemotePionAttemptResult(
  value: Record<string, unknown>,
  authority: RemotePionAttemptAuthorityState,
  attestationPublicKey: string | Buffer,
): RemotePionAttemptResult {
  requireExactKeys(value, [
    'protocolVersion', 'attemptAuthority', 'state',
    'selectedPair', 'challengeBindingSha256', 'failureCode', 'terminalReceipt',
  ])
  const responseAuthority = parseAttemptAuthority(value.attemptAuthority)
  if (value.protocolVersion !== REMOTE_PION_PROTOCOL_VERSION) {
    throw protocolInvalid('remote Pion result is not bound to its attempt')
  }
  if (!sameNetworkMatrixAttemptAuthority(responseAuthority, authority.attemptAuthority)) {
    throw new RemotePionProtocolError(
      'authority-binding-mismatch',
      'remote Pion result crossed its issued attempt authority',
    )
  }
  const state = parseRemotePionResultState(value.state)
  const failureCode = parseRemotePionFailureCode(value.failureCode)
  const challengeBindingSha256 = stringField(value, 'challengeBindingSha256')
  if (
    !SHA256_PATTERN.test(challengeBindingSha256) ||
    challengeBindingSha256 !== remotePionChallengeBindingSha256(authority.attemptAuthority)
  ) throw protocolInvalid('remote Pion result is not bound to its application challenge')
  const terminalReceipt = value.terminalReceipt === null
    ? null
    : parseSignedExternalFixtureTerminalReceipt(value.terminalReceipt)
  const selectedPair = value.selectedPair ?? null
  requireValidRemotePionTerminalReceipt({
    authority,
    attestationPublicKey,
    state,
    selectedPair,
    challengeBindingSha256,
    failureCode,
    terminalReceipt,
  })
  return Object.freeze({
    attemptAuthority: authority.attemptAuthority,
    state,
    selectedPair,
    challengeBindingSha256,
    failureCode,
    terminalReceipt,
  })
}

function parseRemotePionResultState(value: unknown): RemotePionAttemptResult['state'] {
  if (value !== 'pending' && value !== 'established' && value !== 'failed') {
    throw protocolInvalid('remote Pion result state is invalid')
  }
  return value
}

function parseRemotePionFailureCode(value: unknown): string | null {
  if (value !== null && typeof value !== 'string') {
    throw protocolInvalid('remote Pion result failure is invalid')
  }
  return value
}

function requireValidRemotePionTerminalReceipt(input: {
  readonly authority: RemotePionAttemptAuthorityState
  readonly attestationPublicKey: string | Buffer
  readonly state: RemotePionAttemptResult['state']
  readonly selectedPair: unknown | null
  readonly challengeBindingSha256: string
  readonly failureCode: string | null
  readonly terminalReceipt: SignedExternalFixtureTerminalReceipt | null
}): void {
  if (input.state === 'pending') {
    if (input.terminalReceipt !== null) {
      throw protocolInvalid('pending remote Pion result cannot carry a terminal receipt')
    }
    return
  }
  if (input.terminalReceipt === null) {
    throw protocolInvalid('terminal remote Pion result lacks a signed receipt')
  }
  const receipt = authenticateSignedExternalFixtureTerminalReceipt(
    input.terminalReceipt,
    input.attestationPublicKey,
  )
  const terminalAt = canonicalUtcTimestamp(receipt.terminalAt)
  const attemptIssuedAt = canonicalUtcTimestamp(receipt.attemptLeaseIssuedAt)
  const attemptExpiresAt = canonicalUtcTimestamp(receipt.attemptLeaseExpiresAt)
  if (
    !sameNetworkMatrixAttemptAuthority(
      receipt.attemptAuthority,
      input.authority.attemptAuthority,
    ) || receipt.state !== input.state ||
    receipt.challengeBindingSha256 !== input.challengeBindingSha256 ||
    receipt.failureCode !== input.failureCode ||
    JSON.stringify(receipt.selectedPair) !== JSON.stringify(input.selectedPair) ||
    receipt.attemptLeaseIssuedAt !== input.authority.leaseIssuedAt ||
    receipt.attemptLeaseExpiresAt !== input.authority.leaseExpiresAt ||
    receipt.attemptLeaseMillis !== input.authority.leaseMillis ||
    attemptExpiresAt - attemptIssuedAt !== receipt.attemptLeaseMillis ||
    terminalAt < attemptIssuedAt || terminalAt >= attemptExpiresAt ||
    terminalAt < Date.parse(input.authority.authorityIssuedAt) ||
    terminalAt >= Date.parse(input.authority.authorityExpiresAt)
  ) throw protocolInvalid('remote Pion terminal receipt crossed its signed attempt authority')
}

export function fixtureProbeResult(authority: LiveRemotePionAuthority): Extract<
  ExternalFixtureProbeResult,
  { readonly outcome: 'satisfied' }
> {
  if (authority.rtcConfiguration === null) {
    throw new Error('remote Pion run authority is not ready')
  }
  return Object.freeze({
    outcome: 'satisfied',
    probeId: `probe-${authority.fixtureBinding.attestationSha256}`,
    runId: authority.controlAuthority.sampleAuthority.runId,
    profileId: authority.profileId,
    authorityInstanceId: authority.authorityInstanceId,
    remoteServiceInstanceId: authority.fixtureBinding.remoteServiceInstanceId,
    attestationSha256: authority.fixtureBinding.attestationSha256,
    attestationPublicKeySpki: authority.attestationPublicKeySpki,
    signedAttestation: authority.signedAttestation,
    networkBindingSha256: authority.fixtureBinding.networkBindingSha256,
    remotePeerBindingSha256: authority.fixtureBinding.remotePeerBindingSha256,
    controllerPublicIp: authority.controllerPublicIp,
    leaseExpiresAt: authority.expiresAt,
    rtcConfiguration: authority.rtcConfiguration,
  })
}

export function classifyProbeFailure(
  cause: unknown,
  signal: AbortSignal,
): ExternalFixtureProbeResult {
  if (signal.aborted || cause instanceof RemotePionOperationAbortedError) {
    return Object.freeze({ outcome: 'failed', failureCode: 'authority-probe-failed' })
  }
  if (cause instanceof ExternalFixtureAttestationError) {
    return cause.failureCode === 'authority-attestation-expired'
      ? Object.freeze({ outcome: 'unavailable', failureCode: cause.failureCode })
      : Object.freeze({ outcome: 'invalid', failureCode: cause.failureCode })
  }
  if (cause instanceof RemotePionProtocolError) {
    return Object.freeze({ outcome: 'invalid', failureCode: cause.failureCode })
  }
  if (cause instanceof RemotePionUnexpectedStatusError) {
    if (cause.statusCode === 410) {
      return Object.freeze({
        outcome: 'unavailable',
        failureCode: 'authority-attestation-expired',
      })
    }
    if (cause.statusCode === 428) {
      return Object.freeze({
        outcome: 'unavailable',
        failureCode: 'authority-key-rotation-required',
      })
    }
    if (cause.statusCode === 409) {
      return Object.freeze({ outcome: 'invalid', failureCode: 'authority-binding-mismatch' })
    }
    return Object.freeze({ outcome: 'failed', failureCode: 'authority-probe-failed' })
  }
  return cause instanceof RemotePionTransportUnavailableError
    ? Object.freeze({ outcome: 'unavailable', failureCode: 'authority-unreachable' })
    : Object.freeze({ outcome: 'failed', failureCode: 'authority-probe-failed' })
}

export function remotePionChallengeBindingSha256(
  authority: NetworkMatrixAttemptAuthority,
): string {
  return networkMatrixAttemptAuthoritySha256(parseAttemptAuthority(authority))
}

export function attemptBinding(
  value: RemotePionAuthorityBinding,
): RemotePionAuthorityBinding {
  return {
    controlAuthority: value.controlAuthority,
    fixtureBinding: value.fixtureBinding,
  }
}

export function authorityBindingFromAttempt(
  authority: NetworkMatrixAttemptAuthority,
): RemotePionAuthorityBinding {
  return Object.freeze({
    controlAuthority: authority.requestAuthority.controlAuthority,
    fixtureBinding: authority.requestAuthority.fixtureBinding,
  })
}

export function parseAttemptAuthority(value: unknown): NetworkMatrixAttemptAuthority {
  try {
    return parseNetworkMatrixAttemptAuthority(value)
  } catch {
    throw protocolInvalid('remote Pion attempt authority is invalid')
  }
}

function parseControlAuthority(value: unknown): NetworkMatrixControlAuthority {
  try {
    return parseNetworkMatrixControlAuthority(value)
  } catch {
    throw protocolInvalid('remote Pion control authority is invalid')
  }
}

function parseFixtureBinding(value: unknown): NetworkMatrixFixtureAuthorityBinding {
  try {
    return parseNetworkMatrixFixtureAuthorityBinding(value)
  } catch {
    throw protocolInvalid('remote Pion fixture authority binding is invalid')
  }
}

export function sameAuthorityBinding(
  left: RemotePionAuthorityBinding,
  right: RemotePionAuthorityBinding,
): boolean {
  return sameNetworkMatrixControlAuthority(left.controlAuthority, right.controlAuthority) &&
    JSON.stringify(left.fixtureBinding) === JSON.stringify(right.fixtureBinding)
}

export function requireResponseBinding(
  value: Record<string, unknown>,
  expected: RemotePionAuthorityBinding,
): void {
  if (
    !sameNetworkMatrixControlAuthority(
      parseControlAuthority(value.controlAuthority),
      expected.controlAuthority,
    ) ||
    JSON.stringify(parseFixtureBinding(value.fixtureBinding)) !==
      JSON.stringify(expected.fixtureBinding)
  ) throw new RemotePionProtocolError(
    'authority-binding-mismatch',
    'remote Pion response differs from its signed authority binding',
  )
}

export function requireRemotePionControllerAuthority(
  authorities: ReadonlyMap<string, LiveRemotePionAuthority>,
  authority: RemotePionAuthorityBinding | undefined,
  observedRemoteAddress: string,
): void {
  if (authority === undefined) return
  const runId = authority.controlAuthority.sampleAuthority.runId
  const live = authorities.get(runId)
  if (
    live === undefined || live.controllerPublicIp !== observedRemoteAddress ||
    !sameAuthorityBinding(live, authority)
  ) throw new RemotePionProtocolError(
    'authority-binding-mismatch',
    'remote Pion request crossed its attested controller authority',
  )
}

export function requireObservedControllerAddress(
  response: Pick<
    RemotePionHttpResponse,
    'observedRemoteAddress' | 'observedTlsCertificateSha256'
  >,
  expectedCertificateSha256: string,
  localAddresses: readonly string[],
): string {
  const observed = normalizeObservedIpv4(response.observedRemoteAddress)
  if (
    observed === null || !isGlobalUnicastIpv4(observed) ||
    response.observedTlsCertificateSha256 !== expectedCertificateSha256 ||
    localAddresses.map(normalizeObservedIpv4).some((address) => address === observed)
  ) throw new RemotePionProtocolError(
    'authority-binding-mismatch',
    'remote Pion socket is not an independently hosted pinned controller',
  )
  return observed
}

function normalizeObservedIpv4(value: string): string | null {
  const candidate = value.toLowerCase().startsWith('::ffff:') ? value.slice(7) : value
  return isGlobalUnicastIpv4(candidate) || /^\d{1,3}(?:\.\d{1,3}){3}$/u.test(candidate)
    ? candidate
    : null
}

export function systemInterfaceAddresses(): readonly string[] {
  return Object.freeze(Object.values(networkInterfaces()).flatMap((entries) =>
    (entries ?? []).map((entry) => entry.address)))
}

export function parseCanonicalObject(encoded: string): Record<string, unknown> {
  let value: unknown
  try {
    value = JSON.parse(encoded)
  } catch {
    throw protocolInvalid('remote Pion response is not canonical JSON')
  }
  if (
    typeof value !== 'object' || value === null || Array.isArray(value) ||
    encoded !== `${JSON.stringify(value)}\n`
  ) throw protocolInvalid('remote Pion response is not a canonical protocol object')
  return value as Record<string, unknown>
}

export function stringField(value: Record<string, unknown>, name: string): string {
  const field = value[name]
  if (typeof field !== 'string' || field.includes('\0')) {
    throw protocolInvalid('remote Pion response contains an invalid string field')
  }
  return field
}

export function canonicalUtcTimestamp(value: string): number {
  if (!CANONICAL_UTC_TIMESTAMP_PATTERN.test(value)) {
    throw protocolInvalid('remote Pion lease expiry is not canonical UTC')
  }
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp) || new Date(timestamp).toISOString() !== value) {
    throw protocolInvalid('remote Pion lease expiry is not a real UTC instant')
  }
  return timestamp
}

export function requireExactKeys(
  value: Record<string, unknown>,
  expectedKeys: readonly string[],
): void {
  const actual = Object.keys(value)
  if (
    actual.length !== expectedKeys.length ||
    actual.some((key, index) => key !== expectedKeys[index])
  ) throw protocolInvalid('remote Pion response contains fields outside its protocol authority')
}

export function requireAttemptId(value: string): void {
  if (!OPAQUE_ID_PATTERN.test(value)) throw new Error('remote Pion attempt ID is invalid')
}

export function validOpaqueId(value: string): boolean {
  return OPAQUE_ID_PATTERN.test(value)
}

export function protocolInvalid(message: string): RemotePionProtocolError {
  return new RemotePionProtocolError('proof-invalid', message)
}

export function isControlCredentialBytes(value: unknown): value is Uint8Array {
  if (
    !(value instanceof Uint8Array) ||
    value.byteLength < MINIMUM_CONTROL_CREDENTIAL_BYTES ||
    value.byteLength > MAXIMUM_CONTROL_CREDENTIAL_BYTES
  ) return false
  for (const byte of value) {
    const alphaNumeric = byte >= 0x30 && byte <= 0x39 ||
      byte >= 0x41 && byte <= 0x5a || byte >= 0x61 && byte <= 0x7a
    if (!alphaNumeric && byte !== 0x2d && byte !== 0x5f) return false
  }
  return true
}

export function validControlLeaseBinding(value: RemotePionControlLeaseBinding): boolean {
  if (
    typeof value !== 'object' || value === null ||
    !OPAQUE_ID_PATTERN.test(value.probeNonce) ||
    !CANONICAL_ID_PATTERN.test(value.authorityInstanceId) ||
    !SHA256_PATTERN.test(value.attestationSha256) || value.maxAttempts !== 1
  ) return false
  try {
    parseNetworkMatrixControlAuthority(value.controlAuthority)
    const issuedAt = canonicalUtcTimestamp(value.issuedAt)
    const expiresAt = canonicalUtcTimestamp(value.expiresAt)
    return expiresAt > issuedAt && expiresAt - issuedAt <= REMOTE_PION_ATTESTATION_LEASE_MS
  } catch {
    return false
  }
}

export function requireRemotePionStatus(
  statusCode: number,
  expectedStatus: number | readonly number[],
  authorityBound: boolean,
): void {
  const accepted = Array.isArray(expectedStatus)
    ? expectedStatus.includes(statusCode)
    : statusCode === expectedStatus
  if (accepted) return
  if (authorityBound && statusCode === 410) throw new RemotePionAuthorityExpiredError()
  throw new RemotePionUnexpectedStatusError(statusCode)
}
