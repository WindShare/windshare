import { describe, expect, it, vi } from 'vitest'

import {
  CONTAINED_BROWSER_SAMPLE_OUTPUT_SCHEMA,
  CONTAINED_BROWSER_SAMPLE_SECRET_SCHEMA,
  challengeFrame,
  parseContainedBrowserSampleSecret,
  runContainedBrowserSample,
  type ContainedBrowserPionControl,
  type ContainedBrowserSampleDependencies,
  type ContainedBrowserSampleSecret,
  type ContainedBrowserSession,
} from '../../scripts/browser-network-matrix/linux-topology/contained-browser-sample.ts'
import { containedBrowserSampleChild } from '../../scripts/browser-network-matrix/linux-topology/sample-child.ts'
import { externalStunRtcConfiguration } from '../../scripts/browser-network-matrix/runtime-authority.ts'
import type { NetworkMatrixBrowser } from '../../scripts/browser-network-matrix/vocabulary.ts'
import {
  TEST_BROWSER_PUBLIC_IP,
  TEST_FIXTURE_NOW,
  TEST_REMOTE_PEER_PUBLIC_IP,
  testExternalFixtureProof,
  testFixtureAttestationPublicKeyPem,
  testNetworkMatrixAttemptEvidence,
} from './signed-fixture.ts'

const ATTEMPT_ID = 'attempt-012345678'
const CHALLENGE = 'challenge-01234567'
const REQUEST_ID = 'request-012345678'
const CONTROL_CREDENTIAL = 'controlCredentialAuthority0123456789'
const RUN_ID = 'contained-browser-run'
const PROCESS_INSTANCE_ID = 'browser-0123456789abcdef'
const FIXTURE_ISSUED_AT = '2026-07-29T00:00:00.000Z'
const FIXTURE_EXPIRES_AT = '2026-07-29T00:02:00.000Z'

describe('contained browser sample', () => {
  it('rejects legacy profile-level RTC configuration in a sample secret', () => {
    const valid = secret()
    const value = {
      ...valid,
      rtcConfiguration: {
        iceServers: [{
          urls: ['turn:turn.invalid:3478'],
          username: 'matrix-user',
          credential: null,
        }],
      },
    }

    expect(() => parseContainedBrowserSampleSecret(value)).toThrow(
      'contained browser secret authority is invalid',
    )
  })

  it('launches the requested engine, proves the exact challenge, and reaps the attempt', async () => {
    const fixture = harness()
    const output = await runContainedBrowserSample({
      browser: 'chromium',
      secret: fixture.secret,
      signal: new AbortController().signal,
      dependencies: fixture.dependencies,
    })

    expect(fixture.launch).toHaveBeenCalledWith('chromium')
    expect(fixture.session.exchangeChallenge).toHaveBeenCalledWith(
      challengeFrame(fixture.attemptLease),
      500,
    )
    expect(fixture.control.deleteAttempt).toHaveBeenCalledWith(
      ATTEMPT_ID,
      expect.any(AbortSignal),
    )
    expect(fixture.session.close).toHaveBeenCalledOnce()
    expect(output).toMatchObject({
      processInstanceId: PROCESS_INSTANCE_ID,
      browser: 'chromium',
      protocolResult: { state: 'established', challengeEchoed: true },
    })
  })

  it('rejects a launcher that returns a different engine and still closes it', async () => {
    const fixture = harness({ engine: 'firefox' })

    await expect(runContainedBrowserSample({
      browser: 'chromium',
      secret: fixture.secret,
      signal: new AbortController().signal,
      dependencies: fixture.dependencies,
    })).rejects.toThrow('wrong engine')
    expect(fixture.control.createAttempt).not.toHaveBeenCalled()
    expect(fixture.session.close).toHaveBeenCalledOnce()
  })

  it('rejects a non-exact challenge echo and reaps both owners', async () => {
    const fixture = harness({
      browser: 'webkit',
      engine: 'webkit',
      echoedFrame: '{"wrong":true}',
    })

    await expect(runContainedBrowserSample({
      browser: 'webkit',
      secret: fixture.secret,
      signal: new AbortController().signal,
      dependencies: fixture.dependencies,
    })).rejects.toThrow('application path was not established')
    expect(fixture.control.deleteAttempt).toHaveBeenCalledOnce()
    expect(fixture.session.close).toHaveBeenCalledOnce()
  })

  it('makes a cleanup receipt failure terminal after an otherwise valid sample', async () => {
    const fixture = harness({ browser: 'firefox', engine: 'firefox', deleteFailure: true })

    await expect(runContainedBrowserSample({
      browser: 'firefox',
      secret: fixture.secret,
      signal: new AbortController().signal,
      dependencies: fixture.dependencies,
    })).rejects.toThrow('cleanup did not prove terminal reaping')
    expect(fixture.session.close).toHaveBeenCalledOnce()
  })

  it('does not mistake the request ID for cleanup authority when create fails', async () => {
    const fixture = harness({ createFailure: true })

    await expect(runContainedBrowserSample({
      browser: 'chromium',
      secret: fixture.secret,
      signal: new AbortController().signal,
      dependencies: fixture.dependencies,
    })).rejects.toThrow('create response lost')
    expect(fixture.control.deleteAttempt).not.toHaveBeenCalled()
    expect(fixture.session.close).toHaveBeenCalledOnce()
  })

  it('never reflects a credential from a rejected secret', () => {
    const value = JSON.parse(JSON.stringify(secret())) as Record<string, unknown>
    const control = value.control as Record<string, unknown>
    control.credential = `invalid:${CONTROL_CREDENTIAL}`

    expect(() => parseContainedBrowserSampleSecret(value)).toThrow(
      'contained browser secret authority is invalid',
    )
    try {
      parseContainedBrowserSampleSecret(value)
    } catch (cause) {
      expect(String(cause)).not.toContain(CONTROL_CREDENTIAL)
    }
  })

  it('runs only the exact requested child engine and emits one JSON line', async () => {
    const loadSecret = vi.fn().mockResolvedValue(secret('firefox'))
    const output = {
      schemaVersion: CONTAINED_BROWSER_SAMPLE_OUTPUT_SCHEMA,
      processInstanceId: PROCESS_INSTANCE_ID,
      browser: 'firefox' as const,
      protocolResult: {
        attemptId: ATTEMPT_ID,
        state: 'established' as const,
        selectedPair: {},
        challengeBindingSha256: '4'.repeat(64),
        failureCode: null,
        challengeEchoed: true,
      },
      browserSelectedPair: {
        selectedPair: 'absent',
        localCandidateType: null,
        localAddress: null,
        localPort: null,
        remoteCandidateType: null,
        remoteAddress: null,
        remotePort: null,
        protocol: null,
      },
    }
    const run = vi.fn().mockResolvedValue(output)
    const writeOutput = vi.fn()

    await expect(containedBrowserSampleChild([
      '--browser', 'firefox',
    ], { loadSecret, run, writeOutput })).resolves.toBe(output)

    expect(loadSecret).toHaveBeenCalledOnce()
    expect(run).toHaveBeenCalledWith(expect.objectContaining({ browser: 'firefox' }))
    expect(writeOutput).toHaveBeenCalledOnce()
    expect(writeOutput).toHaveBeenCalledWith(`${JSON.stringify(output)}\n`)
  })

  it('rejects an inexact child engine before reading the secret authority', async () => {
    const loadSecret = vi.fn().mockResolvedValue(secret())

    await expect(containedBrowserSampleChild([
      '--browser', 'chromium-with-fallback',
    ], { loadSecret })).rejects.toThrow('contained browser network matrix child failed')

    expect(loadSecret).not.toHaveBeenCalled()
  })
})

interface HarnessOptions {
  readonly browser?: NetworkMatrixBrowser
  readonly engine?: string
  readonly echoedFrame?: string
  readonly deleteFailure?: boolean
  readonly createFailure?: boolean
}

function harness(options: HarnessOptions = {}) {
  const browser = options.browser ?? 'chromium'
  const sampleSecret = secret(browser)
  const attemptEvidence = testNetworkMatrixAttemptEvidence(RUN_ID, {
    profileId: 'scheduled-public-stun',
    browser,
    sampleOrdinal: 1,
  }, {
    attemptId: ATTEMPT_ID,
    challenge: CHALLENGE,
    processInstanceId: PROCESS_INSTANCE_ID,
  })
  const externalFixture = testExternalFixtureProof(RUN_ID, 'scheduled-public-stun')
  const terminal = attemptEvidence.terminalReceipt.receipt
  const attemptLease = Object.freeze({
    attemptAuthority: attemptEvidence.attemptAuthority,
    leaseIssuedAt: FIXTURE_ISSUED_AT,
    leaseExpiresAt: FIXTURE_EXPIRES_AT,
    leaseMillis: 1_000,
  })
  const session: ContainedBrowserSession = {
    engine: options.engine ?? browser,
    createOffer: vi.fn().mockResolvedValue('offer-sdp'),
    acceptAnswer: vi.fn().mockResolvedValue(undefined),
    exchangeChallenge: vi.fn(async (frame: string) => options.echoedFrame ?? frame),
    getStats: vi.fn().mockResolvedValue(publicStats()),
    close: vi.fn().mockResolvedValue(undefined),
  }
  const control: ContainedBrowserPionControl = {
    probeFixture: vi.fn().mockResolvedValue({
      outcome: 'satisfied',
      runId: RUN_ID,
      profileId: 'scheduled-public-stun',
      ...externalFixture,
      rtcConfiguration: externalStunRtcConfiguration('stun:stun.invalid:3478'),
    }),
    createAttempt: options.createFailure
      ? vi.fn().mockRejectedValue(new Error('create response lost'))
      : vi.fn().mockResolvedValue(attemptLease),
    offer: vi.fn().mockResolvedValue('answer-sdp'),
    result: vi.fn().mockResolvedValue({
      attemptAuthority: attemptEvidence.attemptAuthority,
      state: terminal.state,
      selectedPair: terminal.selectedPair,
      challengeBindingSha256: terminal.challengeBindingSha256,
      failureCode: terminal.failureCode,
      terminalReceipt: attemptEvidence.terminalReceipt,
    }),
    deleteAttempt: options.deleteFailure
      ? vi.fn().mockRejectedValue(new Error('cleanup failed'))
      : vi.fn().mockResolvedValue(undefined),
  }
  const launch = vi.fn().mockResolvedValue(session)
  const dependencies: ContainedBrowserSampleDependencies = Object.freeze({
    launch,
    control: () => control,
    requestId: () => REQUEST_ID,
    delay: vi.fn().mockResolvedValue(undefined),
    now: () => TEST_FIXTURE_NOW,
  })
  return { session, control, launch, dependencies, secret: sampleSecret, attemptLease }
}

function secret(browser: NetworkMatrixBrowser = 'chromium'): ContainedBrowserSampleSecret {
  const evidence = testNetworkMatrixAttemptEvidence(RUN_ID, {
    profileId: 'scheduled-public-stun',
    browser,
    sampleOrdinal: 1,
  }, {
    attemptId: ATTEMPT_ID,
    challenge: CHALLENGE,
    processInstanceId: PROCESS_INSTANCE_ID,
  })
  const fixture = testExternalFixtureProof(RUN_ID, 'scheduled-public-stun')
  return parseContainedBrowserSampleSecret({
    schemaVersion: CONTAINED_BROWSER_SAMPLE_SECRET_SCHEMA,
    expectedConnectivity: 'established',
    control: {
      controllerOrigin: 'https://pion.invalid:8443/',
      controlLease: {
        controlAuthority: evidence.attemptAuthority.requestAuthority.controlAuthority,
        probeNonce: 'probeNonce012345678',
        authorityInstanceId: fixture.authorityInstanceId,
        attestationSha256: fixture.attestationSha256,
        issuedAt: FIXTURE_ISSUED_AT,
        expiresAt: FIXTURE_EXPIRES_AT,
        maxAttempts: 1,
      },
      tlsCertificateAuthority: 'test certificate authority',
      tlsCertificateSha256: '2'.repeat(64),
      attestationPublicKey: testFixtureAttestationPublicKeyPem(),
      credential: Buffer.from(CONTROL_CREDENTIAL, 'utf8'),
    },
    attemptLeaseMs: 1_000,
    resultPollIntervalMs: 10,
    resultDeadlineMs: 500,
    challengeDeadlineMs: 500,
    cleanupDeadlineMs: 100,
  })
}

function publicStats(): readonly Record<string, unknown>[] {
  return Object.freeze([
    { id: 'transport', type: 'transport', selectedCandidatePairId: 'pair' },
    {
      id: 'pair',
      type: 'candidate-pair',
      localCandidateId: 'local',
      remoteCandidateId: 'remote',
      selected: true,
      nominated: true,
      state: 'succeeded',
      protocol: 'udp',
    },
    {
      id: 'local',
      type: 'local-candidate',
      candidateType: 'srflx',
      address: TEST_BROWSER_PUBLIC_IP,
      ip: null,
      port: 50_000,
      protocol: 'udp',
    },
    {
      id: 'remote',
      type: 'remote-candidate',
      candidateType: 'host',
      address: TEST_REMOTE_PEER_PUBLIC_IP,
      ip: null,
      port: 40_000,
      protocol: 'udp',
    },
  ])
}
