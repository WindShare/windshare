import { copyFile, mkdir, writeFile } from 'node:fs/promises'
import { dirname, join } from 'node:path'

import {
  ChildEvidenceReporter,
  type ChildEvidenceContext,
} from '../../scripts/browser-evidence/child-evidence.ts'
import type { AttemptEvidence } from '../../scripts/browser-evidence/attempt-evidence.ts'
import type { CapabilityEvidence } from '../../scripts/browser-evidence/capability.ts'
import type { MainRouteEvidence } from '../../scripts/browser-evidence/route-evidence.ts'
import {
  ARTIFACT_KINDS,
  MAIN_TRANSFER_BYTES,
  MAIN_TRANSFER_SHA256,
  type ArtifactKind,
} from '../../scripts/browser-evidence/result.ts'
import {
  admittedEvents,
  browserPair,
  identity,
  pionPair,
  TEST_IDENTITY,
} from './fixtures.ts'

export interface SyntheticScenarioOptions {
  readonly context: ChildEvidenceContext
  readonly mode: string
  readonly delayMs?: number
  readonly environment?: Readonly<Record<string, string | undefined>>
  readonly stdout: (chunk: Uint8Array) => void
  readonly stderr: (chunk: Uint8Array) => void
}

export interface SyntheticScenarioOutcome {
  readonly exitCode: number
}

const TEXT_ENCODER = new TextEncoder()

/**
 * Contract fixtures need deterministic browser evidence, not process authority.
 * Keeping the scenario behind explicit byte sinks lets the pure containment
 * double and the process integration child exercise one semantic producer.
 */
export async function runSyntheticScenario(
  options: SyntheticScenarioOptions,
): Promise<SyntheticScenarioOutcome> {
  const delayMs = options.delayMs ?? 0
  if (!Number.isSafeInteger(delayMs) || delayMs < 0) {
    throw new Error('synthetic scenario delay must be a nonnegative safe integer')
  }
  if (delayMs > 0) await new Promise((resolveDelay) => setTimeout(resolveDelay, delayMs))

  if (options.mode === 'truncated-event') {
    await writeFile(options.context.evidencePath, '{"schemaVersion":1', { flag: 'a' })
    return Object.freeze({ exitCode: 1 })
  }

  const reporter = new ChildEvidenceReporter(options.context)
  if (
    ['main-pass', 'output-overflow', 'descendant-after-root', 'detached-after-root',
      'double-fork-after-root'].includes(options.mode)
  ) emitMainPass(reporter)
  else if (options.mode === 'main-valid-with-incomplete-attempt') {
    emitMainPassWithIncompleteAttempt(reporter)
  } else if (options.mode === 'main-unavailable' || options.mode === 'exit-assertion') {
    emitMainUnavailable(reporter)
  } else if (options.mode === 'pion-pass') emitPionPass(reporter)
  else if (options.mode === 'pion-unavailable') emitPionUnavailable(reporter)
  else if (options.mode === 'crash-before-probe') {
    reporter.recordPageCrash('synthetic public page.crash')
  } else if (options.mode === 'target-crash') reporter.recordTargetCrash('Target crashed')
  else if (options.mode === 'missing-terminal') emitMissingTerminal(reporter)
  else if (options.mode !== 'descendant-timeout' && options.mode !== 'root-exit-with-descendant') {
    throw new Error(`unknown synthetic child mode ${options.mode}`)
  }

  if (options.environment?.SYNTHETIC_ARTIFACT_SOURCE !== undefined) {
    await emitArtifact(reporter, options.environment)
  }
  if (!['crash-before-probe', 'target-crash'].includes(options.mode)) reporter.completeLifecycle()
  await reporter.flush()
  options.stdout(TEXT_ENCODER.encode(`synthetic-child:${options.mode}:stdout\n`))
  options.stderr(TEXT_ENCODER.encode(`synthetic-child:${options.mode}:stderr\n`))
  if (options.mode === 'output-overflow') options.stdout(TEXT_ENCODER.encode('x'.repeat(4_096)))
  return Object.freeze({
    exitCode: ['crash-before-probe', 'target-crash', 'exit-assertion'].includes(options.mode) ? 1 : 0,
  })
}

async function emitArtifact(
  reporter: ChildEvidenceReporter,
  environment: Readonly<Record<string, string | undefined>>,
): Promise<void> {
  const source = environment.SYNTHETIC_ARTIFACT_SOURCE
  if (source === undefined) throw new Error('synthetic artifact source is absent')
  const relativePath = environment.SYNTHETIC_ARTIFACT_PATH ?? 'playwright/diagnostic.txt'
  const destination = join(reporter.context.artifactRoot, ...relativePath.split('/'))
  await mkdir(dirname(destination), { recursive: true })
  await copyFile(source, destination)
  reporter.recordArtifact({
    kind: syntheticArtifactKind(environment.SYNTHETIC_ARTIFACT_KIND),
    relativePath,
    mediaType: environment.SYNTHETIC_ARTIFACT_MEDIA_TYPE ?? 'application/octet-stream',
  })
}

function emitMainPass(reporter: ChildEvidenceReporter): void {
  reporter.recordCapability(availableCapability())
  for (const event of admittedEvents()) reporter.recordAttempt(event)
  reporter.recordDelivery('succeeded', successfulDelivery())
  reporter.recordRoute(hotSwitchRoute())
}

function emitMainPassWithIncompleteAttempt(reporter: ChildEvidenceReporter): void {
  reporter.recordCapability(availableCapability())
  reporter.recordAttempt({ ...firstAdmittedEvent(), attemptId: identity(99) })
  for (const event of admittedEvents()) reporter.recordAttempt(event)
  reporter.recordDelivery('succeeded', successfulDelivery())
  reporter.recordRoute(hotSwitchRoute())
}

function emitMainUnavailable(reporter: ChildEvidenceReporter): void {
  reporter.recordCapability(unavailableCapability())
  reporter.recordDelivery('succeeded', successfulDelivery())
  reporter.recordRoute({
    mode: 'relay-only',
    observations: [{
      observationSequence: 1,
      kind: 'dispatch',
      dispatchSequence: 1,
      route: 'relay',
      lane: { laneId: 1, laneEpoch: 0 },
    }],
  })
}

function emitPionPass(reporter: ChildEvidenceReporter): void {
  reporter.recordCapability(availableCapability())
  const attemptId = identity(20)
  reporter.recordNativeInterop('succeeded', {
    browser: { attemptId, selectedPair: browserPair() },
    pion: { attemptId, selectedPair: pionPair() },
  })
}

function emitPionUnavailable(reporter: ChildEvidenceReporter): void {
  reporter.recordCapability(unavailableCapability())
}

function emitMissingTerminal(reporter: ChildEvidenceReporter): void {
  reporter.recordCapability(availableCapability())
  reporter.recordAttempt(firstAdmittedEvent())
}

function availableCapability(): CapabilityEvidence {
  return {
    schemaVersion: 1 as const,
    apiPresence: 'present' as const,
    probeOutcome: 'succeeded' as const,
    probeDeadlineMs: 5_000 as const,
  }
}

function unavailableCapability(): CapabilityEvidence {
  return {
    schemaVersion: 1 as const,
    apiPresence: 'absent' as const,
    probeOutcome: 'not-started' as const,
    probeDeadlineMs: 5_000 as const,
  }
}

function successfulDelivery() {
  return {
    expectedBytes: MAIN_TRANSFER_BYTES,
    receivedBytes: MAIN_TRANSFER_BYTES,
    expectedSha256: MAIN_TRANSFER_SHA256,
    receivedSha256: MAIN_TRANSFER_SHA256,
    terminal: 'succeeded' as const,
  }
}

function hotSwitchRoute(): MainRouteEvidence {
  const lane = { laneId: 7, laneEpoch: 9 }
  return {
    mode: 'hot-switch' as const,
    observations: [
      {
        observationSequence: 1,
        kind: 'dispatch' as const,
        dispatchSequence: 1,
        route: 'relay' as const,
        lane: { laneId: 1, laneEpoch: 0 },
      },
      {
        observationSequence: 2,
        kind: 'peer-admitted' as const,
        ...TEST_IDENTITY,
        lane,
      },
      {
        observationSequence: 3,
        kind: 'relay-cut-fence' as const,
        dispatchSequenceBoundary: 1,
        proxyAccepting: false as const,
        receiverRelayEligible: false as const,
      },
      {
        observationSequence: 4,
        kind: 'dispatch' as const,
        dispatchSequence: 2,
        route: 'peer' as const,
        lane,
      },
    ],
  }
}

function firstAdmittedEvent(): AttemptEvidence {
  const event = admittedEvents()[0]
  if (event === undefined) throw new Error('synthetic admitted fixture is empty')
  return event
}

function syntheticArtifactKind(value: string | undefined): ArtifactKind {
  const kind = value ?? 'process-log'
  if (!ARTIFACT_KINDS.includes(kind as ArtifactKind)) {
    throw new Error(`unknown synthetic artifact kind ${JSON.stringify(kind)}`)
  }
  return kind as ArtifactKind
}
