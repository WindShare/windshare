import { createHash, createPublicKey, verify } from 'node:crypto'
import { isIP } from 'node:net'

import { validNetworkMatrixIceUri } from '../ice-uri.ts'
import {
  parseNetworkMatrixControlAuthority,
  parseNetworkMatrixFixtureAuthorityBinding,
  sameNetworkMatrixControlAuthority,
  type NetworkMatrixControlAuthority,
  type NetworkMatrixFixtureAuthorityBinding,
} from '../sample-authority.ts'
import type { NetworkMatrixProfileId } from '../vocabulary.ts'

export const EXTERNAL_FIXTURE_DECLARATION_SCHEMA =
  'windshare.browser-network-matrix.external-fixture-declaration/v1' as const
export const EXTERNAL_FIXTURE_ATTESTATION_SCHEMA =
  'windshare.browser-network-matrix.external-fixture-attestation/v1' as const
export const EXTERNAL_REMOTE_PEER_BINDING_SCHEMA =
  'windshare.browser-network-matrix.external-remote-peer-binding/v1' as const
export const EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM = 'ed25519' as const
export const EXTERNAL_FIXTURE_CONTROL_PROTOCOL =
  'windshare.browser-network-matrix.remote-pion/v3' as const

const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const CANONICAL_ID_PATTERN = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/u
const OPAQUE_ID_PATTERN = /^[A-Za-z0-9_-]{16,128}$/u
const SIGNATURE_PATTERN = /^[A-Za-z0-9_-]{86}$/u
const RFC3339_UTC_PATTERN =
  /^\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d\.\d{3}Z$/u
const MAXIMUM_ATTESTATION_LEASE_MS = 300_000
const MAXIMUM_CLOCK_SKEW_MS = 30_000

interface ExternalFixtureNetworkSemanticsBase {
  readonly kind: 'public-stun' | 'restricted-udp' | 'coturn-relay' | 'operator-real-nat'
  readonly policyId: string
  readonly policyVersion: number
}

export interface PublicStunNetworkSemantics extends ExternalFixtureNetworkSemanticsBase {
  readonly kind: 'public-stun'
  readonly stunEndpoint: string
}

export interface RestrictedUdpNetworkSemantics extends ExternalFixtureNetworkSemanticsBase {
  readonly kind: 'restricted-udp'
  readonly outboundUdp: 'denied'
  readonly unsolicitedInboundUdp: 'denied'
  readonly relayAccess: 'denied'
}

export interface CoturnRelayNetworkSemantics extends ExternalFixtureNetworkSemanticsBase {
  readonly kind: 'coturn-relay'
  readonly turnServiceOwnerId: string
  readonly turnUrls: readonly string[]
  readonly turnUsername: string
  readonly turnCredentialId: string
  readonly turnCredentialExpiresAt: string
}

export interface OperatorRealNatNetworkSemantics extends ExternalFixtureNetworkSemanticsBase {
  readonly kind: 'operator-real-nat'
  readonly senderHostId: string
  readonly senderNetworkBoundaryId: string
  readonly stunEndpoint: string
}

export type ExternalFixtureNetworkSemantics =
  | PublicStunNetworkSemantics
  | RestrictedUdpNetworkSemantics
  | CoturnRelayNetworkSemantics
  | OperatorRealNatNetworkSemantics

export interface ExternalFixtureDeclaration {
  readonly schemaVersion: typeof EXTERNAL_FIXTURE_DECLARATION_SCHEMA
  readonly deploymentId: string
  readonly revision: number
  readonly profileId: NetworkMatrixProfileId
  readonly authorityInstanceId: string
  readonly implementationSha256: string
  readonly remoteServiceInstanceId: string
  readonly operatorId: string
  readonly fixtureHostId: string
  readonly fixtureNetworkBoundaryId: string
  readonly controllerOrigin: string
  readonly controllerPublicIp: string
  readonly tlsCertificateSha256: string
  readonly remotePeerPublicIp: string
  readonly remotePeerUdpPortMin: number
  readonly remotePeerUdpPortMax: number
  readonly networkSemantics: ExternalFixtureNetworkSemantics
}

export interface ExternalFixtureAttestation {
  readonly schemaVersion: typeof EXTERNAL_FIXTURE_ATTESTATION_SCHEMA
  readonly runId: string
  readonly nonce: string
  readonly leaseId: string
  readonly leaseMillis: number
  readonly issuedAt: string
  readonly expiresAt: string
  readonly fixture: ExternalFixtureDeclaration
}

export interface SignedExternalFixtureAttestation {
  readonly protocolVersion: string
  readonly attestation: ExternalFixtureAttestation
  readonly attestationSha256: string
  readonly signatureAlgorithm: typeof EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM
  readonly signature: string
}

export interface VerifiedExternalFixtureAuthority {
  readonly attestation: ExternalFixtureAttestation
  readonly attestationSha256: string
  readonly networkBindingSha256: string
  readonly remotePeerBindingSha256: string
  readonly activeStunEndpoint: string | null
}

export interface ExternalFixtureTurnCredential {
  readonly credentialId: string
  readonly expiresAt: string
  readonly username: string
  readonly credential: string
}

export interface ManualOperatorTopologyIdentity {
  readonly senderHostId: string
  readonly senderNetworkBoundaryId: string
}

export interface ExternalFixtureTurnCredentialContext {
  readonly protocolVersion: string
  readonly controlAuthority: NetworkMatrixControlAuthority
  readonly fixtureBinding: NetworkMatrixFixtureAuthorityBinding
  readonly fixture: ExternalFixtureDeclaration
  readonly now?: () => number
}

export interface ExternalFixtureAttestationVerification {
  readonly protocolVersion: string
  readonly profileId: NetworkMatrixProfileId
  readonly runId: string
  readonly nonce: string
  readonly requestedLeaseMillis: number
  readonly controllerOrigin: string
  readonly tlsCertificateSha256: string
  readonly observedControllerIp: string
  readonly observedTlsCertificateSha256: string
  readonly attestationPublicKey: string | Buffer
  readonly manualOperatorIdentity?: ManualOperatorTopologyIdentity
  readonly now?: () => number
}

export function parseSignedExternalFixtureAttestation(
  value: unknown,
  expectedProtocolVersion: string,
): SignedExternalFixtureAttestation {
  const response = exactRecord(value, [
    'protocolVersion', 'attestation', 'attestationSha256', 'signatureAlgorithm', 'signature',
  ])
  if (response.protocolVersion !== expectedProtocolVersion) invalidAttestation()
  return Object.freeze({
    protocolVersion: expectedProtocolVersion,
    attestation: parseExternalFixtureAttestation(response.attestation),
    attestationSha256: requireSha256(response.attestationSha256),
    signatureAlgorithm: requireLiteral(
      response.signatureAlgorithm,
      EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM,
    ),
    signature: requireSignature(response.signature),
  })
}

export function verifyExternalFixtureAttestation(
  signed: SignedExternalFixtureAttestation,
  expected: ExternalFixtureAttestationVerification,
): VerifiedExternalFixtureAuthority {
  const authenticated = authenticateSignedExternalFixtureAttestation(
    signed,
    expected.attestationPublicKey,
  )
  const attestation = authenticated.attestation
  const fixture = attestation.fixture
  if (
    signed.protocolVersion !== expected.protocolVersion ||
    attestation.runId !== expected.runId || attestation.nonce !== expected.nonce ||
    fixture.profileId !== expected.profileId ||
    fixture.controllerOrigin !== expected.controllerOrigin ||
    fixture.tlsCertificateSha256 !== expected.tlsCertificateSha256 ||
    fixture.controllerPublicIp !== expected.observedControllerIp ||
    fixture.tlsCertificateSha256 !== expected.observedTlsCertificateSha256
  ) invalidBinding()
  if (!isGlobalUnicastIpv4(expected.observedControllerIp)) invalidBinding()
  const issuedAt = Date.parse(attestation.issuedAt)
  const expiresAt = Date.parse(attestation.expiresAt)
  const now = (expected.now ?? Date.now)()
  if (
    attestation.leaseMillis > expected.requestedLeaseMillis ||
    expiresAt - issuedAt !== attestation.leaseMillis ||
    issuedAt > now + MAXIMUM_CLOCK_SKEW_MS || expiresAt <= now
  ) expiredAttestation()
  if (fixture.networkSemantics.kind === 'operator-real-nat') {
    if (
      expected.manualOperatorIdentity === undefined ||
      expected.manualOperatorIdentity.senderHostId !== fixture.networkSemantics.senderHostId ||
      expected.manualOperatorIdentity.senderNetworkBoundaryId !==
        fixture.networkSemantics.senderNetworkBoundaryId
    ) invalidBinding()
  }
  if (
    fixture.networkSemantics.kind === 'coturn-relay' &&
    fixture.networkSemantics.turnCredentialExpiresAt !== attestation.expiresAt
  ) expiredAttestation()
  return authenticated
}

export function authenticateSignedExternalFixtureAttestation(
  signed: SignedExternalFixtureAttestation,
  attestationPublicKey: string | Buffer,
): VerifiedExternalFixtureAuthority {
  const attestation = signed.attestation
  const fixture = attestation.fixture
  const canonical = canonicalExternalFixtureAttestationJson(attestation)
  const attestationSha256 = sha256(canonical)
  const networkBindingSha256 = externalFixtureNetworkBindingSha256(fixture)
  const remotePeerBindingSha256 = externalFixtureRemotePeerBindingSha256(fixture)
  if (signed.attestationSha256 !== attestationSha256) invalidBinding()
  let publicKey: ReturnType<typeof createPublicKey>
  try {
    publicKey = createPublicKey(attestationPublicKey)
  } catch {
    invalidSignature()
  }
  if (
    publicKey.asymmetricKeyType !== 'ed25519' ||
    !verify(null, Buffer.from(canonical), publicKey, Buffer.from(signed.signature, 'base64url'))
  ) invalidSignature()
  return Object.freeze({
    attestation,
    attestationSha256,
    networkBindingSha256,
    remotePeerBindingSha256,
    activeStunEndpoint: activeStunEndpointFromFixture(fixture),
  })
}

export function parseExternalFixtureTurnCredential(
  value: unknown,
  expected: ExternalFixtureTurnCredentialContext,
): ExternalFixtureTurnCredential {
  const semantics = expected.fixture.networkSemantics
  if (semantics.kind !== 'coturn-relay') invalidBinding()
  const response = exactRecord(value, [
    'protocolVersion', 'controlAuthority', 'fixtureBinding', 'credentialId', 'expiresAt',
    'username', 'credential',
  ])
  const controlAuthority = parseNetworkMatrixControlAuthority(response.controlAuthority)
  const fixtureBinding = parseNetworkMatrixFixtureAuthorityBinding(response.fixtureBinding)
  if (
    response.protocolVersion !== expected.protocolVersion ||
    !sameNetworkMatrixControlAuthority(controlAuthority, expected.controlAuthority) ||
    JSON.stringify(fixtureBinding) !== JSON.stringify(expected.fixtureBinding) ||
    response.credentialId !== semantics.turnCredentialId ||
    response.expiresAt !== semantics.turnCredentialExpiresAt ||
    response.username !== semantics.turnUsername
  ) invalidBinding()
  if (Date.parse(semantics.turnCredentialExpiresAt) <= (expected.now ?? Date.now)()) {
    expiredAttestation()
  }
  return Object.freeze({
    credentialId: requireCanonicalId(response.credentialId),
    expiresAt: requireUtcTimestamp(response.expiresAt),
    username: requireCredential(response.username),
    credential: requireCredential(response.credential),
  })
}

export function canonicalExternalFixtureAttestationJson(
  attestation: ExternalFixtureAttestation,
): string {
  return `${JSON.stringify(attestation)}\n`
}

export function externalFixtureNetworkBindingSha256(
  fixture: ExternalFixtureDeclaration,
): string {
  return sha256(`${JSON.stringify(fixture)}\n`)
}

export function externalFixtureRemotePeerBindingSha256(
  fixture: ExternalFixtureDeclaration,
): string {
  return sha256(`${JSON.stringify({
    schemaVersion: EXTERNAL_REMOTE_PEER_BINDING_SCHEMA,
    deploymentId: fixture.deploymentId,
    profileId: fixture.profileId,
    authorityInstanceId: fixture.authorityInstanceId,
    remoteServiceInstanceId: fixture.remoteServiceInstanceId,
    fixtureHostId: fixture.fixtureHostId,
    fixtureNetworkBoundaryId: fixture.fixtureNetworkBoundaryId,
    publicIp: fixture.remotePeerPublicIp,
    udpPortMin: fixture.remotePeerUdpPortMin,
    udpPortMax: fixture.remotePeerUdpPortMax,
  })}\n`)
}

export function isGlobalUnicastIpv4(value: unknown): value is string {
  if (typeof value !== 'string' || isIP(value) !== 4) return false
  const octets = value.split('.').map(Number)
  if (octets.some((octet) => !Number.isInteger(octet) || octet < 0 || octet > 255)) return false
  const [a = 0, b = 0, c = 0, d = 0] = octets
  return !(a === 0 || a === 10 || a === 127 || a >= 224 ||
    a === 100 && b >= 64 && b <= 127 ||
    a === 169 && b === 254 ||
    a === 172 && b >= 16 && b <= 31 ||
    a === 192 && b === 168 ||
    a === 192 && b === 0 && (c === 2 || c === 0 && d !== 9 && d !== 10) ||
    a === 192 && b === 88 && c === 99 ||
    a === 198 && (b === 18 || b === 19) ||
    a === 198 && b === 51 && c === 100 ||
    a === 203 && b === 0 && c === 113)
}

function parseExternalFixtureAttestation(value: unknown): ExternalFixtureAttestation {
  const attestation = exactRecord(value, [
    'schemaVersion', 'runId', 'nonce', 'leaseId', 'leaseMillis', 'issuedAt', 'expiresAt',
    'fixture',
  ])
  if (attestation.schemaVersion !== EXTERNAL_FIXTURE_ATTESTATION_SCHEMA) invalidAttestation()
  return Object.freeze({
    schemaVersion: EXTERNAL_FIXTURE_ATTESTATION_SCHEMA,
    runId: requireCanonicalId(attestation.runId),
    nonce: requireOpaqueId(attestation.nonce),
    leaseId: requireOpaqueId(attestation.leaseId),
    leaseMillis: requireLeaseMillis(attestation.leaseMillis),
    issuedAt: requireUtcTimestamp(attestation.issuedAt),
    expiresAt: requireUtcTimestamp(attestation.expiresAt),
    fixture: parseExternalFixtureDeclaration(attestation.fixture),
  })
}

function parseExternalFixtureDeclaration(value: unknown): ExternalFixtureDeclaration {
  const fixture = exactRecord(value, [
    'schemaVersion', 'deploymentId', 'revision', 'profileId', 'authorityInstanceId',
    'implementationSha256', 'remoteServiceInstanceId', 'operatorId', 'fixtureHostId',
    'fixtureNetworkBoundaryId', 'controllerOrigin', 'controllerPublicIp',
    'tlsCertificateSha256', 'remotePeerPublicIp', 'remotePeerUdpPortMin',
    'remotePeerUdpPortMax', 'networkSemantics',
  ])
  if (fixture.schemaVersion !== EXTERNAL_FIXTURE_DECLARATION_SCHEMA) invalidAttestation()
  const profileId = requireProfileId(fixture.profileId)
  const controllerOrigin = requireOriginOnlyHttps(fixture.controllerOrigin)
  const controllerPublicIp = requireGlobalIpv4(fixture.controllerPublicIp)
  const remotePeerPublicIp = requireGlobalIpv4(fixture.remotePeerPublicIp)
  const remotePeerUdpPortMin = requirePort(fixture.remotePeerUdpPortMin)
  const remotePeerUdpPortMax = requirePort(fixture.remotePeerUdpPortMax)
  if (remotePeerUdpPortMax < remotePeerUdpPortMin) invalidAttestation()
  const declaration = Object.freeze({
    schemaVersion: EXTERNAL_FIXTURE_DECLARATION_SCHEMA,
    deploymentId: requireCanonicalId(fixture.deploymentId),
    revision: requireRevision(fixture.revision),
    profileId,
    authorityInstanceId: requireCanonicalId(fixture.authorityInstanceId),
    implementationSha256: requireSha256(fixture.implementationSha256),
    remoteServiceInstanceId: requireCanonicalId(fixture.remoteServiceInstanceId),
    operatorId: requireCanonicalId(fixture.operatorId),
    fixtureHostId: requireCanonicalId(fixture.fixtureHostId),
    fixtureNetworkBoundaryId: requireCanonicalId(fixture.fixtureNetworkBoundaryId),
    controllerOrigin,
    controllerPublicIp,
    tlsCertificateSha256: requireSha256(fixture.tlsCertificateSha256),
    remotePeerPublicIp,
    remotePeerUdpPortMin,
    remotePeerUdpPortMax,
    networkSemantics: parseNetworkSemantics(fixture.networkSemantics, profileId),
  })
  const originHost = new URL(controllerOrigin).hostname.replace(/^\[|\]$/gu, '')
  if (isIP(originHost) !== 0 && originHost !== controllerPublicIp) invalidAttestation()
  if (
    declaration.networkSemantics.kind === 'operator-real-nat' && (
      declaration.networkSemantics.senderHostId === declaration.fixtureHostId ||
      declaration.networkSemantics.senderNetworkBoundaryId === declaration.fixtureNetworkBoundaryId
    )
  ) invalidAttestation()
  return declaration
}

function parseNetworkSemantics(
  value: unknown,
  profileId: NetworkMatrixProfileId,
): ExternalFixtureNetworkSemantics {
  if (profileId === 'scheduled-public-stun') {
    const semantics = exactRecord(value, ['kind', 'policyId', 'policyVersion', 'stunEndpoint'])
    if (semantics.kind !== 'public-stun') invalidAttestation()
    return Object.freeze({
      kind: 'public-stun',
      policyId: requireCanonicalId(semantics.policyId),
      policyVersion: requireRevision(semantics.policyVersion),
      stunEndpoint: requireIceUri(semantics.stunEndpoint, ['stun:']),
    })
  }
  if (profileId === 'scheduled-restricted-udp') {
    const semantics = exactRecord(value, [
      'kind', 'policyId', 'policyVersion', 'outboundUdp', 'unsolicitedInboundUdp',
      'relayAccess',
    ])
    if (
      semantics.kind !== 'restricted-udp' || semantics.outboundUdp !== 'denied' ||
      semantics.unsolicitedInboundUdp !== 'denied' || semantics.relayAccess !== 'denied'
    ) invalidAttestation()
    return Object.freeze({
      kind: 'restricted-udp',
      policyId: requireCanonicalId(semantics.policyId),
      policyVersion: requireRevision(semantics.policyVersion),
      outboundUdp: 'denied',
      unsolicitedInboundUdp: 'denied',
      relayAccess: 'denied',
    })
  }
  if (profileId === 'scheduled-coturn') {
    const semantics = exactRecord(value, [
      'kind', 'policyId', 'policyVersion', 'turnServiceOwnerId', 'turnUrls',
      'turnUsername', 'turnCredentialId', 'turnCredentialExpiresAt',
    ])
    if (semantics.kind !== 'coturn-relay' || !Array.isArray(semantics.turnUrls)) {
      invalidAttestation()
    }
    const turnUrls = Object.freeze(semantics.turnUrls.map((entry) =>
      requireIceUri(entry, ['turn:', 'turns:'])))
    if (turnUrls.length === 0 || new Set(turnUrls).size !== turnUrls.length) invalidAttestation()
    return Object.freeze({
      kind: 'coturn-relay',
      policyId: requireCanonicalId(semantics.policyId),
      policyVersion: requireRevision(semantics.policyVersion),
      turnServiceOwnerId: requireCanonicalId(semantics.turnServiceOwnerId),
      turnUrls,
      turnUsername: requireCredential(semantics.turnUsername),
      turnCredentialId: requireCanonicalId(semantics.turnCredentialId),
      turnCredentialExpiresAt: requireUtcTimestamp(semantics.turnCredentialExpiresAt),
    })
  }
  const semantics = exactRecord(value, [
    'kind', 'policyId', 'policyVersion', 'senderHostId', 'senderNetworkBoundaryId',
    'stunEndpoint',
  ])
  if (semantics.kind !== 'operator-real-nat') invalidAttestation()
  const senderHostId = requireCanonicalId(semantics.senderHostId)
  const senderNetworkBoundaryId = requireCanonicalId(semantics.senderNetworkBoundaryId)
  return Object.freeze({
    kind: 'operator-real-nat',
    policyId: requireCanonicalId(semantics.policyId),
    policyVersion: requireRevision(semantics.policyVersion),
    senderHostId,
    senderNetworkBoundaryId,
    stunEndpoint: requireIceUri(semantics.stunEndpoint, ['stun:']),
  })
}

function activeStunEndpointFromFixture(fixture: ExternalFixtureDeclaration): string | null {
  const semantics = fixture.networkSemantics
  return semantics.kind === 'public-stun' || semantics.kind === 'operator-real-nat'
    ? semantics.stunEndpoint
    : null
}

function exactRecord(value: unknown, keys: readonly string[]): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) invalidAttestation()
  const record = value as Record<string, unknown>
  const actual = Object.keys(record)
  const expected = [...keys]
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) {
    invalidAttestation()
  }
  return record
}

function requireProfileId(value: unknown): NetworkMatrixProfileId {
  if (
    value !== 'scheduled-public-stun' && value !== 'scheduled-restricted-udp' &&
    value !== 'scheduled-coturn' && value !== 'manual-real-nat'
  ) invalidAttestation()
  return value
}

function requireCanonicalId(value: unknown): string {
  if (typeof value !== 'string' || value.length > 96 || !CANONICAL_ID_PATTERN.test(value)) {
    invalidAttestation()
  }
  return value
}

function requireOpaqueId(value: unknown): string {
  if (typeof value !== 'string' || !OPAQUE_ID_PATTERN.test(value)) invalidAttestation()
  return value
}

function requireSignature(value: unknown): string {
  if (typeof value !== 'string' || !SIGNATURE_PATTERN.test(value)) invalidSignature()
  const signature = Buffer.from(value, 'base64url')
  if (signature.byteLength !== 64 || signature.toString('base64url') !== value) invalidSignature()
  return value
}

function requireSha256(value: unknown): string {
  if (typeof value !== 'string' || !SHA256_PATTERN.test(value)) invalidAttestation()
  return value
}

function requireRevision(value: unknown): number {
  if (!Number.isSafeInteger(value) || (value as number) < 1) invalidAttestation()
  return value as number
}

function requirePort(value: unknown): number {
  if (!Number.isSafeInteger(value) || (value as number) < 1 || (value as number) > 65_535) {
    invalidAttestation()
  }
  return value as number
}

function requireLeaseMillis(value: unknown): number {
  if (
    !Number.isSafeInteger(value) || (value as number) < 1 ||
    (value as number) > MAXIMUM_ATTESTATION_LEASE_MS
  ) invalidAttestation()
  return value as number
}

function requireUtcTimestamp(value: unknown): string {
  if (typeof value !== 'string' || !RFC3339_UTC_PATTERN.test(value)) invalidAttestation()
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp) || new Date(timestamp).toISOString() !== value) {
    invalidAttestation()
  }
  return value
}

function requireGlobalIpv4(value: unknown): string {
  if (!isGlobalUnicastIpv4(value)) invalidAttestation()
  return value
}

function requireOriginOnlyHttps(value: unknown): string {
  if (typeof value !== 'string') invalidAttestation()
  let origin: URL
  try {
    origin = new URL(value)
  } catch {
    invalidAttestation()
  }
  if (
    origin.protocol !== 'https:' || origin.username !== '' || origin.password !== '' ||
    origin.pathname !== '/' || origin.search !== '' || origin.hash !== '' ||
    value !== `${origin.origin}/`
  ) invalidAttestation()
  return value
}

function requireIceUri(value: unknown, schemes: readonly string[]): string {
  if (!validNetworkMatrixIceUri(value, schemes)) invalidAttestation()
  return value
}

function requireCredential(value: unknown): string {
  if (
    typeof value !== 'string' || value === '' || Buffer.byteLength(value, 'utf8') > 512 ||
    value.includes('\0') || value.includes('\r') || value.includes('\n')
  ) invalidAttestation()
  return value
}

function requireLiteral<T extends string>(value: unknown, expected: T): T {
  if (value !== expected) invalidAttestation()
  return expected
}

function sha256(value: string): string {
  return createHash('sha256').update(value).digest('hex')
}

function invalidAttestation(): never {
  throw new ExternalFixtureAttestationError('proof-invalid')
}

function invalidBinding(): never {
  throw new ExternalFixtureAttestationError('authority-binding-mismatch')
}

function invalidSignature(): never {
  throw new ExternalFixtureAttestationError('proof-invalid')
}

function expiredAttestation(): never {
  throw new ExternalFixtureAttestationError('authority-attestation-expired')
}

export class ExternalFixtureAttestationError extends Error {
  readonly failureCode:
    | 'proof-invalid'
    | 'authority-binding-mismatch'
    | 'authority-attestation-expired'

  constructor(
    failureCode:
      | 'proof-invalid'
      | 'authority-binding-mismatch'
      | 'authority-attestation-expired',
  ) {
    super('external fixture attestation was rejected')
    this.name = 'ExternalFixtureAttestationError'
    this.failureCode = failureCode
  }
}
