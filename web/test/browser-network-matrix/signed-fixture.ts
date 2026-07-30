import {
  createHash,
  createPrivateKey,
  createPublicKey,
  sign,
  type KeyObject,
} from 'node:crypto'

import { signedExternalFixtureConfigurationSha256 } from '../../scripts/browser-network-matrix/external-fixture-proof.ts'
import {
  canonicalExternalFixtureAttestationJson,
  EXTERNAL_FIXTURE_ATTESTATION_SCHEMA,
  EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
  EXTERNAL_FIXTURE_DECLARATION_SCHEMA,
  EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM,
  externalFixtureNetworkBindingSha256,
  externalFixtureRemotePeerBindingSha256,
  type ExternalFixtureAttestation,
  type ExternalFixtureDeclaration,
  type SignedExternalFixtureAttestation,
} from '../../scripts/browser-network-matrix/linux-topology/external-fixture-attestation.ts'
import {
  canonicalExternalFixtureTerminalReceiptJson,
  type ExternalFixtureTerminalReceipt,
  type SignedExternalFixtureTerminalReceipt,
} from '../../scripts/browser-network-matrix/linux-topology/external-fixture-terminal-receipt.ts'
import {
  canonicalExternalFixtureControlCredentialLeaseJson,
  canonicalExternalFixtureControlCredentialReceiptJson,
  type ExternalFixtureControlCredentialAuthority,
  type ExternalFixtureControlCredentialLease,
  type ExternalFixtureControlCredentialLeasePayload,
  type ExternalFixtureControlCredentialReceipt,
  type SignedExternalFixtureControlCredentialLease,
  type SignedExternalFixtureControlCredentialReceipt,
} from '../../scripts/browser-network-matrix/linux-topology/control-credential.ts'
import {
  NETWORK_MATRIX_ATTEMPT_AUTHORITY_SCHEMA,
  NETWORK_MATRIX_ATTEMPT_REQUEST_AUTHORITY_SCHEMA,
  NETWORK_MATRIX_CONTROL_AUTHORITY_SCHEMA,
  NETWORK_MATRIX_SAMPLE_AUTHORITY_SCHEMA,
  networkMatrixAttemptAuthoritySha256,
  type NetworkMatrixAttemptAuthority,
  type NetworkMatrixControlAuthority,
} from '../../scripts/browser-network-matrix/sample-authority.ts'
import {
  CONTAINED_BROWSER_SAMPLE_OUTPUT_SCHEMA,
  type ContainedBrowserSampleOutput,
} from '../../scripts/browser-network-matrix/linux-topology/contained-browser-sample.ts'
import type { ExternalFixtureTrustProof } from '../../scripts/browser-network-matrix/attestation.ts'
import type { NetworkMatrixAttemptEvidence } from '../../scripts/browser-network-matrix/attempt-evidence.ts'
import type { NetworkMatrixIdentity } from '../../scripts/browser-network-matrix/manifest.ts'
import {
  type NetworkMatrixExecutionAuthority,
} from '../../scripts/browser-network-matrix/runtime-authority.ts'
import {
  NETWORK_MATRIX_BROWSERS,
  NETWORK_MATRIX_ID,
  NETWORK_MATRIX_IDENTITY_COUNTS,
  NETWORK_MATRIX_MANIFEST_SCHEMA,
  NETWORK_MATRIX_PROFILE_REGISTRY,
  NETWORK_MATRIX_REPORTING_SEMANTICS,
  NETWORK_MATRIX_SAMPLE_ORDINALS,
  NETWORK_TOPOLOGY_PROFILE_SCHEMA,
  type NetworkMatrixProfileId,
} from '../../scripts/browser-network-matrix/vocabulary.ts'

const TEST_AUTHORITY_SEED_DOMAIN =
  'windshare/browser-network-matrix/test-fixture-attestation-authority/v1\n'
const ED25519_PKCS8_SEED_PREFIX = Buffer.from('302e020100300506032b657004220420', 'hex')
const FIXTURE_ISSUED_AT = '2026-07-29T00:00:00.000Z'
const FIXTURE_EXPIRES_AT = '2026-07-29T00:05:00.000Z'
const FIXTURE_LEASE_MILLIS = 300_000
const CONTROL_LEASE_MILLIS = 60_000
// The production validator requires globally routable IPv4 values, so the
// deterministic fixture uses stable public DNS endpoints without contacting them.
// eslint-disable-next-line sonarjs/no-hardcoded-ip
export const TEST_CONTROLLER_PUBLIC_IP = '8.8.8.8'
// eslint-disable-next-line sonarjs/no-hardcoded-ip
export const TEST_REMOTE_PEER_PUBLIC_IP = '1.1.1.1'
// eslint-disable-next-line sonarjs/no-hardcoded-ip
export const TEST_BROWSER_PUBLIC_IP = '8.8.4.4'
const TEST_CONTROL_CREDENTIAL_DOMAIN =
  'windshare/browser-network-matrix/test-control-credential/v1\n'

export const TEST_FIXTURE_NOW = Date.parse(FIXTURE_ISSUED_AT)
export const TEST_FIXTURE_PROBE_NONCE = 'test-fixture-probe-nonce'

export const TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SPKI =
  'MCowBQYDK2VwAyEAzzdkusInBjsJpvUWGibdzr50te_7a2iquAxNqY_jJiE'
export const TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SHA256 =
  'dfb0a30ce0e35eae2e02434da28c14a7977eb4b966f82efee9cfd6e27c401ad8'
export const TEST_FIXTURE_CONTROLLER_ORIGIN = 'https://fixture.example.test/'
export const TEST_FIXTURE_TLS_CERTIFICATE_SHA256 = 'a'.repeat(64)
export const TEST_FIXTURE_TLS_CERTIFICATE_AUTHORITY_SHA256 = 'b'.repeat(64)

export interface TestExternalFixtureLiveProof {
  readonly probeId: string
  readonly attestationPublicKeySpki: string
  readonly signedAttestation: SignedExternalFixtureAttestation
  readonly attestationSha256: string
  readonly authorityInstanceId: string
  readonly remoteServiceInstanceId: string
  readonly configurationSha256: string
  readonly networkBindingSha256: string
  readonly remotePeerBindingSha256: string
  readonly controllerPublicIp: string
  readonly leaseExpiresAt: string
}

export interface GeneratedTestNetworkMatrixRegistry {
  readonly manifest: string
  readonly profiles: Readonly<Record<NetworkMatrixProfileId, string>>
}

export interface TestNetworkMatrixAttemptOptions {
  readonly attemptId?: string
  readonly challenge?: string
  readonly processInstanceId?: string
}

export interface TestExternalFixtureControlCredentialAuthorityOptions {
  readonly now?: () => number
  readonly credential?: (scope: {
    readonly runId: string
    readonly profileId: NetworkMatrixProfileId
    readonly probeNonce: string
  }) => string
}

export function generateTestNetworkMatrixRegistry(): GeneratedTestNetworkMatrixRegistry {
  const profiles = Object.fromEntries(NETWORK_MATRIX_PROFILE_REGISTRY.map((reference) => {
    const profile = testProfile(reference.profileId)
    return [reference.profileId, canonicalJson(profile)]
  })) as Record<NetworkMatrixProfileId, string>
  const authorities = NETWORK_MATRIX_PROFILE_REGISTRY.map((reference) => ({
    authorityId: reference.authorityId,
    authorityKind: reference.authorityKind,
    availabilityExpectation: 'not-assumed',
    attestationPublicKeySha256: TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SHA256,
  }))
  const profileReferences = NETWORK_MATRIX_PROFILE_REGISTRY.map((reference) => ({
    ...reference,
    profileSha256: sha256(profiles[reference.profileId]),
  }))
  return Object.freeze({
    manifest: canonicalJson({
      schemaVersion: NETWORK_MATRIX_MANIFEST_SCHEMA,
      matrixId: NETWORK_MATRIX_ID,
      reportingSemantics: NETWORK_MATRIX_REPORTING_SEMANTICS,
      browsers: NETWORK_MATRIX_BROWSERS,
      sampleOrdinals: NETWORK_MATRIX_SAMPLE_ORDINALS,
      authorities,
      profiles: profileReferences,
      identityCounts: NETWORK_MATRIX_IDENTITY_COUNTS,
    }),
    profiles: Object.freeze(profiles),
  })
}

export function testExternalFixtureProof(
  runId: string,
  profileId: NetworkMatrixProfileId,
): TestExternalFixtureLiveProof {
  const fixture = testExternalFixtureDeclaration(profileId)
  const attestation: ExternalFixtureAttestation = Object.freeze({
    schemaVersion: EXTERNAL_FIXTURE_ATTESTATION_SCHEMA,
    runId,
    nonce: `nonce-${profileId}-fixture`,
    leaseId: `lease-${profileId}-fixture`,
    leaseMillis: FIXTURE_LEASE_MILLIS,
    issuedAt: FIXTURE_ISSUED_AT,
    expiresAt: FIXTURE_EXPIRES_AT,
    fixture,
  })
  const canonical = canonicalExternalFixtureAttestationJson(attestation)
  const attestationSha256 = sha256(canonical)
  const signedAttestation = signTestAttestation(attestation, canonical, attestationSha256)
  return Object.freeze({
    probeId: `probe-${attestationSha256}`,
    attestationPublicKeySpki: TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SPKI,
    signedAttestation,
    attestationSha256,
    authorityInstanceId: fixture.authorityInstanceId,
    remoteServiceInstanceId: fixture.remoteServiceInstanceId,
    configurationSha256: signedExternalFixtureConfigurationSha256(fixture, attestationSha256),
    networkBindingSha256: externalFixtureNetworkBindingSha256(fixture),
    remotePeerBindingSha256: externalFixtureRemotePeerBindingSha256(fixture),
    controllerPublicIp: fixture.controllerPublicIp,
    leaseExpiresAt: attestation.expiresAt,
  })
}

export function testExternalFixtureTrustProof(): ExternalFixtureTrustProof {
  return Object.freeze({
    controllerOrigin: TEST_FIXTURE_CONTROLLER_ORIGIN,
    tlsCertificateSha256: TEST_FIXTURE_TLS_CERTIFICATE_SHA256,
    tlsCertificateAuthoritySha256: TEST_FIXTURE_TLS_CERTIFICATE_AUTHORITY_SHA256,
    attestationPublicKeySpki: TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SPKI,
    attestationPublicKeySha256: TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SHA256,
  })
}

export function testNetworkMatrixExecutionAuthority(
  profileId: NetworkMatrixProfileId,
): NetworkMatrixExecutionAuthority {
  return Object.freeze({
    profileId,
    runtimeKind: 'external-fixture',
  })
}

export function testContainedBrowserSampleOutput(
  runId: string,
  identity: NetworkMatrixIdentity,
): ContainedBrowserSampleOutput {
  const processScope = [runId, identity.profileId, identity.browser, identity.sampleOrdinal].join('/')
  const processInstanceId = `browser-${sha256(processScope).slice(0, 16)}`
  const evidence = testNetworkMatrixAttemptEvidence(runId, identity, { processInstanceId })
  const fixture = evidence.externalFixture
  const receipt = evidence.terminalReceipt.receipt
  return Object.freeze({
    schemaVersion: CONTAINED_BROWSER_SAMPLE_OUTPUT_SCHEMA,
    processInstanceId,
    browser: identity.browser,
    protocolResult: Object.freeze({
      runId: fixture.runId,
      authorityInstanceId: fixture.authorityInstanceId,
      attestationSha256: fixture.attestationSha256,
      remoteServiceInstanceId: fixture.remoteServiceInstanceId,
      attestationPublicKeySpki: fixture.attestationPublicKeySpki,
      signedAttestation: fixture.signedAttestation,
      networkBindingSha256: fixture.networkBindingSha256,
      remotePeerBindingSha256: fixture.remotePeerBindingSha256,
      controllerPublicIp: fixture.controllerPublicIp,
      attestationExpiresAt: fixture.attestationExpiresAt,
      remotePeerPublicIp: fixture.remotePeerPublicIp,
      remotePeerUdpPortMin: fixture.remotePeerUdpPortMin,
      remotePeerUdpPortMax: fixture.remotePeerUdpPortMax,
      attemptAuthority: evidence.attemptAuthority,
      state: receipt.state,
      selectedPair: receipt.selectedPair,
      challengeBindingSha256: receipt.challengeBindingSha256,
      challenge: receipt.attemptAuthority.challenge,
      failureCode: receipt.failureCode,
      challengeEchoed: evidence.challenge?.browserEchoObserved ?? false,
      terminalReceipt: evidence.terminalReceipt,
    }),
    browserSelectedPair: evidence.browserSelectedPair,
  })
}

export function canonicalTestContainedBrowserSampleOutputJson(
  runId: string,
  identity: NetworkMatrixIdentity,
): string {
  return canonicalJson(testContainedBrowserSampleOutput(runId, identity))
}

export function testFixtureAttestationPublicKeyPem(): string {
  return createPublicKey({
    key: Buffer.from(TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SPKI, 'base64url'),
    format: 'der',
    type: 'spki',
  }).export({ format: 'pem', type: 'spki' }).toString()
}

export function testExternalFixtureControlCredential(
  runId: string,
  profileId: NetworkMatrixProfileId,
  probeNonce: string,
): string {
  return createHash('sha256')
    .update(TEST_CONTROL_CREDENTIAL_DOMAIN, 'ascii')
    .update(canonicalJson({ runId, profileId, probeNonce }), 'utf8')
    .digest('base64url')
}

export function testExternalFixtureControlCredentialAuthority(
  options: TestExternalFixtureControlCredentialAuthorityOptions = {},
): ExternalFixtureControlCredentialAuthority {
  const now = options.now ?? (() => TEST_FIXTURE_NOW)
  const credential = options.credential ?? ((scope) => testExternalFixtureControlCredential(
    scope.runId,
    scope.profileId,
    scope.probeNonce,
  ))
  return Object.freeze({
    acquire: async (
      scope: Parameters<ExternalFixtureControlCredentialAuthority['acquire']>[0],
    ): Promise<ExternalFixtureControlCredentialLease> => {
      if (scope.signal.aborted) throw new Error('test fixture control credential acquisition aborted')
      const sampleAuthority = scope.sampleAuthority
      const proof = testExternalFixtureProof(sampleAuthority.runId, sampleAuthority.profileId)
      const issuedAtMillis = now()
      const issuedAt = new Date(issuedAtMillis).toISOString()
      const expiresAt = new Date(issuedAtMillis + CONTROL_LEASE_MILLIS).toISOString()
      const idScope = canonicalJson({
        sampleAuthority,
        probeNonce: scope.probeNonce,
      })
      const controlLeaseId = testOpaqueId('lease', idScope)
      const releaseRequestId = testOpaqueId('release', idScope)
      const revokeRequestId = testOpaqueId('revoke', idScope)
      const controlAuthority: NetworkMatrixControlAuthority = Object.freeze({
        schemaVersion: NETWORK_MATRIX_CONTROL_AUTHORITY_SCHEMA,
        sampleAuthority,
        controlLeaseId,
      })
      const turnProviderLeaseId = sampleAuthority.profileId === 'scheduled-coturn'
        ? testOpaqueId('turn-provider', idScope)
        : ''
      const fixture = proof.signedAttestation.attestation.fixture
      const semantics = fixture.networkSemantics
      const turnCredentialId = semantics.kind === 'coturn-relay'
        ? semantics.turnCredentialId
        : ''
      const turnUsername = semantics.kind === 'coturn-relay' ? semantics.turnUsername : ''
      const controlCredential = Buffer.from(credential({
        runId: sampleAuthority.runId,
        profileId: sampleAuthority.profileId,
        probeNonce: scope.probeNonce,
      }), 'utf8')
      const leasePayload: ExternalFixtureControlCredentialLeasePayload = Object.freeze({
        protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
        requestId: controlLeaseId,
        releaseRequestId,
        revokeRequestId,
        controlAuthority,
        probeNonce: scope.probeNonce,
        authorityInstanceId: proof.authorityInstanceId,
        attestationSha256: proof.attestationSha256,
        issuedAt,
        expiresAt,
        maxAttempts: 1,
        credentialByteLength: controlCredential.byteLength,
        turnCapability: sampleAuthority.profileId === 'scheduled-coturn' ? 'bound' : 'not-required',
        turnProviderLeaseId,
        turnCredentialId,
        turnUsername,
        turnExpiresAt: sampleAuthority.profileId === 'scheduled-coturn' ? expiresAt : '',
      })
      const signedLease = signTestControlCredentialLease(leasePayload)
      const receipt = (
        operation: 'release' | 'revoke-and-wait',
      ) => {
        const value: ExternalFixtureControlCredentialReceipt = Object.freeze({
          protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
          operation,
          requestId: operation === 'release' ? releaseRequestId : revokeRequestId,
          releaseRequestId,
          revokeRequestId,
          controlAuthority,
          probeNonce: scope.probeNonce,
          authorityInstanceId: proof.authorityInstanceId,
          attestationSha256: proof.attestationSha256,
          leaseExpiresAt: expiresAt,
          controlTerminal: 'revoked',
          turnProviderLeaseId,
          turnTerminal: sampleAuthority.profileId === 'scheduled-coturn'
            ? 'revoked'
            : 'not-required',
          terminal: 'revoked',
          retiredAt: new Date(now()).toISOString(),
        })
        return Object.freeze({
          receipt: value,
          signedReceipt: signTestControlCredentialReceipt(value),
        })
      }
      return Object.freeze({
        signedLease,
        requestId: controlLeaseId,
        releaseRequestId,
        revokeRequestId,
        controlAuthority,
        probeNonce: scope.probeNonce,
        authorityInstanceId: proof.authorityInstanceId,
        attestationSha256: proof.attestationSha256,
        issuedAt,
        expiresAt,
        maxAttempts: 1,
        turnCapability: sampleAuthority.profileId === 'scheduled-coturn' ? 'bound' : 'not-required',
        turnProviderLeaseId,
        turnCredentialId,
        turnUsername,
        turnExpiresAt: sampleAuthority.profileId === 'scheduled-coturn' ? expiresAt : '',
        credential: controlCredential,
        release: async () => receipt('release'),
        revokeAndWait: async () => receipt('revoke-and-wait'),
      })
    },
    closeAndWait: async () => Object.freeze({ terminal: 'closed' as const }),
    forceTerminateAndWait: async () => Object.freeze({ terminal: 'closed' as const }),
  })
}

export function testNetworkMatrixAttemptEvidence(
  runId: string,
  identity: NetworkMatrixIdentity,
  options: TestNetworkMatrixAttemptOptions = {},
): NetworkMatrixAttemptEvidence {
  const proof = testExternalFixtureProof(runId, identity.profileId)
  const fixture = proof.signedAttestation.attestation.fixture
  const defaultAttemptId = [
    'attempt', identity.profileId, identity.browser, identity.sampleOrdinal,
  ].join('-')
  const defaultChallenge = [
    'challenge', identity.profileId, identity.browser, identity.sampleOrdinal,
  ].join('-')
  const attemptId = options.attemptId ?? defaultAttemptId
  const challenge = options.challenge ?? defaultChallenge
  const processInstanceId = options.processInstanceId ??
    `process-${identity.profileId}-${identity.browser}-${identity.sampleOrdinal}`
  const attemptAuthority: NetworkMatrixAttemptAuthority = Object.freeze({
    schemaVersion: NETWORK_MATRIX_ATTEMPT_AUTHORITY_SCHEMA,
    requestAuthority: Object.freeze({
      schemaVersion: NETWORK_MATRIX_ATTEMPT_REQUEST_AUTHORITY_SCHEMA,
      controlAuthority: Object.freeze({
        schemaVersion: NETWORK_MATRIX_CONTROL_AUTHORITY_SCHEMA,
        sampleAuthority: Object.freeze({
          schemaVersion: NETWORK_MATRIX_SAMPLE_AUTHORITY_SCHEMA,
          runId,
          profileId: identity.profileId,
          browser: identity.browser,
          sampleOrdinal: identity.sampleOrdinal,
          processInstanceId,
          operationId: `${runId}-${identity.profileId}-${identity.browser}-${identity.sampleOrdinal}`,
        }),
        controlLeaseId: attemptId.replace('attempt-', 'control-'),
      }),
      requestId: attemptId.replace('attempt-', 'request-'),
      fixtureBinding: Object.freeze({
        attestationSha256: proof.attestationSha256,
        authorityInstanceId: proof.authorityInstanceId,
        remoteServiceInstanceId: proof.remoteServiceInstanceId,
        networkBindingSha256: proof.networkBindingSha256,
        remotePeerBindingSha256: proof.remotePeerBindingSha256,
      }),
    }),
    attemptId,
    challenge,
  })
  const challengeBindingSha256 = networkMatrixAttemptAuthoritySha256(attemptAuthority)
  const established = identity.profileId !== 'scheduled-restricted-udp'
  const localCandidateType = identity.profileId === 'scheduled-coturn' ? 'relay' : 'srflx'
  const browserSelectedPair = established
    ? Object.freeze({
        selectedPair: 'present' as const,
        localCandidateType,
        localAddress: TEST_BROWSER_PUBLIC_IP,
        localPort: 50_000,
        remoteCandidateType: 'host' as const,
        remoteAddress: fixture.remotePeerPublicIp,
        remotePort: fixture.remotePeerUdpPortMin,
        protocol: 'udp' as const,
      })
    : absentBrowserPair()
  const pionSelectedPair = established
    ? Object.freeze({
        selectedPair: 'present' as const,
        localCandidateType: 'host' as const,
        localAddressFamily: 'ipv4' as const,
        remoteCandidateType: localCandidateType,
        remoteAddressFamily: 'ipv4' as const,
        protocol: 'udp' as const,
        localAddress: fixture.remotePeerPublicIp,
        localPort: fixture.remotePeerUdpPortMin,
        remoteAddress: TEST_BROWSER_PUBLIC_IP,
        remotePort: 50_000,
      })
    : absentPionPair()
  const receipt = testTerminalReceipt(
    attemptAuthority,
    challengeBindingSha256,
    pionSelectedPair,
  )
  return Object.freeze({
    attemptAuthority,
    pionAuthority: 'external-remote',
    externalFixture: Object.freeze({
      runId,
      authorityInstanceId: proof.authorityInstanceId,
      remoteServiceInstanceId: proof.remoteServiceInstanceId,
      attestationSha256: proof.attestationSha256,
      attestationPublicKeySpki: proof.attestationPublicKeySpki,
      signedAttestation: proof.signedAttestation,
      networkBindingSha256: proof.networkBindingSha256,
      remotePeerBindingSha256: proof.remotePeerBindingSha256,
      controllerPublicIp: proof.controllerPublicIp,
      attestationExpiresAt: proof.leaseExpiresAt,
      remotePeerPublicIp: fixture.remotePeerPublicIp,
      remotePeerUdpPortMin: fixture.remotePeerUdpPortMin,
      remotePeerUdpPortMax: fixture.remotePeerUdpPortMax,
    }),
    browserSelectedPair,
    pionSelectedPair,
    challenge: established
      ? Object.freeze({
          bindingSha256: challengeBindingSha256,
          challenge,
          pionChallengeObserved: true as const,
          browserEchoObserved: true as const,
        })
      : null,
    terminalReceipt: receipt,
  })
}

function signTestAttestation(
  attestation: ExternalFixtureAttestation,
  canonical: string,
  attestationSha256: string,
): SignedExternalFixtureAttestation {
  const privateKey = testAuthorityPrivateKey()
  return Object.freeze({
    protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    attestation,
    attestationSha256,
    signatureAlgorithm: EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM,
    signature: sign(null, Buffer.from(canonical), privateKey).toString('base64url'),
  })
}

function signTestControlCredentialLease(
  lease: ExternalFixtureControlCredentialLeasePayload,
): SignedExternalFixtureControlCredentialLease {
  const canonical = canonicalExternalFixtureControlCredentialLeaseJson(lease)
  return Object.freeze({
    protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    lease,
    leaseSha256: sha256(canonical),
    signatureAlgorithm: EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM,
    signature: sign(null, Buffer.from(canonical), testAuthorityPrivateKey()).toString('base64url'),
  })
}

function signTestControlCredentialReceipt(
  receipt: ExternalFixtureControlCredentialReceipt,
): SignedExternalFixtureControlCredentialReceipt {
  const canonical = canonicalExternalFixtureControlCredentialReceiptJson(receipt)
  return Object.freeze({
    protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    receipt,
    receiptSha256: sha256(canonical),
    signatureAlgorithm: EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM,
    signature: sign(null, Buffer.from(canonical), testAuthorityPrivateKey()).toString('base64url'),
  })
}

function testTerminalReceipt(
  attemptAuthority: NetworkMatrixAttemptAuthority,
  challengeBindingSha256: string,
  pionSelectedPair: NetworkMatrixAttemptEvidence['pionSelectedPair'],
): SignedExternalFixtureTerminalReceipt {
  const established = pionSelectedPair.selectedPair === 'present'
  const receipt: ExternalFixtureTerminalReceipt = Object.freeze({
    protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    attemptAuthority,
    terminalAt: '2026-07-29T00:00:02.000Z',
    attemptLeaseIssuedAt: '2026-07-29T00:00:01.000Z',
    attemptLeaseExpiresAt: '2026-07-29T00:01:01.000Z',
    attemptLeaseMillis: 60_000,
    state: established ? 'established' : 'failed',
    selectedPair: established
      ? Object.freeze({
          local: terminalCandidate(
            pionSelectedPair.localCandidateType,
            pionSelectedPair.protocol,
            pionSelectedPair.localAddress,
            pionSelectedPair.localPort,
          ),
          remote: terminalCandidate(
            pionSelectedPair.remoteCandidateType,
            pionSelectedPair.protocol,
            pionSelectedPair.remoteAddress,
            pionSelectedPair.remotePort,
          ),
        })
      : null,
    challengeBindingSha256,
    failureCode: established ? null : 'ice-failed',
  })
  const canonical = canonicalExternalFixtureTerminalReceiptJson(receipt)
  return Object.freeze({
    protocolVersion: EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    receipt,
    receiptSha256: sha256(canonical),
    signatureAlgorithm: EXTERNAL_FIXTURE_SIGNATURE_ALGORITHM,
    signature: sign(null, Buffer.from(canonical), testAuthorityPrivateKey()).toString('base64url'),
  })
}

function terminalCandidate(
  candidateType: 'host' | 'srflx' | 'prflx' | 'relay',
  protocol: 'udp' | 'tcp',
  address: string,
  port: number,
): Record<string, unknown> {
  return Object.freeze({ candidateType, protocol, address, port, addressFamily: 'ipv4' })
}

function absentBrowserPair() {
  return Object.freeze({
    selectedPair: 'absent' as const,
    localCandidateType: null,
    localAddress: null,
    localPort: null,
    remoteCandidateType: null,
    remoteAddress: null,
    remotePort: null,
    protocol: null,
  })
}

function absentPionPair() {
  return Object.freeze({
    selectedPair: 'absent' as const,
    localCandidateType: null,
    localAddressFamily: null,
    remoteCandidateType: null,
    remoteAddressFamily: null,
    protocol: null,
    localAddress: null,
    localPort: null,
    remoteAddress: null,
    remotePort: null,
  })
}

function testAuthorityPrivateKey(): KeyObject {
  const seed = createHash('sha256').update(TEST_AUTHORITY_SEED_DOMAIN, 'ascii').digest()
  const encoded = Buffer.concat([ED25519_PKCS8_SEED_PREFIX, seed])
  seed.fill(0)
  try {
    const privateKey = createPrivateKey({ key: encoded, format: 'der', type: 'pkcs8' })
    const publicDer = Buffer.from(createPublicKey(privateKey).export({ format: 'der', type: 'spki' }))
    try {
      if (
        publicDer.toString('base64url') !== TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SPKI ||
        sha256(publicDer) !== TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SHA256
      ) throw new Error('test fixture authority derivation changed')
      return privateKey
    } finally {
      publicDer.fill(0)
    }
  } finally {
    encoded.fill(0)
  }
}

function testOpaqueId(kind: string, scope: string): string {
  const digest = sha256(`${kind}\n${scope}`)
  return `${kind}-${digest.slice(0, 32)}`
}

function testProfile(profileId: NetworkMatrixProfileId): Record<string, unknown> {
  const reference = NETWORK_MATRIX_PROFILE_REGISTRY.find((entry) => entry.profileId === profileId)
  if (reference === undefined) throw new Error(`unknown test profile ${profileId}`)
  return {
    schemaVersion: NETWORK_TOPOLOGY_PROFILE_SCHEMA,
    profileId: reference.profileId,
    profileKind: reference.profileKind,
    executionMode: reference.executionMode,
    authority: {
      authorityId: reference.authorityId,
      authorityKind: reference.authorityKind,
      availabilityExpectation: 'not-assumed',
      attestationPublicKeySha256: TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SHA256,
    },
    connectivityExpectation: profileId === 'scheduled-restricted-udp'
      ? 'connectivity-blocked'
      : 'connectivity-established',
    candidatePolicy: testCandidatePolicy(profileId),
  }
}

function testCandidatePolicy(profileId: NetworkMatrixProfileId): Record<string, unknown> {
  if (profileId === 'scheduled-restricted-udp') {
    const empty = { allowed: [], required: [], forbidden: [] }
    return {
      selectedPair: 'prohibited',
      localCandidateTypes: empty,
      remoteCandidateTypes: empty,
      protocols: empty,
    }
  }
  if (profileId === 'scheduled-coturn') {
    return {
      selectedPair: 'required',
      localCandidateTypes: {
        allowed: ['relay'],
        required: ['relay'],
        forbidden: ['host', 'srflx', 'prflx'],
      },
      remoteCandidateTypes: {
        allowed: ['host', 'srflx', 'prflx', 'relay'],
        required: [],
        forbidden: [],
      },
      protocols: { allowed: ['udp', 'tcp'], required: [], forbidden: [] },
    }
  }
  return {
    selectedPair: 'required',
    localCandidateTypes: {
      allowed: ['host', 'srflx', 'prflx'],
      required: ['srflx'],
      forbidden: ['relay'],
    },
    remoteCandidateTypes: {
      allowed: ['host', 'srflx', 'prflx'],
      required: [],
      forbidden: ['relay'],
    },
    protocols: { allowed: ['udp'], required: ['udp'], forbidden: ['tcp'] },
  }
}

function testExternalFixtureDeclaration(
  profileId: NetworkMatrixProfileId,
): ExternalFixtureDeclaration {
  const identity = profileId.replace(/^scheduled-/u, '').replace(/^manual-/u, '')
  const base = {
    schemaVersion: EXTERNAL_FIXTURE_DECLARATION_SCHEMA,
    deploymentId: `${identity}-deployment`,
    revision: 1,
    profileId,
    authorityInstanceId: `${identity}-authority`,
    implementationSha256: sha256(`implementation:${profileId}`),
    remoteServiceInstanceId: `${identity}-remote-pion`,
    operatorId: 'windshare-test-operator',
    fixtureHostId: `${identity}-fixture-host`,
    fixtureNetworkBoundaryId: `${identity}-fixture-network`,
    controllerOrigin: 'https://browser-matrix.test/',
    controllerPublicIp: TEST_CONTROLLER_PUBLIC_IP,
    tlsCertificateSha256: sha256('browser-matrix-test-certificate'),
    remotePeerPublicIp: TEST_REMOTE_PEER_PUBLIC_IP,
    remotePeerUdpPortMin: 40_000,
    remotePeerUdpPortMax: 40_099,
  } as const
  if (profileId === 'scheduled-public-stun') {
    return Object.freeze({
      ...base,
      networkSemantics: Object.freeze({
        kind: 'public-stun',
        policyId: 'public-stun-policy',
        policyVersion: 1,
        stunEndpoint: 'stun:stun.cloudflare.com:3478',
      }),
    })
  }
  if (profileId === 'scheduled-restricted-udp') {
    return Object.freeze({
      ...base,
      networkSemantics: Object.freeze({
        kind: 'restricted-udp',
        policyId: 'restricted-udp-policy',
        policyVersion: 1,
        outboundUdp: 'denied',
        unsolicitedInboundUdp: 'denied',
        relayAccess: 'denied',
      }),
    })
  }
  if (profileId === 'scheduled-coturn') {
    return Object.freeze({
      ...base,
      networkSemantics: Object.freeze({
        kind: 'coturn-relay',
        policyId: 'coturn-relay-policy',
        policyVersion: 1,
        turnServiceOwnerId: 'coturn-service-owner',
        turnUrls: Object.freeze(['turn:turn.browser-matrix.test:3478?transport=udp']),
        turnUsername: 'test-turn-user',
        turnCredentialId: 'test-turn-credential',
        turnCredentialExpiresAt: FIXTURE_EXPIRES_AT,
      }),
    })
  }
  return Object.freeze({
    ...base,
    networkSemantics: Object.freeze({
      kind: 'operator-real-nat',
      policyId: 'operator-real-nat-policy',
      policyVersion: 1,
      senderHostId: 'manual-sender-host',
      senderNetworkBoundaryId: 'manual-sender-network',
      stunEndpoint: 'stun:stun.cloudflare.com:3478',
    }),
  })
}

function canonicalJson(value: unknown): string {
  return `${JSON.stringify(value)}\n`
}

function sha256(value: string | Buffer): string {
  return createHash('sha256').update(value).digest('hex')
}
