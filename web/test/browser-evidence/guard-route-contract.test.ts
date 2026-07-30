import { describe, expect, it } from 'vitest'

import {
  parseExecutionEvidence,
  RUNNER_MAXIMUM_EXIT_CODE,
} from '../../scripts/browser-evidence/execution-evidence.ts'
import {
  ARTIFACT_GUARD_SCHEMA_VERSION,
  GUARD_MAXIMUM_ARCHIVE_BYTES,
  GUARD_MAXIMUM_ARCHIVE_ENTRIES,
  GUARD_MAXIMUM_ARCHIVE_NESTING_DEPTH,
  GUARD_MAXIMUM_EXPANDED_ARCHIVE_BYTES,
  parseArtifactGuardResult,
  validateArtifactGuardForSample,
} from '../../scripts/browser-evidence/artifact/guard-result.ts'
import { artifactManifestSha256 } from '../../scripts/browser-evidence/artifact/manifest.ts'
import { parseMainRouteEvidence } from '../../scripts/browser-evidence/route-evidence.ts'
import type { BrowserSampleResult } from '../../scripts/browser-evidence/result.ts'
import { browserRunPolicy } from '../../scripts/browser-evidence/run-policy.ts'
import { TEST_IDENTITY } from './fixtures.ts'

const WINDOWS_CONTROL_C_EXIT_CODE = 0xc000_013a

describe('runner-derived execution evidence', () => {
  it('rejects lifecycle facts that cannot follow the runner terminal', () => {
    expect(parseExecutionEvidence({
      ...executionEvidence(),
      runnerProcess: { terminal: 'exited', exitCode: WINDOWS_CONTROL_C_EXIT_CODE },
    }).runnerProcess).toEqual({ terminal: 'exited', exitCode: WINDOWS_CONTROL_C_EXIT_CODE })
    expect(() => parseExecutionEvidence({
      ...executionEvidence(),
      runnerProcess: { terminal: 'exited', exitCode: RUNNER_MAXIMUM_EXIT_CODE + 1 },
    })).toThrow(/exit code/u)
    expect(() => parseExecutionEvidence({
      ...executionEvidence(),
      lifecycleCompleted: true,
      runnerProcess: { terminal: 'not-started' },
    })).toThrow(/never started/u)
    expect(() => parseExecutionEvidence({
      ...executionEvidence(),
      infrastructureFailure: false,
      runnerProcess: { terminal: 'spawn-failed', errorCode: 'ENOENT', errorMessage: 'missing' },
    })).toThrow(/infrastructure/u)
    expect(() => parseExecutionEvidence({
      ...executionEvidence(),
      infrastructureFailure: false,
      runnerProcess: { terminal: 'signaled', signal: 'SIGKILL' },
    })).toThrow(/infrastructure/u)
  })
})

describe('relay cut route evidence', () => {
  it('proves dispatch-time relay, correlated admission, cut fence, and post-fence peer work', () => {
    expect(parseMainRouteEvidence(hotSwitchRoute())).toMatchObject({ mode: 'hot-switch' })
    expect(() => parseMainRouteEvidence({
      ...hotSwitchRoute(),
      observations: hotSwitchRoute().observations.map((observation) =>
        observation.observationSequence === 4
          ? { ...observation, route: 'relay', lane: { ...observation.lane, laneEpoch: 0 } }
          : observation),
    })).toThrow(/post-fence peer dispatch/u)
    expect(() => parseMainRouteEvidence({
      ...hotSwitchRoute(),
      observations: hotSwitchRoute().observations.map((observation) =>
        observation.kind === 'relay-cut-fence'
          ? { ...observation, dispatchSequenceBoundary: 2 }
          : observation),
    })).toThrow(/cut fence/u)
    expect(() => parseMainRouteEvidence({
      ...hotSwitchRoute(),
      observations: hotSwitchRoute().observations.map((observation) =>
        observation.observationSequence === 1
          ? { ...observation, route: 'peer', lane: { ...observation.lane, laneEpoch: 1 } }
          : observation),
    })).toThrow(/relay, admission/u)
    const route = hotSwitchRoute()
    expect(() => parseMainRouteEvidence({
      ...route,
      observations: [
        ...route.observations.slice(0, 3),
        {
          observationSequence: 4,
          kind: 'dispatch',
          dispatchSequence: 2,
          route: 'peer',
          lane: { laneId: 99, laneEpoch: 1 },
        },
        { ...route.observations[3]!, observationSequence: 5, dispatchSequence: 3 },
      ],
    })).toThrow(/post-fence peer dispatch/u)
  })

  it('preserves protocol relay epoch zero without weakening authenticated peer epochs', () => {
    expect(() => parseMainRouteEvidence({
      mode: 'relay-only',
      observations: [{
        observationSequence: 1,
        kind: 'dispatch',
        dispatchSequence: 1,
        route: 'relay',
        lane: { laneId: 1, laneEpoch: 0 },
      }],
    })).not.toThrow()

    const route = hotSwitchRoute()
    expect(() => parseMainRouteEvidence({
      ...route,
      observations: route.observations.map((observation) =>
        observation.kind === 'peer-admitted'
          ? { ...observation, lane: { ...observation.lane, laneEpoch: 0 } }
          : observation),
    })).toThrow(/\[1, 4294967295\]/u)
    expect(() => parseMainRouteEvidence({
      ...route,
      observations: route.observations.map((observation) =>
        observation.kind === 'dispatch' && observation.route === 'peer'
          ? { ...observation, lane: { ...observation.lane, laneEpoch: 0 } }
          : observation),
    })).toThrow(/\[1, 4294967295\]/u)
    expect(() => parseMainRouteEvidence({
      mode: 'relay-only',
      observations: [{
        observationSequence: 1,
        kind: 'dispatch',
        dispatchSequence: 1,
        route: 'relay',
        lane: { laneId: 1, laneEpoch: -1 },
      }],
    })).toThrow(/\[0, 0\]/u)
    expect(() => parseMainRouteEvidence({
      mode: 'relay-only',
      observations: [{
        observationSequence: 1,
        kind: 'dispatch',
        dispatchSequence: 1,
        route: 'relay',
        lane: { laneId: 1, laneEpoch: 1 },
      }],
    })).toThrow(/\[0, 0\]/u)
  })
})

describe('separate fail-closed artifact guard contract', () => {
  it('freezes limits and canonicalizes every artifact-derived ordering', () => {
    const parsed = parseArtifactGuardResult({
      ...passedGuard(),
      scanEvidence: { ...scanEvidence(), scannedFileCount: 2 },
      checkedArtifactIds: ['trace-z', 'trace-a'],
      uploadableArtifactIds: ['trace-z', 'trace-a'],
    })
    expect(parsed.checkedArtifactIds).toEqual(['trace-a', 'trace-z'])
    expect(parsed.uploadableArtifactIds).toEqual(['trace-a', 'trace-z'])
    expect(() => parseArtifactGuardResult({
      ...passedGuard(),
      scanEvidence: { ...scanEvidence(), maximumArchiveEntries: 99 },
    })).toThrow(/10000/u)
    expect(() => parseArtifactGuardResult({
      ...passedGuard(),
      scanEvidence: {
        ...scanEvidence(),
        scannedArchiveEntryCount: GUARD_MAXIMUM_ARCHIVE_ENTRIES + 1,
      },
    })).toThrow(/exceeds/u)
  })

  it('requires typed causal limit failures and quarantines the complete checked set', () => {
    expect(parseArtifactGuardResult(failedGuard(
      'archive-nesting-limit',
      { observedMaximumArchiveDepth: GUARD_MAXIMUM_ARCHIVE_NESTING_DEPTH + 1 },
    ))).toMatchObject({ guardOutcome: 'failed', uploadableArtifactIds: [] })
    expect(() => parseArtifactGuardResult(failedGuard(
      'archive-entry-limit',
      { scannedArchiveEntryCount: GUARD_MAXIMUM_ARCHIVE_ENTRIES },
    ))).toThrow(/lacks causal/u)
    expect(() => parseArtifactGuardResult({
      ...failedGuard('scanner-crashed'),
      quarantinedArtifactIds: [],
    })).toThrow(/fail closed/u)
  })

  it('rejects traversal paths and correlates the guard to the exact sample artifact index', () => {
    expect(() => parseArtifactGuardResult({
      ...quarantinedGuard(),
      matches: [{
        artifactId: 'trace-a',
        location: 'archive-entry',
        archiveEntryPath: '../secret.txt',
        detector: 'explicit-secret',
      }],
    })).toThrow(/normalized relative/u)

    const guard = parseArtifactGuardResult(passedGuard())
    expect(() => validateArtifactGuardForSample(
      guard,
      sample(['trace-a']),
      SAMPLE_RESULT_SHA256,
    )).not.toThrow()
    expect(() => validateArtifactGuardForSample(
      guard,
      sample(['trace-a', 'trace-z']),
      SAMPLE_RESULT_SHA256,
    )).toThrow(/canonical full sample artifact manifest/u)
    expect(() => validateArtifactGuardForSample(
      parseArtifactGuardResult(failedGuard('scanner-crashed')),
      sample(['trace-a']),
      SAMPLE_RESULT_SHA256,
    )).toThrow(/did not authorize/u)
  })
})

function executionEvidence() {
  return {
    pageCrashed: false,
    targetCrashed: false,
    unexpectedBrowserDisconnect: false,
    infrastructureFailure: false,
    lifecycleCompleted: false,
    runnerProcess: { terminal: 'not-started' },
  }
}

function hotSwitchRoute() {
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
        lane: { laneId: 7, laneEpoch: 9 },
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
        lane: { laneId: 7, laneEpoch: 9 },
      },
    ],
  }
}

function scanEvidence() {
  return {
    terminal: 'completed',
    scannedFileCount: 1,
    scannedArchiveEntryCount: 0,
    observedArchiveBytes: 0,
    expandedArchiveBytes: 0,
    observedMaximumArchiveDepth: 0,
    maximumArchiveBytes: GUARD_MAXIMUM_ARCHIVE_BYTES,
    maximumArchiveEntries: GUARD_MAXIMUM_ARCHIVE_ENTRIES,
    maximumExpandedArchiveBytes: GUARD_MAXIMUM_EXPANDED_ARCHIVE_BYTES,
    maximumArchiveNestingDepth: GUARD_MAXIMUM_ARCHIVE_NESTING_DEPTH,
  }
}

function passedGuard() {
  const artifacts = sample(['trace-a']).artifacts
  return {
    schemaVersion: ARTIFACT_GUARD_SCHEMA_VERSION,
    runId: 'run-1',
    runPolicy: browserRunPolicy('closure'),
    suite: 'main',
    browser: 'chromium',
    sampleIndex: 1,
    checkoutSha: 'a'.repeat(40),
    sampleResultSha256: SAMPLE_RESULT_SHA256,
    artifactManifestSha256: artifactManifestSha256(artifacts),
    guardOutcome: 'passed',
    scanEvidence: scanEvidence(),
    checkedArtifactIds: ['trace-a'],
    uploadableArtifactIds: ['trace-a'],
    quarantinedArtifactIds: [],
    matches: [],
  }
}

const SAMPLE_RESULT_SHA256 = 'd'.repeat(64)

function quarantinedGuard() {
  return {
    ...passedGuard(),
    guardOutcome: 'quarantined',
    uploadableArtifactIds: [],
    quarantinedArtifactIds: ['trace-a'],
    matches: [{
      artifactId: 'trace-a',
      location: 'file',
      archiveEntryPath: null,
      detector: 'explicit-secret',
    }],
  }
}

function failedGuard(
  failureCode: string,
  scanChanges: Record<string, unknown> = {},
) {
  return {
    ...passedGuard(),
    guardOutcome: 'failed',
    scanEvidence: { ...scanEvidence(), terminal: 'failed', ...scanChanges },
    uploadableArtifactIds: [],
    quarantinedArtifactIds: ['trace-a'],
    failureCode,
    failureMessage: 'guard failed closed',
  }
}

function sample(artifactIds: readonly string[]): BrowserSampleResult {
  return {
    runId: 'run-1',
    runPolicy: browserRunPolicy('closure'),
    suite: 'main',
    browser: 'chromium',
    sampleIndex: 1,
    checkoutSha: 'a'.repeat(40),
    artifacts: artifactIds.map((artifactId) => ({
      artifactId,
      kind: 'trace',
      relativePath: `playwright/${artifactId}.zip`,
      mediaType: 'application/zip',
      byteLength: 1,
      sha256: 'e'.repeat(64),
    })),
  } as unknown as BrowserSampleResult
}
