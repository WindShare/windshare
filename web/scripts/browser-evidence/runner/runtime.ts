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
  type BrowserSampleIdentity,
  type BrowserSampleRunnerOptions,
  type BrowserSampleRunOutcome,
  type BrowserSampleTrace,
  type BrowserSampleTraceSink,
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
