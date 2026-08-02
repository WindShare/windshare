import { writeAtomicJson } from '../contract/atomic-json.ts'
import type { ArtifactIndexResult } from '../artifact/index.ts'
import type { ChildEvidenceCollection } from '../child-evidence.ts'
import { classifyRtcCapability, provisionalCapabilityEvidence } from '../capability.ts'
import {
  classifyExecutionOutcome,
  type ExecutionEvidence,
  type RunnerProcessEvidence,
} from '../execution-evidence.ts'
import {
  parseBrowserSampleResult,
  validateMainAcceptance,
  validatePionAcceptance,
  type BrowserSampleResult,
  type PlaywrightOutcome,
} from '../result.ts'
import type { VerifiedTestIceTopologyLock } from '../test-ice-topology.ts'
import type { ResultStatus } from '../vocabulary.ts'
import type { BrowserSampleIdentity } from './contract.ts'
import type { BrowserSampleContainmentTerminationReason } from '../process/containment.ts'
import { boundedMessage, normalizedViolations } from './diagnostic-text.ts'

export interface CaptureSummary {
  readonly observedBytes: number
  readonly capturedBytes: number
  readonly truncated: boolean
}

export interface ChildProcessRun {
  readonly processEvidence: RunnerProcessEvidence
  readonly terminationReason: BrowserSampleContainmentTerminationReason
  readonly stdout: CaptureSummary
  readonly stderr: CaptureSummary
  readonly treeEmpty?: boolean
  readonly cleanupOutcome?: 'completed' | 'failed'
  readonly inputEvidence?: unknown
  readonly ownershipEvidence?: unknown
}

interface FinalResultFacts {
  readonly identity: BrowserSampleIdentity
  readonly topologyLock: VerifiedTestIceTopologyLock
  readonly collection: ChildEvidenceCollection
  readonly child: ChildProcessRun
  readonly executionEvidence: ExecutionEvidence
  readonly playwrightOutcome: PlaywrightOutcome
  readonly artifacts: ArtifactIndexResult
  readonly violations: string[]
}

export function provisionalResult(
  identity: BrowserSampleIdentity,
  topologyLock: VerifiedTestIceTopologyLock,
): unknown {
  const common = {
    schemaVersion: 1,
    resultStatus: 'provisional',
    runId: identity.runId,
    runPolicy: identity.runPolicy,
    browser: identity.browser,
    sampleIndex: identity.sampleIndex,
    checkoutSha: identity.checkoutSha,
    topologyId: topologyLock.profile.topologyId,
    topologyProfileSha256: topologyLock.profileSha256,
    topologyResolutionSha256: topologyLock.resolutionSha256,
    rtcCapability: 'unknown',
    capabilityEvidence: provisionalCapabilityEvidence(),
    executionOutcome: 'unknown',
    executionEvidence: provisionalExecutionEvidence(),
    playwrightOutcome: 'not-started',
    artifacts: [],
    integrityViolations: [],
  }
  return identity.suite === 'main'
    ? {
        ...common,
        suite: 'main',
        peerAttemptOutcome: 'not-started',
        deliveryOutcome: 'not-started',
        attempts: [],
        deliveryEvidence: null,
        routeEvidence: null,
      }
    : {
        ...common,
        suite: 'pion',
        applicability: 'unknown',
        nativeInteropOutcome: 'not-started',
        nativeInteropEvidence: null,
      }
}

function provisionalExecutionEvidence(): ExecutionEvidence {
  return Object.freeze({
    pageCrashed: false,
    targetCrashed: false,
    unexpectedBrowserDisconnect: false,
    infrastructureFailure: false,
    lifecycleCompleted: false,
    runnerProcess: Object.freeze({ terminal: 'not-started' }),
  })
}

export function deriveExecutionEvidence(
  collection: ChildEvidenceCollection,
  child: ChildProcessRun,
): ExecutionEvidence {
  const browserCrash = collection.pageCrashed || collection.targetCrashed ||
    collection.unexpectedBrowserDisconnect
  const infrastructureFailure = collection.infrastructureFailure || child.terminationReason === 'deadline' ||
    child.processEvidence.terminal === 'spawn-failed' ||
    (child.processEvidence.terminal === 'signaled' && !browserCrash)
  return Object.freeze({
    pageCrashed: collection.pageCrashed,
    targetCrashed: collection.targetCrashed,
    unexpectedBrowserDisconnect: collection.unexpectedBrowserDisconnect,
    infrastructureFailure,
    lifecycleCompleted: collection.lifecycleCompleted,
    runnerProcess: child.processEvidence,
  })
}

export function derivePlaywrightOutcome(processEvidence: RunnerProcessEvidence): PlaywrightOutcome {
  if (processEvidence.terminal === 'spawn-failed' || processEvidence.terminal === 'not-started') {
    return 'not-started'
  }
  return processEvidence.terminal === 'exited' && processEvidence.exitCode === 0 ? 'passed' : 'failed'
}

export function collectRunnerViolations(
  collection: ChildEvidenceCollection,
  child: ChildProcessRun,
  executionEvidence: ExecutionEvidence,
): string[] {
  const violations = [...collection.integrityViolations]
  if (child.stdout.truncated) violations.push('runner stdout exceeded its capture limit and was truncated')
  if (child.stderr.truncated) violations.push('runner stderr exceeded its capture limit and was truncated')
  if (child.terminationReason === 'deadline') {
    violations.push('browser sample child exceeded the runner process deadline')
  }
  if (child.processEvidence.terminal === 'spawn-failed') violations.push('browser sample child failed to spawn')
  if (collection.lifecycleCompleted && collection.capabilityEvidence === null) {
    violations.push('completed browser sample omitted capability evidence')
  }
  if (classifyExecutionOutcome(executionEvidence) === 'unknown') {
    violations.push('runner-derived execution outcome remained unknown after child termination')
  }
  return [...new Set(violations)]
}

export async function writeRunnerDiagnostic(
  path: string,
  operationId: string,
  child: ChildProcessRun,
  collection: ChildEvidenceCollection,
  violations: readonly string[],
): Promise<void> {
  await writeAtomicJson(path, {
    runnerDiagnosticSchemaVersion: 1,
    operationId,
    processEvidence: child.processEvidence,
    processDeadlineExceeded: child.terminationReason === 'deadline',
    ...(child.treeEmpty === undefined
      ? {}
      : {
          ownershipSettlement: {
            terminationReason: child.terminationReason,
            treeEmpty: child.treeEmpty,
            cleanupOutcome: child.cleanupOutcome,
            inputEvidence: child.inputEvidence,
            ownershipEvidence: child.ownershipEvidence,
          },
        }),
    stdout: child.stdout,
    stderr: child.stderr,
    childEvidence: {
      lifecycleCompleted: collection.lifecycleCompleted,
      diagnosticEventCount: collection.diagnosticEventCount,
      attemptCount: collection.attempts.length,
    },
    integrityViolations: normalizedViolations(violations),
  })
}

export function deriveFinalResult(facts: FinalResultFacts): BrowserSampleResult {
  const executionOutcome = classifyExecutionOutcome(facts.executionEvidence)
  const status: ResultStatus = facts.violations.length === 0 ? 'final-valid' : 'final-invalid'
  const candidate = buildResult(facts, status, executionOutcome, facts.violations)
  try {
    return parseBrowserSampleResult(candidate, facts.topologyLock)
  } catch (cause) {
    facts.violations.push(`terminal sample evidence is internally inconsistent: ${boundedMessage(cause)}`)
  }
  const invalidCandidates = conservativeInvalidResults(facts, executionOutcome)
  let lastError: unknown
  for (const invalid of invalidCandidates) {
    try {
      return parseBrowserSampleResult(invalid, facts.topologyLock)
    } catch (cause) {
      lastError = cause
    }
  }
  throw new Error(`runner could not construct a fail-closed final result: ${boundedMessage(lastError)}`)
}

function buildResult(
  facts: FinalResultFacts,
  resultStatus: ResultStatus,
  executionOutcome: ReturnType<typeof classifyExecutionOutcome>,
  violations: readonly string[],
): unknown {
  const capabilityEvidence = facts.collection.capabilityEvidence ?? provisionalCapabilityEvidence()
  const common = {
    schemaVersion: 1,
    resultStatus,
    runId: facts.identity.runId,
    runPolicy: facts.identity.runPolicy,
    browser: facts.identity.browser,
    sampleIndex: facts.identity.sampleIndex,
    checkoutSha: facts.identity.checkoutSha,
    topologyId: facts.topologyLock.profile.topologyId,
    topologyProfileSha256: facts.topologyLock.profileSha256,
    topologyResolutionSha256: facts.topologyLock.resolutionSha256,
    rtcCapability: classifyRtcCapability(capabilityEvidence),
    capabilityEvidence,
    executionOutcome,
    executionEvidence: facts.executionEvidence,
    playwrightOutcome: facts.playwrightOutcome,
    artifacts: facts.artifacts.artifacts,
    integrityViolations: resultStatus === 'final-invalid' ? normalizedViolations(violations) : [],
  }
  if (facts.identity.suite === 'main') {
    const authoritativeAttempts = facts.collection.attempts
    return {
      ...common,
      suite: 'main',
      peerAttemptOutcome: reduceAttemptOutcome(authoritativeAttempts),
      deliveryOutcome: facts.collection.delivery?.outcome ?? 'not-started',
      attempts: authoritativeAttempts,
      deliveryEvidence: facts.collection.delivery?.evidence ?? null,
      routeEvidence: facts.collection.route,
    }
  }
  return {
    ...common,
    suite: 'pion',
    applicability: applicabilityForCapability(capabilityEvidence.apiPresence),
    nativeInteropOutcome: facts.collection.nativeInterop?.outcome ?? 'not-started',
    nativeInteropEvidence: facts.collection.nativeInterop?.evidence ?? null,
  }
}

function conservativeInvalidResults(
  facts: FinalResultFacts,
  executionOutcome: ReturnType<typeof classifyExecutionOutcome>,
): readonly unknown[] {
  const violations = normalizedViolations(facts.violations)
  const preserved = buildResult(facts, 'final-invalid', executionOutcome, violations)
  if (facts.identity.suite === 'main') {
    const main = preserved as Record<string, unknown>
    return [
      main,
      { ...main, routeEvidence: null },
      {
        ...main,
        routeEvidence: null,
        deliveryOutcome: 'not-started',
        deliveryEvidence: null,
      },
    ]
  }
  const pion = preserved as Record<string, unknown>
  return [
    pion,
    { ...pion, nativeInteropOutcome: 'not-started', nativeInteropEvidence: null },
  ]
}

function reduceAttemptOutcome(attempts: readonly { readonly outcome: 'admitted' | 'failed' }[]): 'not-started' | 'admitted' | 'failed' {
  if (attempts.length === 0) return 'not-started'
  return attempts.some((attempt) => attempt.outcome === 'failed') ? 'failed' : 'admitted'
}

function applicabilityForCapability(presence: string): 'unknown' | 'applicable' | 'not-applicable' {
  if (presence === 'unknown') return 'unknown'
  return presence === 'absent' ? 'not-applicable' : 'applicable'
}

export function acceptedBySuite(
  result: BrowserSampleResult,
  topologyLock: VerifiedTestIceTopologyLock,
): boolean {
  try {
    if (result.suite === 'main') validateMainAcceptance(result, topologyLock)
    else validatePionAcceptance(result)
    return true
  } catch {
    return false
  }
}
