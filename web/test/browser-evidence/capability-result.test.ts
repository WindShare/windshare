import { readFile } from 'node:fs/promises'

import { beforeAll, describe, expect, it } from 'vitest'

import { artifactIdForManifest } from '../../scripts/browser-evidence/artifact/manifest.ts'
import type {
  AttemptEvidence,
  BrowserSelectedPairEvidence,
} from '../../scripts/browser-evidence/attempt-evidence.ts'
import {
  CAPABILITY_PROBE_DEADLINE_MS,
  classifyRtcCapability,
  parseCapabilityEvidence,
  provisionalCapabilityEvidence,
  rtcPeerConnectionApiPresent,
} from '../../scripts/browser-evidence/capability.ts'
import {
  MAIN_TRANSFER_BYTES,
  MAIN_TRANSFER_SHA256,
  classifyExecutionOutcome,
  parseBrowserSampleResult,
  validateMainAcceptance,
  validatePionAcceptance,
  type ArtifactKind,
  type BrowserSampleResult,
} from '../../scripts/browser-evidence/result.ts'
import { browserRunPolicy } from '../../scripts/browser-evidence/run-policy.ts'
import {
  parseTestIceTopologyJson,
  parseTestIceTopologyResolutionJson,
  testIceTopologyResolutionSha256,
  testIceTopologySha256,
  verifyTestIceTopologyLock,
  type TestIceTopology,
  type TestIceTopologyResolution,
  type VerifiedTestIceTopologyLock,
} from '../../scripts/browser-evidence/test-ice-topology.ts'
import { admittedEvents, browserPair, collect, identity, pionPair, TEST_IDENTITY } from './fixtures.ts'

let topology: TestIceTopology
let resolution: TestIceTopologyResolution
let profileSha256: string
let resolutionSha256: string
let topologyLock: VerifiedTestIceTopologyLock

beforeAll(async () => {
  const profileEncoded = await sharedFixture('pr-same-host-kernel-route-ipv4.json')
  const resolutionEncoded = await sharedFixture('pr-same-host-kernel-route-ipv4-resolution.json')
  topology = parseTestIceTopologyJson(profileEncoded)
  profileSha256 = await testIceTopologySha256(topology)
  resolution = parseTestIceTopologyResolutionJson(resolutionEncoded, topology, profileSha256)
  resolutionSha256 = await testIceTopologyResolutionSha256(resolution, topology, profileSha256)
  topologyLock = await verifyTestIceTopologyLock(
    topology,
    resolution,
    profileSha256,
    resolutionSha256,
  )
})

describe('runtime RTC capability contract', () => {
  it('reserves unavailable exclusively for API absence', () => {
    expect(rtcPeerConnectionApiPresent({})).toBe(false)
    expect(rtcPeerConnectionApiPresent({ RTCPeerConnection: class {} })).toBe(true)
    expect(classifyRtcCapability(provisionalCapabilityEvidence())).toBe('unknown')
    expect(classifyRtcCapability(parseCapabilityEvidence({
      schemaVersion: 1,
      apiPresence: 'absent',
      probeOutcome: 'not-started',
      probeDeadlineMs: CAPABILITY_PROBE_DEADLINE_MS,
    }))).toBe('unavailable')
    expect(classifyRtcCapability(parseCapabilityEvidence(availableCapability()))).toBe('available')
    expect(classifyRtcCapability(parseCapabilityEvidence(unusableCapability()))).toBe('unusable')
  })

  it('rejects probe results without API authority or complete failure evidence', () => {
    expect(() => parseCapabilityEvidence({
      schemaVersion: 1,
      apiPresence: 'absent',
      probeOutcome: 'failed',
      probeDeadlineMs: CAPABILITY_PROBE_DEADLINE_MS,
      failureCode: 'unexpected',
      failureMessage: 'should not run',
    })).toThrow(/cannot run/u)
    expect(() => parseCapabilityEvidence({
      schemaVersion: 1,
      apiPresence: 'present',
      probeOutcome: 'failed',
      probeDeadlineMs: CAPABILITY_PROBE_DEADLINE_MS,
    })).toThrow(/failure code/u)
    expect(() => parseCapabilityEvidence({
      ...availableCapability(),
      failureCode: undefined,
    })).toThrow(/must not be undefined/u)
  })
})

describe('browser sample result contract', () => {
  it('integrates runner authority, direct pair proof, route fence proof, and exact workload', () => {
    const parsed = parseResult(finalMainResult())
    expect(parsed).toMatchObject({
      suite: 'main',
      rtcCapability: 'available',
      peerAttemptOutcome: 'admitted',
      deliveryOutcome: 'succeeded',
      executionOutcome: 'healthy',
    })
    if (parsed.suite !== 'main') throw new Error('expected main result')
    expect(() => validateMainAcceptance(parsed, topologyLock)).not.toThrow()

    expect(() => parseResult({
      ...finalMainResult(),
      deliveryEvidence: { ...finalMainResult().deliveryEvidence, expectedBytes: 1 },
    })).toThrow(/16777216/u)
    expect(() => parseResult({
      ...finalMainResult(),
      deliveryEvidence: {
        ...finalMainResult().deliveryEvidence,
        expectedSha256: 'b'.repeat(64),
        receivedSha256: 'b'.repeat(64),
      },
    })).toThrow(/expected delivery SHA-256/u)
    expect(() => parseResult({
      ...finalMainResult(),
      executionEvidence: {
        ...finalMainResult().executionEvidence,
        runnerProcess: { terminal: 'exited', exitCode: 1 },
      },
    })).toThrow(/passed Playwright/u)
  })

  it('keeps runtime admission while the separate acceptance authority rejects missing proof', () => {
    const withoutPairs = parseResult({
      ...finalMainResult(),
      attempts: collect(admittedEvents({}, { selectedPairs: false })),
    })
    if (withoutPairs.suite !== 'main') throw new Error('expected main result')
    expect(withoutPairs.peerAttemptOutcome).toBe('admitted')
    expect(() => validateMainAcceptance(withoutPairs, topologyLock)).toThrow(/direct pair/u)

    const withoutRoute = parseResult({ ...finalMainResult(), routeEvidence: null })
    if (withoutRoute.suite !== 'main') throw new Error('expected main result')
    expect(() => validateMainAcceptance(withoutRoute, topologyLock)).toThrow(/hot-switch/u)
  })

  it('requires the browser remote endpoint to equal the authenticated Pion local endpoint', () => {
    const canonical = browserPair()
    const remoteWithoutAddress = omitProperties(canonical.remote, 'address')
    const remoteWithoutPort = omitProperties(canonical.remote, 'port')
    for (const remote of [
      remoteWithoutAddress,
      remoteWithoutPort,
      { ...canonical.remote, address: 'pion-endpoint.local' },
      { ...canonical.remote, address: '192.0.2.99' },
      { ...canonical.remote, port: 40_002 },
    ]) {
      const result = parseResult({
        ...finalMainResult(),
        attempts: admittedAttemptsWithBrowserPair({ ...canonical, remote }),
      })
      if (result.suite !== 'main') throw new Error('expected main result')
      expect(() => validateMainAcceptance(result, topologyLock)).toThrow(/direct pair/u)
    }
  })

  it('constrains browser-local privacy to host mDNS while retaining exact port correlation', () => {
    const canonical = browserPair()
    const mdns = parseResult({
      ...finalMainResult(),
      attempts: admittedAttemptsWithBrowserPair({
        ...canonical,
        local: { ...canonical.local, address: 'fixture-peer.local' },
      }),
    })
    if (mdns.suite !== 'main') throw new Error('expected main result')
    expect(() => validateMainAcceptance(mdns, topologyLock)).not.toThrow()

    const redactedLocal = omitProperties(canonical.local, 'address', 'port')
    const redacted = parseResult({
      ...finalMainResult(),
      attempts: admittedAttemptsWithBrowserPair({ ...canonical, local: redactedLocal }),
    })
    if (redacted.suite !== 'main') throw new Error('expected main result')
    expect(() => validateMainAcceptance(redacted, topologyLock)).not.toThrow()

    for (const local of [
      { ...canonical.local, address: 'untrusted-hostname.example' },
      { ...canonical.local, port: 40_002 },
    ]) {
      const result = parseResult({
        ...finalMainResult(),
        attempts: admittedAttemptsWithBrowserPair({ ...canonical, local }),
      })
      if (result.suite !== 'main') throw new Error('expected main result')
      expect(() => validateMainAcceptance(result, topologyLock)).toThrow(/direct pair/u)
    }
  })

  it('correlates hot-switch admission to the exact attempt and lane', () => {
    const route = hotSwitchRouteEvidence()
    expect(() => parseResult({
      ...finalMainResult(),
      routeEvidence: {
        ...route,
        observations: route.observations.map((observation) =>
          observation.kind === 'peer-admitted'
            ? { ...observation, attemptId: identity(99) }
            : observation),
      },
    })).toThrow(/does not identify/u)
  })

  it('rejects final-valid results that reuse a session lane ID or epoch', () => {
    const collisions = [
      { lane: { laneId: 7, laneEpoch: 9 }, expected: /lane ID 7 is reused/u },
      { lane: { laneId: 7, laneEpoch: 10 }, expected: /lane ID 7 is reused/u },
      { lane: { laneId: 8, laneEpoch: 9 }, expected: /lane epoch 9 is reused/u },
    ]
    for (const { lane, expected } of collisions) {
      expect(() => parseResult({
        ...finalMainResult(),
        attempts: mainAttemptsWithSecondLane(lane),
      })).toThrow(expected)
    }
  })

  it('accepts a final-valid result with independently fresh session lane allocations', () => {
    const parsed = parseResult({
      ...finalMainResult(),
      attempts: mainAttemptsWithSecondLane({ laneId: 8, laneEpoch: 10 }),
    })
    if (parsed.suite !== 'main') throw new Error('expected main result')
    expect(() => validateMainAcceptance(parsed, topologyLock)).not.toThrow()
  })

  it('keeps provisional and invalid evidence non-authoritative', () => {
    const provisional = provisionalMainResult()
    expect(parseResult(provisional).resultStatus).toBe('provisional')
    expect(() => parseResult({ ...provisional, peerAttemptOutcome: 'failed' })).toThrow(/must not assert/u)

    const invalid = {
      ...provisional,
      resultStatus: 'final-invalid',
      integrityViolations: ['browser side stream ended without a terminal'],
      executionOutcome: 'infrastructure-failed',
      executionEvidence: {
        ...provisional.executionEvidence,
        infrastructureFailure: true,
        runnerProcess: { terminal: 'exited', exitCode: 1 },
      },
      playwrightOutcome: 'failed',
    }
    expect(parseResult(invalid).resultStatus).toBe('final-invalid')
  })

  it('derives Pion applicability from API presence across all capability outcomes', () => {
    const available = parseResult(finalPionResult('available'))
    if (available.suite !== 'pion') throw new Error('expected Pion result')
    expect(validatePionAcceptance(available)).toBe('accepted')

    const unavailable = parseResult(finalPionResult('unavailable'))
    if (unavailable.suite !== 'pion') throw new Error('expected Pion result')
    expect(validatePionAcceptance(unavailable)).toBe('requires-main-relay-fallback')

    const unusable = parseResult(finalPionResult('unusable'))
    if (unusable.suite !== 'pion') throw new Error('expected Pion result')
    expect(unusable).toMatchObject({ applicability: 'applicable', nativeInteropOutcome: 'failed' })
    expect(() => validatePionAcceptance(unusable)).toThrow(/never accepted/u)

    const unknown = parseResult(finalInvalidPionUnknown())
    expect(unknown).toMatchObject({
      rtcCapability: 'unknown',
      applicability: 'unknown',
      nativeInteropOutcome: 'not-started',
    })
    expect(() => parseResult({
      ...finalInvalidPionUnknown(),
      applicability: 'applicable',
    })).toThrow(/API presence/u)
  })

  it('requires explicit same-attempt native browser/Pion evidence', () => {
    const pion = finalPionResult('available')
    if (pion.nativeInteropEvidence === null) throw new Error('expected native interop evidence')
    const nativeEvidence = pion.nativeInteropEvidence
    expect(() => parseResult({
      ...pion,
      nativeInteropEvidence: {
        ...nativeEvidence,
        pion: { ...nativeEvidence.pion, attemptId: identity(21) },
      },
    })).toThrow(/different attempts/u)
    expect(() => parseResult({
      ...pion,
      nativeInteropEvidence: {
        ...nativeEvidence,
        pion: {
          ...nativeEvidence.pion,
          selectedPair: {
            ...pionPair(),
            local: { ...pionPair().local, address: '192.0.2.11' },
          },
        },
      },
    })).toThrow(/correlated direct/u)

    const canonicalBrowserPair = browserPair()
    const remoteWithoutAddress = omitProperties(canonicalBrowserPair.remote, 'address')
    const remoteWithoutPort = omitProperties(canonicalBrowserPair.remote, 'port')
    for (const remote of [
      remoteWithoutAddress,
      remoteWithoutPort,
      { ...canonicalBrowserPair.remote, address: 'pion-endpoint.local' },
      { ...canonicalBrowserPair.remote, address: '192.0.2.99' },
      { ...canonicalBrowserPair.remote, port: 40_002 },
    ]) {
      expect(() => parseResult({
        ...pion,
        nativeInteropEvidence: {
          ...nativeEvidence,
          browser: {
            ...nativeEvidence.browser,
            selectedPair: { ...canonicalBrowserPair, remote },
          },
        },
      })).toThrow(/correlated direct/u)
    }

    expect(() => parseResult({
      ...pion,
      nativeInteropEvidence: {
        ...nativeEvidence,
        browser: {
          ...nativeEvidence.browser,
          selectedPair: {
            ...browserPair(),
            local: { ...browserPair().local, address: 'browser-host.local' },
          },
        },
      },
    })).not.toThrow()

    const redactedLocal = omitProperties(browserPair().local, 'address', 'port')
    expect(() => parseResult({
      ...pion,
      nativeInteropEvidence: {
        ...nativeEvidence,
        browser: {
          ...nativeEvidence.browser,
          selectedPair: { ...browserPair(), local: redactedLocal },
        },
      },
    })).not.toThrow()
  })

  it('binds results to canonically hashed profile and immutable resolution objects', async () => {
    await expect(verifyTestIceTopologyLock(
      topology,
      resolution,
      'f'.repeat(64),
      resolutionSha256,
    )).rejects.toThrow(/profile SHA/u)
    await expect(verifyTestIceTopologyLock(
      topology,
      resolution,
      profileSha256,
      'f'.repeat(64),
    )).rejects.toThrow(/resolution SHA/u)

    const alternateAddress = '192.0.2.11'
    const mutatedResolution = {
      ...resolution,
      probeResults: resolution.probeResults.map((probe) => ({
        ...probe,
        sourceAddress: alternateAddress,
      })),
      interface: {
        ...resolution.interface,
        selectedAddress: alternateAddress,
        eligibleAddresses: [
          ...resolution.interface.eligibleAddresses,
          { address: alternateAddress, prefixLength: 24 },
        ],
      },
    }
    await expect(verifyTestIceTopologyLock(
      topology,
      mutatedResolution,
      profileSha256,
      resolutionSha256,
    )).rejects.toThrow(/canonical resolution/u)
    expect(() => parseBrowserSampleResult(finalMainResult(), {
      profile: topology,
      resolution,
      profileSha256,
      resolutionSha256,
    } as VerifiedTestIceTopologyLock)).toThrow(/verified/u)
  })

  it('normalizes artifact and integrity diagnostics into deterministic order', () => {
    const artifacts = [artifact('runner-stderr', 'logs/z.txt'), artifact('runner-stdout', 'logs/a.txt')]
    const parsed = parseResult({
      ...finalMainResult(),
      artifacts,
    })
    expect(parsed.artifacts.map(({ artifactId }) => artifactId)).toEqual(
      artifacts.map(({ artifactId }) => artifactId).sort(),
    )

    const invalid = finalInvalidPionUnknown()
    const normalized = parseResult({
      ...invalid,
      integrityViolations: ['z violation', 'a violation'],
    })
    expect(normalized.integrityViolations).toEqual(['a violation', 'z violation'])
  })

  it('keeps collection paths and retired generation authority out of result contracts', () => {
    expect(() => parseResult({
      ...finalMainResult(),
      artifactGeneration: null,
    })).toThrow(/unknown field.*artifactGeneration/u)
    expect(() => parseResult({
      ...finalMainResult(),
      artifactRoot: 'C:/private/collection',
    })).toThrow(/unknown field.*artifactRoot/u)
  })

  it('classifies runner facts without inventing a browser crash from a signal', () => {
    expect(classifyExecutionOutcome({
      pageCrashed: true,
      targetCrashed: false,
      unexpectedBrowserDisconnect: false,
      infrastructureFailure: true,
      lifecycleCompleted: false,
      runnerProcess: { terminal: 'signaled', signal: 'SIGKILL' },
    })).toBe('crashed')
    expect(classifyExecutionOutcome({
      pageCrashed: false,
      targetCrashed: false,
      unexpectedBrowserDisconnect: false,
      infrastructureFailure: true,
      lifecycleCompleted: false,
      runnerProcess: { terminal: 'signaled', signal: 'SIGKILL' },
    })).toBe('infrastructure-failed')
  })
})

function parseResult(value: unknown): BrowserSampleResult {
  return parseBrowserSampleResult(value, topologyLock)
}

function provisionalMainResult() {
  return {
    ...provisionalCommon(),
    suite: 'main',
    peerAttemptOutcome: 'not-started',
    deliveryOutcome: 'not-started',
    attempts: [],
    deliveryEvidence: null,
    routeEvidence: null,
  }
}

function provisionalCommon() {
  return {
    schemaVersion: 1,
    resultStatus: 'provisional',
    runId: 'run-1',
    runPolicy: browserRunPolicy('blocking'),
    browser: 'chromium',
    sampleIndex: 1,
    checkoutSha: 'a'.repeat(40),
    topologyId: topology.topologyId,
    topologyProfileSha256: profileSha256,
    topologyResolutionSha256: resolutionSha256,
    rtcCapability: 'unknown',
    capabilityEvidence: provisionalCapabilityEvidence(),
    executionOutcome: 'unknown',
    executionEvidence: {
      pageCrashed: false,
      targetCrashed: false,
      unexpectedBrowserDisconnect: false,
      infrastructureFailure: false,
      lifecycleCompleted: false,
      runnerProcess: { terminal: 'not-started' },
    },
    playwrightOutcome: 'not-started',
    artifacts: [],
    integrityViolations: [],
  }
}

function finalCommon() {
  return {
    ...provisionalCommon(),
    resultStatus: 'final-valid',
    rtcCapability: 'available',
    capabilityEvidence: availableCapability(),
    executionOutcome: 'healthy',
    executionEvidence: {
      pageCrashed: false,
      targetCrashed: false,
      unexpectedBrowserDisconnect: false,
      infrastructureFailure: false,
      lifecycleCompleted: true,
      runnerProcess: { terminal: 'exited', exitCode: 0 },
    },
    playwrightOutcome: 'passed',
  }
}

function finalMainResult() {
  return {
    ...finalCommon(),
    suite: 'main',
    peerAttemptOutcome: 'admitted',
    deliveryOutcome: 'succeeded',
    attempts: collect(admittedEvents()),
    deliveryEvidence: {
      expectedBytes: MAIN_TRANSFER_BYTES,
      receivedBytes: MAIN_TRANSFER_BYTES,
      expectedSha256: MAIN_TRANSFER_SHA256,
      receivedSha256: MAIN_TRANSFER_SHA256,
      terminal: 'succeeded',
    },
    routeEvidence: hotSwitchRouteEvidence(),
  }
}

function admittedAttemptsWithBrowserPair(
  selectedPair: BrowserSelectedPairEvidence,
) {
  return collect(admittedEvents().map((event): AttemptEvidence =>
    event.side === 'browser' && event.stage === 'admitted'
      ? { ...event, selectedPair }
      : event))
}

function mainAttemptsWithSecondLane(lane: {
  readonly laneId: number
  readonly laneEpoch: number
}) {
  const first = collect(admittedEvents())
  const second = collect(admittedEvents(
    { attemptId: identity(11) },
    { lane },
  ))
  const receiveSequenceOffset = first.reduce(
    (count, attempt) => count + attempt.events.length,
    0,
  )
  return [
    ...first,
    ...second.map((attempt) => ({
      ...attempt,
      events: attempt.events.map((received) => ({
        ...received,
        receiveSequence: received.receiveSequence + receiveSequenceOffset,
      })),
    })),
  ]
}

function finalPionResult(capability: 'available' | 'unavailable' | 'unusable') {
  const common = {
    ...finalCommon(),
    rtcCapability: capability,
    capabilityEvidence: capabilityEvidenceFor(capability),
    suite: 'pion',
  }
  if (capability === 'unavailable') {
    return {
      ...common,
      applicability: 'not-applicable',
      nativeInteropOutcome: 'not-started',
      nativeInteropEvidence: null,
    }
  }
  const attemptId = identity(20)
  return {
    ...common,
    applicability: 'applicable',
    nativeInteropOutcome: capability === 'available' ? 'succeeded' : 'failed',
    nativeInteropEvidence: {
      browser: { attemptId, selectedPair: capability === 'available' ? browserPair() : null },
      pion: { attemptId, selectedPair: capability === 'available' ? pionPair() : null },
      ...(capability === 'unusable'
        ? { failureCode: 'negotiation', failureMessage: 'Probe failed before native negotiation' }
        : {}),
    },
  }
}

function capabilityEvidenceFor(capability: 'available' | 'unavailable' | 'unusable') {
  if (capability === 'available') return availableCapability()
  return capability === 'unavailable' ? unavailableCapability() : unusableCapability()
}

function omitProperties<Value extends object, Key extends keyof Value>(
  value: Value,
  ...keys: Key[]
): Omit<Value, Key> {
  const omitted = new Set<PropertyKey>(keys)
  return Object.fromEntries(
    Object.entries(value).filter(([name]) => !omitted.has(name)),
  ) as Omit<Value, Key>
}

function finalInvalidPionUnknown() {
  return {
    ...provisionalCommon(),
    resultStatus: 'final-invalid',
    integrityViolations: ['runner terminated before API presence classification'],
    executionOutcome: 'infrastructure-failed',
    executionEvidence: {
      ...provisionalCommon().executionEvidence,
      infrastructureFailure: true,
      runnerProcess: { terminal: 'spawn-failed', errorCode: 'ENOENT', errorMessage: 'runner missing' },
    },
    suite: 'pion',
    applicability: 'unknown',
    nativeInteropOutcome: 'not-started',
    nativeInteropEvidence: null,
  }
}

function hotSwitchRouteEvidence() {
  const peerLane = { laneId: 7, laneEpoch: 9 }
  return {
    mode: 'hot-switch',
    observations: [
      {
        observationSequence: 1,
        kind: 'dispatch',
        dispatchSequence: 1,
        route: 'relay',
        lane: { laneId: 1, laneEpoch: 0 },
      },
      {
        observationSequence: 2,
        kind: 'peer-admitted',
        ...TEST_IDENTITY,
        lane: peerLane,
      },
      {
        observationSequence: 3,
        kind: 'relay-cut-fence',
        dispatchSequenceBoundary: 1,
        proxyAccepting: false,
        receiverRelayEligible: false,
      },
      {
        observationSequence: 4,
        kind: 'dispatch',
        dispatchSequence: 2,
        route: 'peer',
        lane: peerLane,
      },
    ],
  }
}

function availableCapability() {
  return {
    schemaVersion: 1,
    apiPresence: 'present',
    probeOutcome: 'succeeded',
    probeDeadlineMs: CAPABILITY_PROBE_DEADLINE_MS,
  }
}

function unavailableCapability() {
  return {
    schemaVersion: 1,
    apiPresence: 'absent',
    probeOutcome: 'not-started',
    probeDeadlineMs: CAPABILITY_PROBE_DEADLINE_MS,
  }
}

function unusableCapability() {
  return {
    schemaVersion: 1,
    apiPresence: 'present',
    probeOutcome: 'failed',
    probeDeadlineMs: CAPABILITY_PROBE_DEADLINE_MS,
    failureCode: 'offer-creation',
    failureMessage: 'createOffer rejected',
  }
}

async function sharedFixture(name: string): Promise<string> {
  return readFile(new URL(`../../../testdata/test-ice-topology/${name}`, import.meta.url), 'utf8')
}

function artifact(kind: ArtifactKind, relativePath: string) {
  const manifest = {
    kind,
    relativePath,
    mediaType: 'text/plain',
    byteLength: 1,
    sha256: 'c'.repeat(64),
  }
  return { artifactId: artifactIdForManifest(manifest), ...manifest }
}
