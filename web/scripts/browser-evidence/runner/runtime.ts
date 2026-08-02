import { createWriteStream } from 'node:fs'
import { mkdir, writeFile } from 'node:fs/promises'
import { basename, dirname, join, resolve } from 'node:path'
import { finished } from 'node:stream/promises'

import { BrowserSampleResultWriter, writeAtomicJson } from '../contract/atomic-json.ts'
import { indexArtifacts } from '../artifact/index.ts'
import {
  collectChildEvidence,
  type ChildEvidenceContext,
} from '../child-evidence.ts'
import {
  BrowserSampleStaging,
} from '../process/attachment-staging.ts'
import type {
  BrowserSampleContainmentBackend,
  BrowserSampleContainmentPreflight,
  BrowserSampleContainmentTrace,
} from '../process/containment.ts'
import { BrowserSampleContainmentError } from '../process/containment.ts'
import {
  createOwnedEventChannel,
  type OwnedByteSnapshot,
} from '../process/owned-process-channel.mjs'
import {
  createProductionContainmentBackend,
} from '../process/containment-factory.ts'
import { createInheritedProcessContainmentBackend } from '../process/inherited-process-backend.ts'
import {
  type BrowserSampleResult,
} from '../result.ts'
import {
  parseBrowserRunPolicy,
  validatePolicySampleIndex,
} from '../run-policy.ts'
import {
  readVerifiedTestIceTopologyLock,
  type VerifiedTestIceTopologyLock,
} from '../test-ice-topology.ts'
import {
  RUNNER_MAXIMUM_CAPTURED_STREAM_BYTES,
  RUNNER_PROCESS_TERMINATION_GRACE_MS,
  RUNNER_SAMPLE_PROCESS_DEADLINE_MS,
  BROWSER_SAMPLE_TRACE_SCHEMA_VERSION,
  type BrowserSampleIdentity,
  type BrowserSampleRunnerOptions,
  type BrowserSampleRunExecution,
  type BrowserSampleRunOutcome,
  type BrowserSampleTrace,
} from './contract.ts'
import { boundedMessage, boundedText } from './diagnostic-text.ts'
import {
  acceptedBySuite,
  collectRunnerViolations,
  deriveExecutionEvidence,
  deriveFinalResult,
  derivePlaywrightOutcome,
  provisionalResult,
  writeRunnerDiagnostic,
  type CaptureSummary,
  type ChildProcessRun,
} from './result-assembly.ts'

const MAXIMUM_LIFECYCLE_TRACE_FAILURES = 8
const MAXIMUM_LIFECYCLE_TRACE_CAUSES = 8
const MAXIMUM_BROWSER_SAMPLE_TRACE_EVENTS = 64

export function startBrowserSample(
  options: BrowserSampleRunnerOptions,
): BrowserSampleRunExecution {
  const journal = createOwnedEventChannel<BrowserSampleTrace>(
    MAXIMUM_BROWSER_SAMPLE_TRACE_EVENTS,
    'browser sample lifecycle trace',
  )
  const lifecycle = new BrowserSampleLifecycleTraceOwner(
    options,
    journal.append,
  )
  lifecycle.start()
  const result = runBrowserSampleOperation(options, lifecycle).then(
    (outcome) => {
      lifecycle.terminal('succeeded', {
        resultStatus: outcome.result.resultStatus,
        executionOutcome: outcome.result.executionOutcome,
        playwrightOutcome: outcome.result.playwrightOutcome,
      })
      return outcome
    },
    (cause: unknown) => {
      lifecycle.terminal('failed', { failure: boundedMessage(cause) })
      throw cause
    },
  ).finally(() => {
    journal.finish()
    const snapshot = journal.view.snapshot()
    const traceFailure = journal.failure()
    if (traceFailure !== undefined) throw traceFailure
    if (
      !snapshot.completed ||
      snapshot.truncated ||
      snapshot.observedEvents !== snapshot.capturedEvents
    ) throw new Error('browser sample lifecycle trace evidence is incomplete')
  })
  return Object.freeze({ result, traces: journal.view })
}

async function runBrowserSampleOperation(
  options: BrowserSampleRunnerOptions,
  lifecycle: BrowserSampleLifecycleTraceOwner,
): Promise<BrowserSampleRunOutcome> {
  options = normalizedRunnerOptions(options)
  const topologyLock = readVerifiedTestIceTopologyLock(options.topologyLock)
  const operationId = options.operationId
  const containment = selectedContainmentBackend(options)
  const preflight = containmentPreflight(options, operationId)
  lifecycle.emit('containment-preflight-started', 'started', {
    backend: containment.kind,
  })
  await containment.preflight(preflight)
  lifecycle.emit('containment-preflight-completed', 'succeeded', {
    backend: containment.kind,
  })

  const sampleDirectory = resolve(options.sampleDirectory)
  await mkdir(dirname(sampleDirectory), { recursive: true })
  await mkdir(sampleDirectory)
  const resultPath = join(sampleDirectory, 'result.json')
  const resultWriter = new BrowserSampleResultWriter(resultPath, topologyLock)
  const provisional = await resultWriter.writeProvisional(provisionalResult(options, topologyLock))
  lifecycle.emit('provisional-result-written', 'succeeded')

  let staging: BrowserSampleStaging | undefined
  let finalizationStarted = false
  let finalizationCompleted = false
  let finalizationPhase = 'not-started'
  try {
    staging = await BrowserSampleStaging.create(sampleDirectory, options.stagingFaultCut)
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

    lifecycle.markCleanupPending()
    lifecycle.emit('child-process-starting', 'started', {
      executable: basename(options.command.executable),
      containmentBackend: containment.kind,
    })
    const child = await executeChild(
      options,
      executionContext,
      staging,
      containment,
      preflight,
      lifecycle,
    )
    lifecycle.markProcessSettled(containment.kind, child.cleanupOutcome)
    lifecycle.emit('child-process-terminal', 'succeeded', {
      terminal: child.processEvidence.terminal,
      terminationReason: child.terminationReason,
      cleanupOutcome: child.cleanupOutcome ??
        (containment.kind === 'inherited' ? 'deferred-to-outer-owner' : 'completed'),
    })
    finalizationStarted = true
    finalizationPhase = 'assemble-parent-collection'
    lifecycle.emit('attachment-finalization-started', 'started', {
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
    lifecycle.emit('attachment-finalization-completed', 'succeeded', {
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
      lifecycle.emit('attachment-ownership-transfer-failed', 'failed',
        lifecycleFailureContext(containment.kind, ownershipTransfer.phase, ownershipTransfer.failures))
    }
    const result = options.ownershipMode === 'inherited'
      ? finalResult
      : await resultWriter.writeFinal(finalResult)
    lifecycle.emit(
      options.ownershipMode === 'inherited'
        ? 'parent-finalization-candidate-ready'
        : 'final-result-written',
      'succeeded', {
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
      lifecycle.emit(
        'attachment-finalization-failed',
        'failed',
        lifecycleFailureContext(containment.kind, finalizationPhase, cause),
      )
    }
    return restoreProvisionalAndRollback(
      resultPath,
      provisional,
      cause,
      staging,
      containment.kind,
      lifecycle,
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
      options.containmentBackend !== undefined || options.processOwner !== undefined
    ) {
      throw new Error(
        'inherited sample ownership cannot be combined with a nested containment backend',
      )
    }
    if (options.outerProcessAuthority === undefined) {
      throw new Error('inherited sample ownership requires an explicit outer process authority')
    }
    return createInheritedProcessContainmentBackend(options.outerProcessAuthority)
  }
  if (options.outerProcessAuthority !== undefined) {
    throw new Error('owned sample containment must not claim an outer process authority')
  }
  if (options.containmentBackend !== undefined) {
    if (options.processOwner !== undefined) {
      throw new Error(
        'an injected containment backend cannot be combined with production containment authorities',
      )
    }
    return options.containmentBackend
  }
  if (options.processOwner === undefined) {
    throw new Error('owned sample containment requires an authenticated test process owner')
  }
  return createProductionContainmentBackend({ processOwner: options.processOwner })
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
  lifecycle: BrowserSampleLifecycleTraceOwner,
  event: BrowserSampleContainmentTrace,
): void {
  lifecycle.emit(
    `containment-${event.milestone}`,
    event.outcome,
    event.context,
  )
}

async function restoreProvisionalAndRollback(
  resultPath: string,
  provisional: BrowserSampleResult,
  cause: unknown,
  staging: BrowserSampleStaging | undefined,
  backend: BrowserSampleContainmentBackend['kind'],
  lifecycle: BrowserSampleLifecycleTraceOwner,
): Promise<never> {
  // Containment resolves only after the child can no longer mutate staging.
  // Reasserting the parsed snapshot then erases any pre-retirement spoof before
  // the failure becomes observable; this path can never authorize a final write.
  try {
    await writeAtomicJson(resultPath, provisional)
  } catch (restorationCause) {
    lifecycle.markCleanupFailed()
    throw new AggregateError(
      [cause, restorationCause],
      'failed to restore provisional browser sample authority',
      { cause: restorationCause },
    )
  }
  if (staging !== undefined) {
    lifecycle.emitCleanup('attachment-rollback-started', 'started', {
      backend,
      phase: 'remove-owned-staging',
      settledFailureCount: 0,
    })
    try {
      await staging.dispose()
      lifecycle.emitCleanup('attachment-rollback-completed', 'succeeded', {
        backend,
        phase: 'remove-owned-staging',
        settledFailureCount: 0,
      })
    } catch (cleanupCause) {
      lifecycle.markCleanupFailed()
      lifecycle.emitCleanup(
        'attachment-rollback-failed',
        'failed',
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

function childContext(
  options: BrowserSampleRunnerOptions,
  topologyLock: VerifiedTestIceTopologyLock,
  artifactRoot: string,
  childDirectory: string,
): ChildEvidenceContext {
  return Object.freeze({
    runId: options.runId,
    operationId: options.operationId,
    scenario: options.scenario,
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
  lifecycle: BrowserSampleLifecycleTraceOwner,
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
      capture: Object.freeze({ stdoutBytes: maximumBytes, stderrBytes: maximumBytes }),
    })
    const stdoutSummary = persistOwnedCapture(stdout, authority.output.stdout, maximumBytes, 'stdout')
    const stderrSummary = persistOwnedCapture(stderr, authority.output.stderr, maximumBytes, 'stderr')
    emitContainmentSnapshot(lifecycle, authority.traces)
    return Object.freeze({
      ...authority,
      stdout: stdoutSummary,
      stderr: stderrSummary,
    })
  } catch (cause) {
    if (cause instanceof BrowserSampleContainmentError) {
      emitContainmentSnapshot(lifecycle, cause.traces)
    }
    lifecycle.markContainmentFailure()
    throw cause
  } finally {
    try {
      await Promise.all([stdout.close(), stderr.close()])
    } catch (cause) {
      lifecycle.markCleanupFailed()
      throw cause
    }
  }
}

function emitContainmentSnapshot(
  lifecycle: BrowserSampleLifecycleTraceOwner,
  snapshot: BrowserSampleContainmentError['traces'],
): void {
  if (!snapshot.completed || snapshot.truncated ||
      snapshot.observedEvents !== snapshot.capturedEvents) {
    throw new Error('sample containment returned incomplete terminal trace evidence')
  }
  for (const event of snapshot.events) emitContainmentTrace(lifecycle, event)
}

function persistOwnedCapture(
  destination: BoundedFileCapture,
  snapshot: OwnedByteSnapshot,
  maximumBytes: number,
  label: string,
): CaptureSummary {
  const bytes = snapshot.bytes()
  if (!snapshot.completed || !Number.isSafeInteger(snapshot.observedBytes) ||
      !Number.isSafeInteger(snapshot.capturedBytes) || snapshot.observedBytes < snapshot.capturedBytes ||
      snapshot.capturedBytes > maximumBytes || bytes.byteLength !== snapshot.capturedBytes ||
      snapshot.truncated !== (snapshot.observedBytes > snapshot.capturedBytes)) {
    throw new Error(`sample containment returned invalid ${label} capture evidence`)
  }
  destination.consume(bytes)
  return Object.freeze({
    observedBytes: snapshot.observedBytes,
    capturedBytes: snapshot.capturedBytes,
    truncated: snapshot.truncated,
  })
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

type BrowserSampleCleanupOutcome =
  | 'not-required'
  | 'pending'
  | 'completed'
  | 'failed'
  | 'deferred-to-outer-owner'

class BrowserSampleLifecycleTraceOwner {
  readonly #identity: BrowserSampleIdentity
  readonly #append: (trace: BrowserSampleTrace) => void
  readonly #ownershipMode: 'owned' | 'inherited'
  #lastMilestone = 'operation-started'
  #cleanupOutcome: BrowserSampleCleanupOutcome = 'not-required'
  #terminal = false

  constructor(identity: BrowserSampleRunnerOptions, append: (trace: BrowserSampleTrace) => void) {
    this.#identity = identity
    this.#append = append
    this.#ownershipMode = identity.ownershipMode ?? 'owned'
  }

  start(): void {
    this.#deliver('operation-started', 'started', {
      ownershipMode: this.#ownershipMode,
    })
  }

  emit(
    milestone: string,
    outcome: BrowserSampleTrace['outcome'],
    context?: Readonly<Record<string, unknown>>,
  ): void {
    if (this.#terminal) return
    this.#lastMilestone = milestone
    this.#observeCleanup(context?.cleanupOutcome)
    this.#deliver(milestone, outcome, context)
  }

  emitCleanup(
    milestone: string,
    outcome: BrowserSampleTrace['outcome'],
    context?: Readonly<Record<string, unknown>>,
  ): void {
    if (this.#terminal) return
    this.#observeCleanup(context?.cleanupOutcome)
    this.#deliver(milestone, outcome, context)
  }

  markCleanupPending(): void {
    this.#cleanupOutcome = 'pending'
  }

  markProcessSettled(
    backend: BrowserSampleContainmentBackend['kind'],
    outcome: 'completed' | 'failed' | undefined,
  ): void {
    this.#cleanupOutcome = outcome ?? (backend === 'inherited'
      ? 'deferred-to-outer-owner'
      : 'completed')
  }

  markContainmentFailure(): void {
    if (this.#cleanupOutcome === 'pending') this.#cleanupOutcome = 'failed'
  }

  markCleanupFailed(): void {
    this.#cleanupOutcome = 'failed'
  }

  terminal(
    outcome: BrowserSampleTrace['outcome'],
    context: Readonly<Record<string, unknown>>,
  ): void {
    if (this.#terminal) return
    this.#terminal = true
    if (this.#cleanupOutcome === 'pending') this.#cleanupOutcome = 'failed'
    this.#deliver('operation-terminal', outcome, {
      cleanupOutcome: this.#cleanupOutcome,
      lastMilestone: this.#lastMilestone,
      ...context,
    })
  }

  #observeCleanup(value: unknown): void {
    if (value === 'completed' || value === 'failed' || value === 'deferred-to-outer-owner') {
      this.#cleanupOutcome = value
    }
  }

  #deliver(
    milestone: string,
    outcome: BrowserSampleTrace['outcome'],
    context?: Readonly<Record<string, unknown>>,
  ): void {
    this.#append(this.#event(milestone, outcome, context))
  }

  #event(
    milestone: string,
    outcome: BrowserSampleTrace['outcome'],
    context?: Readonly<Record<string, unknown>>,
  ): BrowserSampleTrace {
    return Object.freeze({
      schemaVersion: BROWSER_SAMPLE_TRACE_SCHEMA_VERSION,
      component: 'browser-evidence-runner',
      runId: this.#identity.runId,
      operationId: this.#identity.operationId,
      scenario: this.#identity.scenario,
      outcome,
      milestone,
      suite: this.#identity.suite,
      browser: this.#identity.browser,
      sampleIndex: this.#identity.sampleIndex,
      ...(context === undefined ? {} : { context: Object.freeze({ ...context }) }),
    })
  }
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
