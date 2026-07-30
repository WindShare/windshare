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

const MANIFEST_SHA256 = '4e57f971941aef9667f42531fdb4f903d89fbded9753afe660273efe0f6f4379'
const PROFILE_SHA256 = Object.freeze({
  'scheduled-public-stun': 'c4a10d8d5712307e29cde26ec26dadcec2ff89da293a4aa467ef656f2cb2b7e5',
  'scheduled-restricted-udp': '01f59210b0e92ee8b327714afe3daad86ee1ae167bbb792ade1f7b593b744e31',
  'scheduled-coturn': '1777486737a8e7e4f4286d788689ea6b9d50c2a60b0a54021815a43a7df96a90',
  'manual-real-nat': '2689011b60e2b16549725c13188abeadef106590f47829309c8c2099a9cc432f',
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

  it('freezes four profile authorities and exactly 60 canonical identities', () => {
    expect(registry.manifestSha256).toBe(MANIFEST_SHA256)
    expect(Object.fromEntries(registry.manifest.profiles.map(({ profileId, profileSha256 }) => [
      profileId,
      profileSha256,
    ]))).toEqual(PROFILE_SHA256)
    expect(registry.manifest.reportingSemantics).toBe('observational-nonblocking')

    const all = networkMatrixIdentities(registry.manifest)
    const scheduled = networkMatrixIdentities(registry.manifest, 'scheduled')
    const manual = networkMatrixIdentities(registry.manifest, 'manual')
    expect([all.length, scheduled.length, manual.length]).toEqual([60, 45, 15])
    expect(new Set(all.map(({ profileId, browser, sampleOrdinal }) =>
      `${profileId}/${browser}/${sampleOrdinal}`))).toHaveLength(60)
    expect(all[0]).toEqual({
      profileId: 'scheduled-public-stun',
      browser: 'chromium',
      sampleOrdinal: 1,
    })
    expect(all.at(-1)).toEqual({
      profileId: 'manual-real-nat',
      browser: 'webkit',
      sampleOrdinal: 5,
    })

    const hostileIdentities = networkMatrixIdentities as (
      manifest: LoadedNetworkMatrixRegistry['manifest'],
      mode: string,
    ) => readonly unknown[]
    expect(() => hostileIdentities(registry.manifest, 'nightly')).toThrow(/frozen vocabulary/u)
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
    const copiedManifest = join(copiedRegistry, 'manifest.v1.json')
    const profilePath = join(copiedRegistry, 'profiles', 'scheduled-public-stun.v1.json')
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
    const raw = cloneJson(rawAttestation(registry, runId, 'manual-real-nat', 'satisfied'))
    const proof = raw.proof as Record<string, unknown>
    const externalFixtureTrust = proof.externalFixtureTrust as Record<string, unknown>
    externalFixtureTrust.attestationPublicKeySha256 = '0'.repeat(64)
    expect(() => parseNetworkRuntimeAttestation(raw, attestationContext()))
      .toThrow(/public-key/u)

    const extraClaim = cloneJson(rawAttestation(
      registry,
      runId,
      'manual-real-nat',
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
        'manual-real-nat',
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
      'manual-real-nat',
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
