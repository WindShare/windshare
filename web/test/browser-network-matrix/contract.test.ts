import { createHash } from 'node:crypto'
import { cp, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'

import { afterEach, beforeAll, describe, expect, it } from 'vitest'

import {
  parseNetworkRuntimeAttestation,
} from '../../scripts/browser-network-matrix/attestation.ts'
import {
  parseContainedBrowserSampleOutput,
} from '../../scripts/browser-network-matrix/linux-topology/contained-browser-sample.ts'
import {
  validateExternalFixtureControlCredential,
} from '../../scripts/browser-network-matrix/linux-topology/control-credential.ts'
import { NETWORK_MATRIX_SAMPLE_AUTHORITY_SCHEMA } from '../../scripts/browser-network-matrix/sample-authority.ts'
import {
  loadNetworkMatrixRegistry,
  networkMatrixIdentities,
  parseNetworkMatrixManifest,
  parseNetworkMatrixManifestJson,
  sha256,
  type LoadedNetworkMatrixRegistry,
} from '../../scripts/browser-network-matrix/manifest.ts'
import {
  parseNetworkTopologyProfile,
  parseNetworkTopologyProfileJson,
} from '../../scripts/browser-network-matrix/profile.ts'
import {
  canonicalManualSupplementProfileJson,
  parseManualSupplementProfileJson,
} from '../../scripts/browser-network-matrix/supplement/manual-profile.ts'
import { cloneJson, loadRegistry, MANIFEST_PATH, rawAttestation } from './fixtures.ts'
import {
  canonicalTestContainedBrowserSampleOutputJson,
  generateTestNetworkMatrixRegistry,
  TEST_FIXTURE_NOW,
  TEST_FIXTURE_PROBE_NONCE,
  TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SHA256,
  TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SPKI,
  testExternalFixtureControlCredential,
  testExternalFixtureControlCredentialAuthority,
  testNetworkMatrixExecutionAuthority,
} from './signed-fixture.ts'

const MANIFEST_SHA256 = '98f4d23f7029b84512a3307e7ed503a1c57e1a246ad86e3bb4f354f06e7a26bc'
const PROFILE_SHA256 = Object.freeze({
  'scheduled-public-stun': 'b25de62281b8f73757f15554f9d313457414784715623bae3ca1eb16eed62ade',
  'scheduled-restricted-udp': '71bc3f49d1386a97e200a0f73c3e0757d3bb23452b6bef201ff0a9bc5477a46a',
  'scheduled-coturn': '07e86e7fa9d197bd746f7360a06c25865d59cf246dc547a572e99e7787540b92',
})

let registry: LoadedNetworkMatrixRegistry
const temporaryRoots: string[] = []

beforeAll(async () => { registry = await loadRegistry() })
afterEach(async () => {
  await Promise.all(temporaryRoots.splice(0).map((root) => rm(root, { recursive: true, force: true })))
})

describe('browser network matrix registry contract', () => {
  it('rebuilds the complete test registry deterministically from its test-only trust anchor', async () => {
    const first = generateTestNetworkMatrixRegistry()
    const second = generateTestNetworkMatrixRegistry()
    expect(second).toEqual(first)
    expect(await readFile(MANIFEST_PATH, 'utf8')).toBe(first.manifest)
    await Promise.all(registry.manifest.profiles.map(async ({ profileId, profilePath }) => {
      expect(await readFile(join(dirname(MANIFEST_PATH), profilePath), 'utf8'))
        .toBe(first.profiles[profileId])
    }))

    const spki = Buffer.from(TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SPKI, 'base64url')
    expect(createHash('sha256').update(spki).digest('hex'))
      .toBe(TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SHA256)
    expect(registry.manifest.authorities.every((authority) =>
      authority.attestationPublicKeySha256 === TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SHA256,
    )).toBe(true)
  })

  it('derives canonical child bytes and sample-scoped lease secrets without profile live binding', async () => {
    const runId = 'canonical-child-fixture-run'
    const profileId = 'scheduled-public-stun' as const
    const identity = Object.freeze({ profileId, browser: 'chromium' as const, sampleOrdinal: 1 })
    const firstOutput = canonicalTestContainedBrowserSampleOutputJson(runId, identity)
    const secondOutput = canonicalTestContainedBrowserSampleOutputJson(runId, identity)
    expect(secondOutput).toBe(firstOutput)
    const canonicalOutput = JSON.parse(firstOutput) as Record<string, unknown>
    const parsedOutput = parseContainedBrowserSampleOutput(canonicalOutput)
    expect(parsedOutput).toEqual(canonicalOutput)
    expect(Object.keys(canonicalOutput.protocolResult as Record<string, unknown>)).toEqual([
      'runId', 'authorityInstanceId', 'attestationSha256', 'remoteServiceInstanceId',
      'attestationPublicKeySpki', 'signedAttestation', 'networkBindingSha256',
      'remotePeerBindingSha256', 'controllerPublicIp', 'attestationExpiresAt',
      'remotePeerPublicIp', 'remotePeerUdpPortMin', 'remotePeerUdpPortMax', 'attemptAuthority',
      'state', 'selectedPair', 'challengeBindingSha256', 'challenge', 'failureCode',
      'challengeEchoed', 'terminalReceipt',
    ])

    const execution = testNetworkMatrixExecutionAuthority(profileId)
    expect(execution).toEqual({
      profileId,
      runtimeKind: 'external-fixture',
    })

    const sampleAuthority = Object.freeze({
      schemaVersion: NETWORK_MATRIX_SAMPLE_AUTHORITY_SCHEMA,
      runId,
      profileId,
      browser: 'chromium' as const,
      sampleOrdinal: 1 as const,
      processInstanceId: 'browser-canonical-child',
      operationId: 'canonical-child-operation',
    })
    const scope = Object.freeze({
      sampleAuthority,
      probeNonce: TEST_FIXTURE_PROBE_NONCE,
      signal: new AbortController().signal,
    })
    const [firstLease, secondLease] = await Promise.all([
      testExternalFixtureControlCredentialAuthority().acquire(scope),
      testExternalFixtureControlCredentialAuthority().acquire(scope),
    ])
    try {
      expect(secondLease.controlAuthority.controlLeaseId).toBe(
        firstLease.controlAuthority.controlLeaseId,
      )
      expect(secondLease.releaseRequestId).toBe(firstLease.releaseRequestId)
      expect(secondLease.revokeRequestId).toBe(firstLease.revokeRequestId)
      expect(secondLease.credential).not.toBe(firstLease.credential)
      expect(Buffer.from(secondLease.credential)).toEqual(Buffer.from(firstLease.credential))
      expect(Buffer.from(firstLease.credential).toString('utf8')).toBe(
        testExternalFixtureControlCredential(runId, profileId, TEST_FIXTURE_PROBE_NONCE),
      )
      expect(testExternalFixtureControlCredential(
        runId,
        profileId,
        `${TEST_FIXTURE_PROBE_NONCE}-other`,
      )).not.toBe(Buffer.from(firstLease.credential).toString('utf8'))
      validateExternalFixtureControlCredential(firstLease, {
        sampleAuthority,
        probeNonce: TEST_FIXTURE_PROBE_NONCE,
        now: TEST_FIXTURE_NOW,
      })
    } finally {
      firstLease.credential.fill(0)
      secondLease.credential.fill(0)
      await Promise.all([firstLease.release(), secondLease.release()])
    }
  })

  it('freezes three required scheduled authorities and exactly 45 canonical identities', () => {
    expect(registry.manifestSha256).toBe(MANIFEST_SHA256)
    expect(Object.fromEntries(registry.manifest.profiles.map(({ profileId, profileSha256 }) => [
      profileId,
      profileSha256,
    ]))).toEqual(PROFILE_SHA256)
    expect(registry.manifest.reportingSemantics).toBe('scheduled-hard-fail-closed')

    const all = networkMatrixIdentities(registry.manifest)
    const scheduled = networkMatrixIdentities(registry.manifest, 'scheduled')
    expect([all.length, scheduled.length]).toEqual([45, 45])
    expect(new Set(all.map(({ profileId, browser, sampleOrdinal }) =>
      `${profileId}/${browser}/${sampleOrdinal}`))).toHaveLength(45)
    expect(all[0]).toEqual({
      profileId: 'scheduled-public-stun',
      browser: 'chromium',
      sampleOrdinal: 1,
    })
    expect(all.at(-1)).toEqual({
      profileId: 'scheduled-coturn',
      browser: 'webkit',
      sampleOrdinal: 5,
    })

    const hostileIdentities = networkMatrixIdentities as (
      manifest: LoadedNetworkMatrixRegistry['manifest'],
      mode: string,
    ) => readonly unknown[]
    expect(() => hostileIdentities(registry.manifest, 'nightly')).toThrow(/frozen vocabulary/u)
    expect(() => hostileIdentities(registry.manifest, 'manual')).toThrow(/frozen vocabulary/u)
  })

  it('keeps the manual real-NAT profile supplemental and outside every hard count', async () => {
    const supplementalPath = join(
      dirname(MANIFEST_PATH),
      'supplemental',
      'manual-real-nat.profile.v1.json',
    )
    const encoded = await readFile(supplementalPath, 'utf8')
    const supplemental = parseManualSupplementProfileJson(encoded)
    expect(Object.keys(supplemental)).toEqual([
      'schemaVersion',
      'supplementId',
      'reportingSemantics',
      'profileId',
      'authority',
      'connectivityExpectation',
      'candidatePolicy',
    ])
    expect(supplemental).toMatchObject({
      schemaVersion: 'windshare.browser-network-matrix.manual-supplement-profile/v1',
      supplementId: 'manual-real-nat-supplement-v1',
      reportingSemantics: 'manual-supplemental-non-authoritative',
      profileId: 'manual-real-nat',
    })
    expect(registry.manifest.profiles).not.toContainEqual(
      expect.objectContaining({ profileId: 'manual-real-nat' }),
    )
    expect(registry.manifest.identityCounts).toEqual({ total: 45, scheduled: 45 })
    expect(() => parseNetworkTopologyProfile(supplemental)).toThrow()
    expect(canonicalManualSupplementProfileJson(supplemental)).toBe(encoded)
  })

  it('rejects ambiguous bytes, duplicate members, unknown fields, and repeated profile digests', async () => {
    const encoded = await readFile(MANIFEST_PATH, 'utf8')
    expect(parseNetworkMatrixManifestJson(encoded)).toEqual(registry.manifest)
    expect(() => parseNetworkMatrixManifestJson(`\n${encoded}`)).toThrow(/canonical minified JSON/u)

    const duplicatedMember = encoded.replace(
      '{"schemaVersion":',
      '{"schemaVersion":"duplicate","schemaVersion":',
    )
    expect(() => parseNetworkMatrixManifestJson(duplicatedMember)).toThrow(/strict JSON/u)

    const unknownField = { ...cloneJson(registry.manifest), fabricatedAuthority: true }
    expect(() => parseNetworkMatrixManifest(unknownField)).toThrow(/exactly/u)

    const repeatedDigest = cloneJson(registry.manifest)
    repeatedDigest.profiles[1]!.profileSha256 = repeatedDigest.profiles[0]!.profileSha256
    expect(() => parseNetworkMatrixManifest(repeatedDigest)).toThrow(/digests repeat/u)
  })

  it('rejects non-JSON object fields, accessors, and sparse arrays before reading them', () => {
    const symbolField = cloneJson(registry.manifest)
    Object.defineProperty(symbolField, Symbol('fabricated'), {
      value: true,
      enumerable: true,
    })
    expect(() => parseNetworkMatrixManifest(symbolField)).toThrow(/symbol field/u)

    let accessorRead = false
    const accessorField = cloneJson(registry.manifest)
    Object.defineProperty(accessorField, 'matrixId', {
      get: () => {
        accessorRead = true
        return registry.manifest.matrixId
      },
      enumerable: true,
    })
    expect(() => parseNetworkMatrixManifest(accessorField)).toThrow(/JSON data field/u)
    expect(accessorRead).toBe(false)

    const sparseBrowsers = cloneJson(registry.manifest)
    delete sparseBrowsers.browsers[1]
    expect(() => parseNetworkMatrixManifest(sparseBrowsers)).toThrow(/must not be sparse/u)
  })

  it('detects profile mutation before accepting its semantic contents', async () => {
    const root = await mkdtemp(join(tmpdir(), 'windshare-network-matrix-'))
    temporaryRoots.push(root)
    const copiedRegistry = join(root, 'registry')
    await cp(dirname(MANIFEST_PATH), copiedRegistry, { recursive: true })
    const copiedManifest = join(copiedRegistry, 'scheduled-hard.manifest.v2.json')
    const profilePath = join(copiedRegistry, 'profiles', 'scheduled-public-stun.v2.json')
    await writeFile(profilePath, `${await readFile(profilePath, 'utf8')}\n`, 'utf8')

    await expect(loadNetworkMatrixRegistry(copiedManifest)).rejects.toThrow(/manifest digest/u)
  })

  it('rejects contradictory or ambiguous candidate-path policy definitions', async () => {
    const reference = registry.manifest.profiles[0]!
    const encoded = await readFile(join(dirname(MANIFEST_PATH), reference.profilePath), 'utf8')
    expect(sha256(encoded)).toBe(reference.profileSha256)
    expect(parseNetworkTopologyProfileJson(encoded)).toEqual(registry.profiles[0])

    const requiresTwoValues = cloneJson(registry.profiles[0]!)
    requiresTwoValues.candidatePolicy.localCandidateTypes.required = ['srflx', 'prflx']
    expect(() => parseNetworkTopologyProfile(requiresTwoValues)).toThrow(/multiple values/u)

    const unclassifiedVocabulary = cloneJson(registry.profiles[0]!)
    unclassifiedVocabulary.candidatePolicy.localCandidateTypes.forbidden = []
    expect(() => parseNetworkTopologyProfile(unclassifiedVocabulary)).toThrow(/classify every/u)

    const blockedWithPathPolicy = cloneJson(registry.profiles[1]!)
    blockedWithPathPolicy.candidatePolicy.protocols.allowed = ['udp']
    blockedWithPathPolicy.candidatePolicy.protocols.forbidden = ['tcp']
    expect(() => parseNetworkTopologyProfile(blockedWithPathPolicy)).toThrow(/cannot carry/u)

    const rewrittenConnectivity = cloneJson(registry.profiles[0]!)
    rewrittenConnectivity.connectivityExpectation = 'connectivity-blocked'
    rewrittenConnectivity.candidatePolicy = cloneJson(registry.profiles[1]!.candidatePolicy)
    expect(() => parseNetworkTopologyProfile(rewrittenConnectivity))
      .toThrow(/connectivity expectation/u)
  })
})

describe('browser network matrix runtime attestation contract', () => {
  const runId = 'contract-attestation-run'

  it('binds each satisfied proof to the manifest authority kind', () => {
    for (const profile of registry.manifest.profiles) {
      const raw = rawAttestation(registry, runId, profile.profileId, 'satisfied')
      const attestation = parseNetworkRuntimeAttestation(raw, attestationContext())
      expect(attestation.authorityId).toBe(profile.authorityId)
      expect(attestation.profileSha256).toBe(profile.profileSha256)
      expect(attestation.proof).not.toBeNull()
    }
  })

  it('rejects fabricated proof kinds, unsatisfied proof, and mismatched failure taxonomy', () => {
    const wrongKind = rawAttestation(registry, runId, 'scheduled-public-stun', 'satisfied')
    const wrongKindProof = wrongKind.proof as Record<string, unknown>
    wrongKindProof.proofKind = 'fabricated-proof-kind'
    expect(() => parseNetworkRuntimeAttestation(wrongKind, attestationContext()))
      .toThrow(/proof kind/u)

    const unavailableWithProof = rawAttestation(
      registry,
      runId,
      'scheduled-restricted-udp',
      'unavailable',
    )
    unavailableWithProof.proof = rawAttestation(
      registry,
      runId,
      'scheduled-restricted-udp',
      'satisfied',
    ).proof
    expect(() => parseNetworkRuntimeAttestation(unavailableWithProof, attestationContext()))
      .toThrow(/must be null/u)

    const invalidWithUnavailableCode = rawAttestation(
      registry,
      runId,
      'scheduled-coturn',
      'invalid',
    )
    ;(invalidWithUnavailableCode.failure as Record<string, unknown>).failureCode =
      'authority-not-provisioned'
    expect(() => parseNetworkRuntimeAttestation(invalidWithUnavailableCode, attestationContext()))
      .toThrow(/frozen vocabulary/u)
  })

  it('rejects mutations that invalidate the pinned local fixture trust proof', () => {
    const raw = cloneJson(rawAttestation(registry, runId, 'scheduled-coturn', 'satisfied'))
    const proof = raw.proof as Record<string, unknown>
    const externalFixtureTrust = proof.externalFixtureTrust as Record<string, unknown>
    externalFixtureTrust.attestationPublicKeySha256 = '0'.repeat(64)
    expect(() => parseNetworkRuntimeAttestation(raw, attestationContext()))
      .toThrow(/public-key/u)

    const extraClaim = cloneJson(rawAttestation(
      registry,
      runId,
      'scheduled-coturn',
      'satisfied',
    ))
    const extraProof = extraClaim.proof as Record<string, unknown>
    const extraTrust = extraProof.externalFixtureTrust as Record<string, unknown>
    extraTrust.platformClaims = ['local-only']
    expect(() => parseNetworkRuntimeAttestation(extraClaim, attestationContext()))
      .toThrow(/exactly/u)

    for (const [field, value] of [
      ['controllerOrigin', 'http://fixture.example.test/'],
      ['tlsCertificateSha256', 'f'.repeat(63)],
      ['tlsCertificateAuthoritySha256', 'F'.repeat(64)],
      [
        'attestationPublicKeySpki',
        `${TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SPKI.slice(0, -1)}A`,
      ],
    ] as const) {
      const mutated = cloneJson(rawAttestation(
        registry,
        runId,
        'scheduled-coturn',
        'satisfied',
      ))
      const mutatedTrust = ((mutated.proof as Record<string, unknown>)
        .externalFixtureTrust as Record<string, unknown>)
      mutatedTrust[field] = value
      expect(() => parseNetworkRuntimeAttestation(mutated, attestationContext())).toThrow()
    }

    const wrongAuthority = rawAttestation(
      registry,
      runId,
      'scheduled-coturn',
      'satisfied',
    )
    wrongAuthority.authorityId = 'public-stun-endpoint'
    expect(() => parseNetworkRuntimeAttestation(wrongAuthority, attestationContext()))
      .toThrow(/authority ID/u)
  })

  function attestationContext() {
    return Object.freeze({
      manifest: registry.manifest,
      manifestSha256: registry.manifestSha256,
      runId,
    })
  }
})
