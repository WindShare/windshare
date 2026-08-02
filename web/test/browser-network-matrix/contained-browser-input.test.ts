import { access, mkdtemp, readdir, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { NetworkMatrixSampleExecutionContext } from '../../scripts/browser-network-matrix/runner.ts'
import {
  FilesystemContainedBrowserSampleInputAuthorityFactory,
} from '../../scripts/browser-network-matrix/linux-topology/contained-browser-input.ts'
import {
  loadContainedBrowserSampleSecret,
} from '../../scripts/browser-network-matrix/linux-topology/contained-browser-sample.ts'
import { loadRegistry } from './fixtures.ts'
import {
  TEST_FIXTURE_NOW,
  TEST_FIXTURE_PROBE_NONCE,
  testExternalFixtureControlCredential,
  testExternalFixtureControlCredentialAuthority,
  testFixtureAttestationPublicKeyPem,
  testNetworkMatrixExecutionAuthority,
} from './signed-fixture.ts'

const RUN_ID = 'contained-input-run'
const roots: string[] = []

beforeEach(() => { vi.spyOn(Date, 'now').mockReturnValue(TEST_FIXTURE_NOW) })
afterEach(async () => {
  await Promise.all(roots.splice(0).map((root) => rm(root, { recursive: true, force: true })))
  vi.restoreAllMocks()
})

describe('filesystem contained browser input authority', () => {
  it('places the credential only in one owned anonymous frame and erases it on close', async () => {
    const fixture = await authorityFiles()
    const factory = inputFactory(fixture.root, fixture.certificate, fixture.attestationPublicKey)
    const authority = await factory.acquire(
      await sampleContext(),
      new AbortController().signal,
    ).result
    const credential = testExternalFixtureControlCredential(
      RUN_ID,
      'scheduled-public-stun',
      TEST_FIXTURE_PROBE_NONCE,
    )
    const ownedDirectory = dirname(authority.sampleDirectory)
    const secretFrame = authority.secretFrame

    expect(Buffer.from(secretFrame).toString('utf8')).toContain(credential)
    expect(authority.containsSensitiveValue(Buffer.from(secretFrame).toString('utf8'))).toBe(true)
    expect(JSON.stringify(authority)).not.toContain(credential)
    expect(Object.hasOwn(authority, 'secretConfigPath')).toBe(false)
    const parsed = await loadContainedBrowserSampleSecret((async function* () {
      yield secretFrame
    })())
    try {
      expect(Buffer.from(parsed.control.credential).toString('utf8')).toBe(credential)
    } finally {
      parsed.control.credential.fill(0)
    }

    await authority.close().result
    await expect(access(ownedDirectory)).rejects.toThrow()
    expect(secretFrame.every((byte) => byte === 0)).toBe(true)
    await expect(authority.forceTerminateAndWait('sample-execute')).resolves.toBeUndefined()
  })

  it('cancels an in-flight authority resolver without leaving sample staging', async () => {
    const fixture = await authorityFiles()
    const controlFiles = vi.fn((_context, signal: AbortSignal) => new Promise<never>((_resolve, reject) => {
      signal.addEventListener('abort', () => reject(new Error('cancelled')), { once: true })
    }))
    const factory = new FilesystemContainedBrowserSampleInputAuthorityFactory({
      ...baseOptions(fixture.root),
      topologyFiles: topologyFiles,
      controlFiles,
    })
    const operation = factory.acquire(await sampleContext(), new AbortController().signal)
    await vi.waitFor(() => expect(controlFiles).toHaveBeenCalledOnce())

    await operation.forceTerminateAndWait('sample-execute')

    await expect(operation.result).rejects.toThrow('input acquisition failed')
    expect((await readdir(fixture.root)).sort()).toEqual([
      'attestation-public-key.pem',
      'certificate.pem',
    ])
  })

  it('detects the canonical lease credential after JSON serialization', async () => {
    const fixture = await authorityFiles()
    const authority = await inputFactory(
      fixture.root,
      fixture.certificate,
      fixture.attestationPublicKey,
    ).acquire(await sampleContext(), new AbortController().signal).result
    const credential = testExternalFixtureControlCredential(
      RUN_ID,
      'scheduled-public-stun',
      TEST_FIXTURE_PROBE_NONCE,
    )

    expect(authority.containsSensitiveValue(JSON.stringify({
      candidateStats: [{ diagnostic: credential }],
    }))).toBe(true)

    await authority.close().result
  })
})

function inputFactory(
  root: string,
  certificate: string,
  attestationPublicKey: string,
): FilesystemContainedBrowserSampleInputAuthorityFactory {
  return new FilesystemContainedBrowserSampleInputAuthorityFactory({
    ...baseOptions(root),
    topologyFiles,
    controlFiles: async () => ({
      controllerOrigin: 'https://browser-matrix.test/',
      tlsCertificateSha256: '2'.repeat(64),
      tlsCertificateAuthorityFile: certificate,
      attestationPublicKeyFile: attestationPublicKey,
    }),
  })
}

function baseOptions(root: string) {
  return Object.freeze({
    checkoutSha: '3'.repeat(40),
    temporaryRoot: root,
    controlCredentials: testExternalFixtureControlCredentialAuthority(),
    probeNonce: () => TEST_FIXTURE_PROBE_NONCE,
    attemptLeaseMs: 1_000,
    resultPollIntervalMs: 10,
    resultDeadlineMs: 500,
    challengeDeadlineMs: 500,
    cleanupDeadlineMs: 100,
  })
}

function topologyFiles() {
  return Object.freeze({
    topologyProfilePath: resolve('testdata', 'browser-network-matrix', 'profile.json'),
    topologyProfileSha256: '4'.repeat(64),
    topologyResolutionPath: resolve('testdata', 'browser-network-matrix', 'resolution.json'),
    topologyResolutionSha256: '5'.repeat(64),
  })
}

async function authorityFiles(): Promise<{
  readonly root: string
  readonly certificate: string
  readonly attestationPublicKey: string
}> {
  const root = await mkdtemp(join(tmpdir(), 'windshare-contained-input-test-'))
  roots.push(root)
  const certificate = join(root, 'certificate.pem')
  const attestationPublicKey = join(root, 'attestation-public-key.pem')
  await Promise.all([
    writeFile(certificate, 'test certificate authority', { mode: 0o600 }),
    writeFile(attestationPublicKey, testFixtureAttestationPublicKeyPem(), { mode: 0o600 }),
  ])
  return Object.freeze({ root, certificate, attestationPublicKey })
}

async function sampleContext(): Promise<NetworkMatrixSampleExecutionContext> {
  const registry = await loadRegistry()
  const profile = registry.profiles.find(({ profileId }) => profileId === 'scheduled-public-stun')
  if (profile === undefined) throw new Error('test registry lacks public STUN profile')
  return Object.freeze({
    runId: RUN_ID,
    manifestSha256: registry.manifestSha256,
    identity: Object.freeze({
      profileId: 'scheduled-public-stun',
      browser: 'chromium',
      sampleOrdinal: 1,
    }),
    profile,
    authority: testNetworkMatrixExecutionAuthority('scheduled-public-stun'),
    operationId: 'contained-input-run-public-chromium-1',
  })
}
