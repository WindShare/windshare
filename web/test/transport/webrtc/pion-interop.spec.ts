import { mkdir, writeFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'

import { expect, test, type Page, type TestInfo } from '@playwright/test'

import {
  CHILD_EVIDENCE_CONTEXT_ENV,
  ChildEvidenceReporter,
} from '../../../scripts/browser-evidence/child-evidence'
import type * as BrowserHarness from './browser-harness'
import { classifyNativePeerConnection } from './browser-capability'
import {
  executePionFocusedSample,
  type PionFocusedObservation,
  type PionSucceededObservation,
} from './pion-focused-consumer'

const HARNESS_PATH = '/test/transport/webrtc/browser-harness.ts'
const MAX_FRAME_BYTES = 65_536
const HIGH_WATER_BYTES = 1024 * 1024
const NATIVE_EVIDENCE_RELATIVE_PATH = 'pion/native-interop-evidence.json'

test('production browser adapter interoperates with the accepted Pion adapter', async ({
  browserName,
  page,
}, testInfo) => {
  const reporter = optionalChildReporter()
  try {
    await runPionSample(page, browserName, testInfo, reporter)
  } finally {
    if (reporter !== null) {
      reporter.completeLifecycle()
      await reporter.flush()
    }
  }
})

async function runPionSample(
  page: Page,
  browserName: string,
  testInfo: TestInfo,
  reporter: ChildEvidenceReporter | null,
): Promise<void> {
  validateReporterIdentity(reporter, browserName, testInfo)
  const capability = await classifyNativePeerConnection(page)
  reporter?.recordCapability(capability.evidence)
  await executePionFocusedSample(capability, {
    readPionTopology: () =>
      page.evaluate(async (path) => {
        const harness = await import(path) as typeof BrowserHarness
        return harness.readPionTopology()
      }, HARNESS_PATH),
    runPionInteropSample: () =>
      page.evaluate(async (path) => {
        const harness = await import(path) as typeof BrowserHarness
        return harness.runPionInteropSample()
      }, HARNESS_PATH),
    retainObservation: (observation) =>
      retainPionObservation(testInfo, reporter, observation),
    verifySuccessfulInterop,
  })
}

async function retainPionObservation(
  testInfo: TestInfo,
  reporter: ChildEvidenceReporter | null,
  observation: PionFocusedObservation,
): Promise<void> {
  validateReporterTopology(reporter, observation)
  if (observation.nativeInteropOutcome !== 'not-started') {
    reporter?.recordNativeInterop(
      observation.nativeInteropOutcome,
      observation.nativeInteropEvidence,
    )
  }
  await retainNativeEvidence(testInfo, reporter, observation)
  if (observation.nativeInteropOutcome === 'not-started') {
    expect(observation).toMatchObject({
      rtcCapability: 'unavailable',
      applicability: 'not-applicable',
      nativeInteropOutcome: 'not-started',
      nativeInteropEvidence: null,
    })
  }
}

function verifySuccessfulInterop(
  result: BrowserHarness.PionInteropResult,
  observation: PionSucceededObservation,
): void {
  expect(observation.applicability).toBe('applicable')
  expect(observation.nativeInteropEvidence.browser.attemptId).toBe(
    observation.nativeInteropEvidence.pion.attemptId,
  )
  expect(observation.nativeInteropEvidence.browser.selectedPair).not.toBeNull()
  expect(observation.nativeInteropEvidence.pion.selectedPair).not.toBeNull()
  expect(result.browser).toMatchObject({
    label: 'windshare-frame-channel',
    protocol: 'windshare-v2',
    ordered: true,
    reliable: true,
    negotiated: false,
    highWaterObserved: true,
    lowWaterObserved: true,
    cancellationWaitObserved: true,
    cancellationError: 'AbortError',
    canceledMarkerReceived: false,
    exactServerProbe: true,
    serverFinished: true,
    terminalLast: true,
    channelState: 'closed',
    channelReason: 'none',
  })
  expect(result.browser.maximumMessageSize).toBeGreaterThanOrEqual(MAX_FRAME_BYTES)
  expect(result.browser.clientBurstMessages).toBeGreaterThan(0)
  expect(result.browser.serverBurstMessages).toBeGreaterThan(0)

  expect(result.server).toMatchObject({
    errors: [],
    channelLabel: 'windshare-frame-channel',
    channelProtocol: 'windshare-v2',
    ordered: true,
    reliable: true,
    negotiated: false,
    clientProbeReceived: true,
    serverProbeSent: true,
    terminalAcknowledged: true,
    channelDone: true,
    channelStateClosed: true,
    channelError: 'no error',
    physicalCloseSettled: true,
  })
  expect(result.server['sctpMaxMessageSize']).toBeGreaterThanOrEqual(MAX_FRAME_BYTES)
  expect(result.server['serverBufferPeak']).toBeGreaterThanOrEqual(HIGH_WATER_BYTES)
  expect(result.server['clientBurstMessages']).toBe(result.browser.clientBurstMessages)
  expect(result.server['serverBurstMessages']).toBe(result.browser.serverBurstMessages)
}

function optionalChildReporter(): ChildEvidenceReporter | null {
  const encoded = process.env[CHILD_EVIDENCE_CONTEXT_ENV]
  return encoded === undefined || encoded === '' ? null : new ChildEvidenceReporter()
}

function validateReporterIdentity(
  reporter: ChildEvidenceReporter | null,
  browserName: string,
  testInfo: TestInfo,
): void {
  if (reporter === null) return
  // The parent owns the evidence envelope, while Playwright owns the process's
  // actual project. Binding them here prevents a healthy run from being filed
  // under a different suite or browser identity.
  if (reporter.context.suite !== 'pion') {
    throw new Error('native interop evidence context must identify the Pion suite')
  }
  if (reporter.context.browser !== browserName) {
    throw new Error('native interop evidence browser differs from the running Playwright browser')
  }
  if (testInfo.project.retries !== 0 || testInfo.retry !== 0) {
    throw new Error('native interop evidence samples prohibit Playwright retries')
  }
  if (testInfo.project.repeatEach !== 1 || testInfo.repeatEachIndex !== 0) {
    throw new Error('native interop evidence samples prohibit Playwright repeat-each')
  }
}

function validateReporterTopology(
  reporter: ChildEvidenceReporter | null,
  topology: BrowserHarness.PionTopologySummary,
): void {
  if (
    reporter !== null &&
    (reporter.context.topologyProfileSha256 !== topology.topologyProfileSha256 ||
      reporter.context.topologyResolutionSha256 !== topology.topologyResolutionSha256)
  ) {
    throw new Error('native interop topology digests differ from the parent evidence context')
  }
}

async function retainNativeEvidence(
  testInfo: TestInfo,
  reporter: ChildEvidenceReporter | null,
  evidence: unknown,
): Promise<void> {
  const encoded = `${JSON.stringify(evidence)}\n`
  await testInfo.attach('native-interop-evidence', {
    body: encoded,
    contentType: 'application/json',
  })
  if (reporter === null) return
  const artifactPath = resolve(reporter.context.artifactRoot, ...NATIVE_EVIDENCE_RELATIVE_PATH.split('/'))
  await mkdir(dirname(artifactPath), { recursive: true })
  await writeFile(artifactPath, encoded, 'utf8')
  reporter.recordArtifact({
    kind: 'native-interop-evidence',
    relativePath: NATIVE_EVIDENCE_RELATIVE_PATH,
    mediaType: 'application/json',
  })
}
