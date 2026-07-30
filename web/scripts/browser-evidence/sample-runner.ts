import { createWriteStream } from 'node:fs'
import { mkdir, writeFile } from 'node:fs/promises'
import { basename, dirname, join, resolve } from 'node:path'
import { finished } from 'node:stream/promises'

import { BrowserSampleResultWriter, writeAtomicJson } from './contract/atomic-json.ts'
import { indexArtifacts, type ArtifactIndexResult } from './artifact/index.ts'
import {
  collectChildEvidence,
  type ChildEvidenceCollection,
  type ChildEvidenceContext,
} from './child-evidence.ts'
import { provisionalCapabilityEvidence, classifyRtcCapability } from './capability.ts'
import {
  classifyExecutionOutcome,
  type ExecutionEvidence,
  type RunnerProcessEvidence,
} from './execution-evidence.ts'
import {
  BrowserSampleStaging,
  type BrowserSampleStagingHooks,
} from './process/attachment-staging.ts'
import type {
  BrowserSampleContainmentBackend,
  BrowserSampleContainmentPreflight,
  BrowserSampleContainmentTrace,
  ContainedSampleCommand,
} from './process/containment.ts'
import {
  createProductionContainmentBackend,
} from './process/containment-factory.ts'
import { createInheritedProcessContainmentBackend } from './process/inherited-process-backend.ts'
import type { LinuxProcessOwnerArtifact } from './process/linux-process-owner-client.ts'
import {
  parseBrowserSampleResult,
  validateMainAcceptance,
  validatePionAcceptance,
  type BrowserSampleResult,
  type PlaywrightOutcome,
} from './result.ts'
import {
  parseBrowserRunPolicy,
  validatePolicySampleIndex,
  type BrowserRunPolicy,
} from './run-policy.ts'
import {
  readVerifiedTestIceTopologyLock,
  type VerifiedTestIceTopologyLock,
} from './test-ice-topology.ts'
import type { BrowserEngine, BrowserSuite, ResultStatus } from './vocabulary.ts'

export const RUNNER_MAXIMUM_CAPTURED_STREAM_BYTES = 16_777_216 as const
export const RUNNER_SAMPLE_PROCESS_DEADLINE_MS = 1_200_000 as const
export const RUNNER_PROCESS_TERMINATION_GRACE_MS = 5_000 as const
const MAXIMUM_LIFECYCLE_TRACE_FAILURES = 8
const MAXIMUM_LIFECYCLE_TRACE_CAUSES = 8
export interface BrowserSampleIdentity {
  readonly runId: string
  readonly runPolicy: BrowserRunPolicy
  readonly suite: BrowserSuite
  readonly browser: BrowserEngine
  readonly sampleIndex: number
  readonly checkoutSha: string
}

export type BrowserSampleCommand = ContainedSampleCommand

export interface BrowserSampleRunnerOptions extends BrowserSampleIdentity {
  readonly sampleDirectory: string
  readonly topologyLock: VerifiedTestIceTopologyLock
  readonly topologyProfilePath: string
  readonly topologyResolutionPath: string
  readonly command: BrowserSampleCommand
  readonly maximumCapturedStreamBytes?: number
  readonly processDeadlineMs?: number
  readonly windowsJobHelperPath?: string
  readonly linuxProcessOwner?: LinuxProcessOwnerArtifact
  readonly readOnlyInputRoots?: readonly string[]
  readonly containmentBackend?: BrowserSampleContainmentBackend
  /**
   * `inherited` is valid only when this runner is the sole child of an external
   * subreaper/Job. It also defers terminal result persistence to that owner.
   */
  readonly ownershipMode?: 'owned' | 'inherited'
  readonly stagingHooks?: BrowserSampleStagingHooks
  readonly trace?: BrowserSampleTraceSink
}

export interface BrowserSampleTrace {
  readonly operationId: string
  readonly milestone: string
  readonly suite: BrowserSuite
  readonly browser: BrowserEngine
  readonly sampleIndex: number
  readonly context?: Readonly<Record<string, unknown>>
}

export type BrowserSampleTraceSink = (trace: BrowserSampleTrace) => void

export interface BrowserSampleRunOutcome {
  readonly result: BrowserSampleResult
  readonly resultPath: string
  readonly sampleDirectory: string
  readonly artifactRoot: string
  readonly acceptedBeforeGuard: boolean
}

interface ChildProcessRun {
  readonly processEvidence: RunnerProcessEvidence
  readonly timedOut: boolean
  readonly stdout: CaptureSummary
  readonly stderr: CaptureSummary
}

interface CaptureSummary {
  readonly observedBytes: number
  readonly capturedBytes: number
  readonly truncated: boolean
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

export async function runBrowserSample(
  options: BrowserSampleRunnerOptions,
): Promise<BrowserSampleRunOutcome> {
  options = normalizedRunnerOptions(options)
  const topologyLock = readVerifiedTestIceTopologyLock(options.topologyLock)
  const trace = options.trace ?? defaultTraceSink
  const operationId = sampleOperationId(options)
  const containment = selectedContainmentBackend(options)
  const preflight = containmentPreflight(options, operationId)
  emitTrace(trace, options, operationId, 'containment-preflight-started', {
    backend: containment.kind,
  })
  await containment.preflight(preflight)
  emitTrace(trace, options, operationId, 'containment-preflight-completed', {
    backend: containment.kind,
  })

  const sampleDirectory = resolve(options.sampleDirectory)
  await mkdir(dirname(sampleDirectory), { recursive: true })
  await mkdir(sampleDirectory)
  const resultPath = join(sampleDirectory, 'result.json')
  const resultWriter = new BrowserSampleResultWriter(resultPath, topologyLock)
  const provisional = await resultWriter.writeProvisional(provisionalResult(options, topologyLock))
  emitTrace(trace, options, operationId, 'provisional-result-written')

  let staging: BrowserSampleStaging | undefined
  let finalizationStarted = false
  let finalizationCompleted = false
  let finalizationPhase = 'not-started'
  try {
    staging = await BrowserSampleStaging.create(sampleDirectory, options.stagingHooks)
    const executionContext = childContext(
      options,
      topologyLock,
      staging.childAttachmentRoot,
      staging.childPath('child'),
    )
    await mkdir(dirname(executionContext.evidencePath), { recursive: true })
    await writeFile(
      executionContext.evidencePath,
      '',
      { encoding: 'utf8', flag: 'wx', mode: 0o600 },
    )

    emitTrace(trace, options, operationId, 'child-process-starting', {
      executable: basename(options.command.executable),
      containmentBackend: containment.kind,
    })
    const child = await executeChild(
      options,
      executionContext,
      staging,
      containment,
      preflight,
      trace,
    )
    emitTrace(trace, options, operationId, 'child-process-terminal', {
      terminal: child.processEvidence.terminal,
      timedOut: child.timedOut,
    })
    finalizationStarted = true
    finalizationPhase = 'assemble-parent-collection'
    emitTrace(trace, options, operationId, 'attachment-finalization-started', {
      backend: containment.kind,
      phase: finalizationPhase,
      settledFailureCount: 0,
    })
    const finalizedCollection = await staging.finalize()
    const artifactRoot = finalizedCollection.absoluteRoot
    finalizationCompleted = true
    const context = childContext(
      options,
      topologyLock,
      artifactRoot,
      join(artifactRoot, 'child'),
    )

    const collection = await collectChildEvidence(context.evidencePath, context)
    const executionEvidence = deriveExecutionEvidence(collection, child)
    const playwrightOutcome = derivePlaywrightOutcome(child.processEvidence)
    const preIndexViolations = collectRunnerViolations(collection, child, executionEvidence)
    const runnerDirectory = join(artifactRoot, 'runner')
    await writeRunnerDiagnostic(
      join(runnerDirectory, 'diagnostic.json'),
      operationId,
      child,
      collection,
      preIndexViolations,
    )
    const artifacts = await indexArtifacts(artifactRoot, options.suite, collection.artifactRegistrations)
    emitTrace(trace, options, operationId, 'attachment-finalization-completed', {
      backend: containment.kind,
      phase: finalizationPhase,
      artifactCount: artifacts.artifacts.length,
      settledFailureCount: 0,
    })
    const violations = [...preIndexViolations, ...artifacts.integrityViolations]
    const finalResult = deriveFinalResult({
      identity: options,
      topologyLock,
      collection,
      child,
      executionEvidence,
      playwrightOutcome,
      artifacts,
      violations,
    })
    const ownershipTransfer = await staging.commit()
    if (ownershipTransfer.failures.length > 0) {
      emitTrace(trace, options, operationId, 'attachment-ownership-transfer-failed',
        lifecycleFailureContext(containment.kind, ownershipTransfer.phase, ownershipTransfer.failures))
    }
    const result = options.ownershipMode === 'inherited'
      ? finalResult
      : await resultWriter.writeFinal(finalResult)
    emitTrace(trace, options, operationId,
      options.ownershipMode === 'inherited'
        ? 'parent-finalization-candidate-ready'
        : 'final-result-written', {
      resultStatus: result.resultStatus,
      executionOutcome: result.executionOutcome,
      playwrightOutcome: result.playwrightOutcome,
      integrityViolationCount: result.integrityViolations.length,
    })
    return Object.freeze({
      result,
      resultPath,
      sampleDirectory,
      artifactRoot,
      acceptedBeforeGuard: acceptedBySuite(result, topologyLock),
    })
  } catch (cause) {
    if (finalizationStarted && !finalizationCompleted) {
      emitTrace(
        trace,
        options,
        operationId,
        'attachment-finalization-failed',
        lifecycleFailureContext(containment.kind, finalizationPhase, cause),
      )
    }
    return restoreProvisionalAndRollback(
      resultPath,
      provisional,
      cause,
      staging,
      containment.kind,
      trace,
      options,
      operationId,
    )
  }
}

function normalizedRunnerOptions(options: BrowserSampleRunnerOptions): BrowserSampleRunnerOptions {
  const runPolicy = parseBrowserRunPolicy(options.runPolicy, 'browser sample run policy')
  validatePolicySampleIndex(options.sampleIndex, runPolicy, 'browser sample index')
  return Object.freeze({ ...options, runPolicy })
}

function selectedContainmentBackend(
  options: BrowserSampleRunnerOptions,
): BrowserSampleContainmentBackend {
  const ownershipMode = options.ownershipMode ?? 'owned'
  if (ownershipMode === 'inherited') {
    if (
      options.containmentBackend !== undefined || options.windowsJobHelperPath !== undefined ||
      options.linuxProcessOwner !== undefined
    ) {
      throw new Error(
        'inherited sample ownership cannot be combined with a nested containment backend',
      )
    }
    return createInheritedProcessContainmentBackend()
  }
  if (options.containmentBackend !== undefined) {
    if (options.windowsJobHelperPath !== undefined || options.linuxProcessOwner !== undefined) {
      throw new Error(
        'an injected containment backend cannot be combined with production containment authorities',
      )
    }
    return options.containmentBackend
  }
  return createProductionContainmentBackend({
    ...(options.linuxProcessOwner === undefined
      ? {}
      : { linuxProcessOwner: options.linuxProcessOwner }),
    ...(options.windowsJobHelperPath === undefined
      ? {}
      : { windowsJobHelperPath: options.windowsJobHelperPath }),
  })
}

function containmentPreflight(
  options: BrowserSampleRunnerOptions,
  operationId: string,
): BrowserSampleContainmentPreflight {
  return Object.freeze({
    operationId,
    topologyProfilePath: resolve(options.topologyProfilePath),
    topologyProfileSha256: options.topologyLock.profileSha256,
    topologyResolutionPath: resolve(options.topologyResolutionPath),
    topologyResolutionSha256: options.topologyLock.resolutionSha256,
    readOnlyInputRoots: Object.freeze(
      (options.readOnlyInputRoots ?? []).map((root) => resolve(root)),
    ),
  })
}

function emitContainmentTrace(
  trace: BrowserSampleTraceSink,
  identity: BrowserSampleIdentity,
  operationId: string,
  event: BrowserSampleContainmentTrace,
): void {
  emitTrace(
    trace,
    identity,
    operationId,
    `containment-${event.milestone}`,
    event.context,
  )
}

async function restoreProvisionalAndRollback(
  resultPath: string,
  provisional: BrowserSampleResult,
  cause: unknown,
  staging: BrowserSampleStaging | undefined,
  backend: BrowserSampleContainmentBackend['kind'],
  trace: BrowserSampleTraceSink,
  identity: BrowserSampleIdentity,
  operationId: string,
): Promise<never> {
  // Containment resolves only after the child can no longer mutate staging.
  // Reasserting the parsed snapshot then erases any pre-retirement spoof before
  // the failure becomes observable; this path can never authorize a final write.
  try {
    await writeAtomicJson(resultPath, provisional)
  } catch (restorationCause) {
    throw new AggregateError(
      [cause, restorationCause],
      'failed to restore provisional browser sample authority',
      { cause: restorationCause },
    )
  }
  if (staging !== undefined) {
    emitTrace(trace, identity, operationId, 'attachment-rollback-started', {
      backend,
      phase: 'remove-owned-staging',
      settledFailureCount: 0,
    })
    try {
      await staging.dispose()
      emitTrace(trace, identity, operationId, 'attachment-rollback-completed', {
        backend,
        phase: 'remove-owned-staging',
        settledFailureCount: 0,
      })
    } catch (cleanupCause) {
      emitTrace(
        trace,
        identity,
        operationId,
        'attachment-rollback-failed',
        lifecycleFailureContext(backend, 'remove-owned-staging', cleanupCause),
      )
      throw new AggregateError(
        [cause, cleanupCause],
        'browser sample run and staging rollback both failed',
        { cause: cleanupCause },
      )
    }
  }
  throw cause
}

function provisionalResult(
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

function childContext(
  options: BrowserSampleRunnerOptions,
  topologyLock: VerifiedTestIceTopologyLock,
  artifactRoot: string,
  childDirectory: string,
): ChildEvidenceContext {
  return Object.freeze({
    runId: options.runId,
    runPolicy: options.runPolicy,
    suite: options.suite,
    browser: options.browser,
    sampleIndex: options.sampleIndex,
    checkoutSha: options.checkoutSha,
    topologyProfileSha256: topologyLock.profileSha256,
    topologyResolutionSha256: topologyLock.resolutionSha256,
    topologyProfilePath: resolve(options.topologyProfilePath),
    topologyResolutionPath: resolve(options.topologyResolutionPath),
    evidencePath: join(childDirectory, 'evidence.jsonl'),
    artifactRoot,
  })
}

async function executeChild(
  options: BrowserSampleRunnerOptions,
  context: ChildEvidenceContext,
  staging: BrowserSampleStaging,
  containment: BrowserSampleContainmentBackend,
  preflight: BrowserSampleContainmentPreflight,
  trace: BrowserSampleTraceSink,
): Promise<ChildProcessRun> {
  const maximumBytes = options.maximumCapturedStreamBytes ?? RUNNER_MAXIMUM_CAPTURED_STREAM_BYTES
  const stdout = new BoundedFileCapture(staging.runnerPath('stdout.log'), maximumBytes)
  const stderr = new BoundedFileCapture(staging.runnerPath('stderr.log'), maximumBytes)
  try {
    const authority = await containment.execute({
      ...preflight,
      command: options.command,
      sampleDirectory: staging.sampleDirectory,
      childAttachmentStagingRoot: staging.childAttachmentRoot,
      childContext: context,
      deadlineMs: options.processDeadlineMs ?? RUNNER_SAMPLE_PROCESS_DEADLINE_MS,
      terminationGraceMs: RUNNER_PROCESS_TERMINATION_GRACE_MS,
      stdout: (chunk) => stdout.consume(chunk),
      stderr: (chunk) => stderr.consume(chunk),
      trace: (event) => emitContainmentTrace(trace, options, preflight.operationId, event),
    })
    return Object.freeze({
      ...authority,
      stdout: stdout.summary(),
      stderr: stderr.summary(),
    })
  } finally {
    await Promise.all([stdout.close(), stderr.close()])
  }
}

class BoundedFileCapture {
  readonly #stream
  readonly #maximumBytes: number
  #observedBytes = 0
  #capturedBytes = 0
  #closed = false

  constructor(path: string, maximumBytes: number) {
    if (!Number.isSafeInteger(maximumBytes) || maximumBytes < 1) {
      throw new Error('captured stream byte limit must be a positive safe integer')
    }
    this.#maximumBytes = maximumBytes
    this.#stream = createWriteStream(path, { flags: 'wx', mode: 0o600 })
  }

  consume(chunk: Uint8Array): void {
    if (this.#closed) return
    this.#observedBytes += chunk.byteLength
    const remaining = this.#maximumBytes - this.#capturedBytes
    if (remaining <= 0) return
    const accepted = chunk.subarray(0, Math.min(remaining, chunk.byteLength))
    this.#capturedBytes += accepted.byteLength
    this.#stream.write(accepted)
  }

  async close(): Promise<void> {
    if (this.#closed) return
    this.#closed = true
    this.#stream.end()
    await finished(this.#stream)
  }

  summary(): CaptureSummary {
    return Object.freeze({
      observedBytes: this.#observedBytes,
      capturedBytes: this.#capturedBytes,
      truncated: this.#observedBytes > this.#capturedBytes,
    })
  }
}

function deriveExecutionEvidence(
  collection: ChildEvidenceCollection,
  child: ChildProcessRun,
): ExecutionEvidence {
  const browserCrash = collection.pageCrashed || collection.targetCrashed ||
    collection.unexpectedBrowserDisconnect
  const infrastructureFailure = collection.infrastructureFailure || child.timedOut ||
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

function derivePlaywrightOutcome(processEvidence: RunnerProcessEvidence): PlaywrightOutcome {
  if (processEvidence.terminal === 'spawn-failed' || processEvidence.terminal === 'not-started') {
    return 'not-started'
  }
  return processEvidence.terminal === 'exited' && processEvidence.exitCode === 0 ? 'passed' : 'failed'
}

function collectRunnerViolations(
  collection: ChildEvidenceCollection,
  child: ChildProcessRun,
  executionEvidence: ExecutionEvidence,
): string[] {
  const violations = [...collection.integrityViolations]
  if (child.stdout.truncated) violations.push('runner stdout exceeded its capture limit and was truncated')
  if (child.stderr.truncated) violations.push('runner stderr exceeded its capture limit and was truncated')
  if (child.timedOut) violations.push('browser sample child exceeded the runner process deadline')
  if (child.processEvidence.terminal === 'spawn-failed') violations.push('browser sample child failed to spawn')
  if (collection.lifecycleCompleted && collection.capabilityEvidence === null) {
    violations.push('completed browser sample omitted capability evidence')
  }
  if (classifyExecutionOutcome(executionEvidence) === 'unknown') {
    violations.push('runner-derived execution outcome remained unknown after child termination')
  }
  return [...new Set(violations)]
}

async function writeRunnerDiagnostic(
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
    processDeadlineExceeded: child.timedOut,
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

function deriveFinalResult(facts: FinalResultFacts): BrowserSampleResult {
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

function acceptedBySuite(
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

function emitTrace(
  sink: BrowserSampleTraceSink,
  identity: BrowserSampleIdentity,
  operationId: string,
  milestone: string,
  context?: Readonly<Record<string, unknown>>,
): void {
  const trace = Object.freeze({
    operationId,
    milestone,
    suite: identity.suite,
    browser: identity.browser,
    sampleIndex: identity.sampleIndex,
    ...(context === undefined ? {} : { context }),
  })
  try {
    sink(trace)
  } catch (cause) {
    // Observability is not evidence authority. A broken trace integration must
    // never strand a sample at its provisional result after the child exits.
    try {
      defaultTraceSink(Object.freeze({
        ...trace,
        milestone: 'trace-sink-failed',
        context: Object.freeze({ failedMilestone: milestone, error: boundedMessage(cause) }),
      }))
    } catch {
      // stderr itself can be unavailable during process teardown; result
      // persistence remains the higher-priority authority.
    }
  }
}

function defaultTraceSink(trace: BrowserSampleTrace): void {
  process.stderr.write(`${JSON.stringify({ component: 'browser-evidence-runner', ...trace })}\n`)
}

function sampleOperationId(identity: BrowserSampleIdentity): string {
  return `${identity.runId}/${identity.suite}/${identity.browser}/${identity.sampleIndex}`
}

function normalizedViolations(violations: readonly string[]): readonly string[] {
  return Object.freeze([...new Set(violations.map((violation) => boundedText(violation, 1_024)))].sort())
}

function boundedMessage(value: unknown): string {
  const message = value instanceof Error ? value.message : String(value)
  return boundedText(message || 'unknown runner error', 512)
}

function lifecycleFailureContext(
  backend: BrowserSampleContainmentBackend['kind'],
  phase: string,
  cause: unknown,
): Readonly<Record<string, unknown>> {
  const settled = settledFailureMessages(cause)
  return Object.freeze({
    backend,
    phase: boundedText(phase, 128),
    settledFailureCount: settled.total,
    settledFailures: Object.freeze(settled.messages),
    settledFailuresTruncated: settled.total > settled.messages.length,
    causeChain: Object.freeze(causeChain(cause)),
  })
}

function settledFailureMessages(cause: unknown): {
  readonly total: number
  readonly messages: readonly string[]
} {
  let failures: readonly unknown[]
  if (Array.isArray(cause)) failures = cause
  else if (cause instanceof AggregateError) failures = [...cause.errors]
  else failures = [cause]
  return Object.freeze({
    total: failures.length,
    messages: Object.freeze(
      failures.slice(0, MAXIMUM_LIFECYCLE_TRACE_FAILURES).map(boundedMessage),
    ),
  })
}

function causeChain(cause: unknown): readonly string[] {
  const result: string[] = []
  const visited = new Set<unknown>()
  let current: unknown = cause
  while (current !== undefined && current !== null && result.length < MAXIMUM_LIFECYCLE_TRACE_CAUSES) {
    if (visited.has(current)) {
      result.push('cyclic error cause')
      break
    }
    visited.add(current)
    result.push(boundedMessage(current))
    current = typeof current === 'object' && 'cause' in current
      ? (current as { readonly cause?: unknown }).cause
      : undefined
  }
  return result
}

function boundedText(value: string, maximumBytes: number): string {
  const normalized = value.normalize('NFC')
  let result = ''
  let bytes = 0
  for (const character of normalized) {
    const width = Buffer.byteLength(character, 'utf8')
    if (bytes + width > maximumBytes) break
    result += character
    bytes += width
  }
  return result || 'unknown runner error'
}
