import type {
  BrowserHandoffTargetOffer,
  PortableEnvironmentOffer,
} from '../planning'
import {
  DEFAULT_PORTABLE_ARTIFACT_LIMIT,
  DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
  DEFAULT_PORTABLE_MAXIMUM_PARTS,
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  type ReceiveIntent,
} from '../../transfer/intent'
import type {
  ExactPreparationEvidence,
  ExecutionAdmissionResult,
  MaterializationSummary,
  PlanPauseRequest,
  PlanSettlementRequest,
  PortableExecution,
} from '../../transfer/output-session'
import type { SuccessfulTransferWorkerSettlement } from '../../transfer/outcome'
import type {
  V2PortableOriginalExecutionRoute,
  V2PortableZipExecutionRoute,
} from '../../transfer/settlement/v2-plan-authority'
import type {
  PreparationAdmissionReason,
  ReceiveLifecycleState,
} from '../workspace/state'
import type { PreparationFileEntry } from '../workspace/preparation'
import type { ZipCentralDirectorySpool } from '../streams/zip-spool'
import {
  issuePortableArtifactAdmission,
  type PortableArtifactAdmission,
} from './admission'
import {
  openPortableHandoff,
  type BrowserHandoffPublisher,
  type DownloadStarted,
  type PortableAssemblyPorts,
  type PortableHandoffSession,
  type PortableRestartReason,
} from './browser-download'
import {
  PortableOriginalOutputSession,
  PortableSealedZipOutputSession,
  type PortablePreparedOutput,
  type PortableZipArchiveWriterFactory,
} from './output-session'
import {
  PortablePreparationAdmissionError,
  normalizeOriginalAdmissionError,
  normalizeZipAdmissionError,
  planPortableZipLayout,
  requirePortableArtifactBudget,
  sealPortablePreparation,
  validateOriginalPreparation,
  validateZipPreparation,
} from './preparation-evidence'

type PortableRejectedState = Extract<
  ReceiveLifecycleState,
  { readonly kind: 'discarded' | 'needs-attention' }
>
type PortableAbortState = Extract<
  ReceiveLifecycleState,
  { readonly kind: 'restart-required' | 'discarded' | 'needs-attention' }
>
type PortableDownloadState = Extract<
  ReceiveLifecycleState,
  { readonly kind: 'download-started'; readonly attemptKind: 'portable' }
>

export interface PortableAdmissionRejectionRecord {
  readonly intent: ReceiveIntent
  readonly reason: PreparationAdmissionReason
  readonly preparationManifestDigest?: string
}

export interface PortableDownloadStartedRecord {
  readonly intent: ReceiveIntent
  readonly attemptId: string
  readonly admission: PortableArtifactAdmission
  readonly result: DownloadStarted
}

export interface PortableAbortRecord {
  readonly intent: ReceiveIntent
  readonly attemptId: string
  readonly reason: PortableRestartReason
  readonly cleanup: 'clean' | 'unknown'
}

export interface PortableExecutionLifecycleAuthority {
  rejectAdmission(
    record: PortableAdmissionRejectionRecord,
    signal: AbortSignal,
  ): Promise<PortableRejectedState>
  recordDownloadStarted(
    record: PortableDownloadStartedRecord,
    signal: AbortSignal,
  ): Promise<PortableDownloadState>
  recordAbort(
    record: PortableAbortRecord,
    signal: AbortSignal,
  ): Promise<PortableAbortState>
}

export interface PortableExecutionEnvironment {
  readonly portable: PortableEnvironmentOffer
  readonly handoffTarget: BrowserHandoffTargetOffer
}

export interface PortableExecutionRoutePorts {
  readonly environment: PortableExecutionEnvironment
  readonly attemptId: string
  readonly publisher: BrowserHandoffPublisher
  readonly assembly: PortableAssemblyPorts
  readonly lifecycle: PortableExecutionLifecycleAuthority
  readonly createZipSpool?: () => ZipCentralDirectorySpool
  readonly createZipWriter?: PortableZipArchiveWriterFactory
}

export interface PortableExecutionRoutes {
  readonly portableOriginal?: V2PortableOriginalExecutionRoute
  readonly portableZip?: V2PortableZipExecutionRoute
}

/**
 * Route presence is the production capability fact. Unsupported handoff engines
 * and missing ZIP-spool producers remove the matching route; neither condition
 * authorizes W4-B to choose a workspace or partial-ZIP fallback.
 */
export function createPortableExecutionRoutes(
  ports: PortableExecutionRoutePorts,
): PortableExecutionRoutes {
  assertPortableEnvironmentEnvelope(ports.environment)
  assertRuntimePorts(ports)
  if (!ports.environment.handoffTarget.supportsPortableArtifact) return Object.freeze({})

  const portableOriginal: V2PortableOriginalExecutionRoute = Object.freeze({
    prepare: (
      intent: Parameters<V2PortableOriginalExecutionRoute['prepare']>[0],
      evidence: ExactPreparationEvidence,
      signal: AbortSignal,
    ) => preparePortableOriginal(intent, evidence, signal, ports),
  })
  if (ports.createZipSpool === undefined) return Object.freeze({ portableOriginal })

  const portableZip: V2PortableZipExecutionRoute = Object.freeze({
    prepare: (
      intent: Parameters<V2PortableZipExecutionRoute['prepare']>[0],
      evidence: ExactPreparationEvidence,
      signal: AbortSignal,
    ) => preparePortableZip(intent, evidence, signal, ports),
  })
  return Object.freeze({ portableOriginal, portableZip })
}

async function preparePortableOriginal(
  intent: Parameters<V2PortableOriginalExecutionRoute['prepare']>[0],
  evidence: ExactPreparationEvidence,
  signal: AbortSignal,
  ports: PortableExecutionRoutePorts,
): Promise<ExecutionAdmissionResult<PortableExecution>> {
  signal.throwIfAborted()
  try {
    assertEnvironmentBinding(intent, ports.environment)
    const original = validateOriginalPreparation(intent, evidence)
    const preparationManifestDigest = await sealPortablePreparation(intent, evidence)
    requirePortableArtifactBudget(intent, original.exactSize)
    const admission = issuePortableArtifactAdmission({
      receiveIntentDigest: intent.digest,
      artifactDigest: intent.artifact.digest,
      sealedArtifact: {
        artifactKind: 'original-file',
        preparationManifestDigest,
      },
      exactArtifactBytes: original.exactSize,
    })
    const handoff = await openPortableHandoff({
      intent,
      admission,
      attemptId: ports.attemptId,
      publisher: ports.publisher,
      assembly: ports.assembly,
    })
    const output = new PortableOriginalOutputSession({
      intent,
      entry: original,
      handoff,
    })
    return acceptedPortableExecution({
      intent,
      evidence,
      admission,
      handoff,
      output,
      lifecycle: ports.lifecycle,
      attemptId: ports.attemptId,
    })
  } catch (error) {
    return rejectAdmissionError(
      intent,
      signal,
      ports.lifecycle,
      normalizeOriginalAdmissionError(error),
    )
  }
}

async function preparePortableZip(
  intent: Parameters<V2PortableZipExecutionRoute['prepare']>[0],
  evidence: ExactPreparationEvidence,
  signal: AbortSignal,
  ports: PortableExecutionRoutePorts,
): Promise<ExecutionAdmissionResult<PortableExecution>> {
  signal.throwIfAborted()
  const createZipSpool = ports.createZipSpool
  if (createZipSpool === undefined) {
    throw new DOMException('Portable ZIP output is unavailable', 'NotSupportedError')
  }
  try {
    assertEnvironmentBinding(intent, ports.environment)
    validateZipPreparation(intent, evidence)
    const preparationManifestDigest = await sealPortablePreparation(intent, evidence)
    const layout = await planPortableZipLayout(intent, evidence, preparationManifestDigest)
    requirePortableArtifactBudget(intent, layout.exactArchiveBytes)
    const admission = issuePortableArtifactAdmission({
      receiveIntentDigest: intent.digest,
      artifactDigest: intent.artifact.digest,
      sealedArtifact: {
        artifactKind: 'zip-archive',
        preparationManifestDigest,
        sealedZipLayoutDigest: layout.digest,
      },
      exactArtifactBytes: layout.exactArchiveBytes,
    })
    const handoff = await openPortableHandoff({
      intent,
      admission,
      attemptId: ports.attemptId,
      publisher: ports.publisher,
      assembly: ports.assembly,
    })
    const files = evidence.entries.filter(
      (entry): entry is PreparationFileEntry => entry.kind === 'file',
    )
    const output = new PortableSealedZipOutputSession({
      intent,
      files,
      layout,
      handoff,
      createSpool: createZipSpool,
      ...(ports.createZipWriter === undefined
        ? {}
        : { createWriter: ports.createZipWriter }),
    })
    return acceptedPortableExecution({
      intent,
      evidence,
      admission,
      handoff,
      output,
      lifecycle: ports.lifecycle,
      attemptId: ports.attemptId,
    })
  } catch (error) {
    return rejectAdmissionError(intent, signal, ports.lifecycle, normalizeZipAdmissionError(error))
  }
}

function acceptedPortableExecution(input: Readonly<{
  intent: ReceiveIntent
  evidence: ExactPreparationEvidence
  admission: PortableArtifactAdmission
  handoff: PortableHandoffSession
  output: PortablePreparedOutput
  lifecycle: PortableExecutionLifecycleAuthority
  attemptId: string
}>): ExecutionAdmissionResult<PortableExecution> {
  const controller = new PortableExecutionController(input)
  const execution: PortableExecution = Object.freeze({
    planKind: 'portable-handoff',
    output: input.output,
    pause: (request: PlanPauseRequest, signal: AbortSignal) =>
      controller.pause(request, signal),
    settle: (
      request: PlanSettlementRequest<SuccessfulTransferWorkerSettlement>,
      signal: AbortSignal,
    ) => controller.settle(request, signal),
  })
  return Object.freeze({ kind: 'accepted', execution })
}

class PortableExecutionController {
  readonly #intent: ReceiveIntent
  readonly #evidence: ExactPreparationEvidence
  readonly #admission: PortableArtifactAdmission
  readonly #handoff: PortableHandoffSession
  readonly #output: PortablePreparedOutput
  readonly #lifecycle: PortableExecutionLifecycleAuthority
  readonly #attemptId: string
  #downloadStarted: DownloadStarted | undefined
  #settlementClaimed = false

  constructor(input: Readonly<{
    intent: ReceiveIntent
    evidence: ExactPreparationEvidence
    admission: PortableArtifactAdmission
    handoff: PortableHandoffSession
    output: PortablePreparedOutput
    lifecycle: PortableExecutionLifecycleAuthority
    attemptId: string
  }>) {
    this.#intent = input.intent
    this.#evidence = input.evidence
    this.#admission = input.admission
    this.#handoff = input.handoff
    this.#output = input.output
    this.#lifecycle = input.lifecycle
    this.#attemptId = input.attemptId
    input.handoff.result.then(
      result => { this.#downloadStarted = result },
      () => undefined,
    )
  }

  async settle(
    request: PlanSettlementRequest<SuccessfulTransferWorkerSettlement>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState> {
    this.#claimSettlement()
    signal.throwIfAborted()
    assertSuccessfulMaterialization(request, this.#evidence)
    await this.#output.finalize(signal)
    const result = await this.#handoff.result
    if (this.#output.cleanupPending) {
      await this.#output.retryCleanup()
      if (this.#output.cleanupPending) {
        throw new Error('portable output cleanup remains pending')
      }
    }
    const state = await this.#lifecycle.recordDownloadStarted({
      intent: this.#intent,
      attemptId: this.#attemptId,
      admission: this.#admission,
      result,
    }, signal)
    signal.throwIfAborted()
    return validateDownloadState(this.#intent, this.#attemptId, state)
  }

  async pause(
    request: PlanPauseRequest,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState> {
    this.#claimSettlement()
    let cleanup: PortableAbortRecord['cleanup'] = 'clean'
    try {
      await this.#output.abort(request.reason)
      if (this.#output.cleanupPending) await this.#output.retryCleanup()
      if (this.#output.cleanupPending) cleanup = 'unknown'
    } catch {
      cleanup = 'unknown'
    }
    await Promise.resolve()
    if (this.#downloadStarted !== undefined) {
      const state = await this.#lifecycle.recordDownloadStarted({
        intent: this.#intent,
        attemptId: this.#attemptId,
        admission: this.#admission,
        result: this.#downloadStarted,
      }, signal)
      return validateDownloadState(this.#intent, this.#attemptId, state)
    }
    const state = await this.#lifecycle.recordAbort({
      intent: this.#intent,
      attemptId: this.#attemptId,
      reason: portableRestartReason(request.reason),
      cleanup,
    }, signal)
    signal.throwIfAborted()
    return validateAbortState(this.#intent, cleanup, state)
  }

  #claimSettlement(): void {
    if (this.#settlementClaimed) throw new Error('portable execution is already settled')
    this.#settlementClaimed = true
  }
}

async function rejectAdmissionError(
  intent: ReceiveIntent,
  signal: AbortSignal,
  lifecycle: PortableExecutionLifecycleAuthority,
  error: unknown,
): Promise<ExecutionAdmissionResult<PortableExecution>> {
  signal.throwIfAborted()
  if (!(error instanceof PortablePreparationAdmissionError)) throw error
  const state = await lifecycle.rejectAdmission({
    intent,
    reason: error.reason,
    ...(error.preparationManifestDigest === undefined
      ? {}
      : { preparationManifestDigest: error.preparationManifestDigest }),
  }, signal)
  signal.throwIfAborted()
  validateLifecycleIdentity(intent, state)
  if (state.kind !== 'discarded' && state.kind !== 'needs-attention') {
    throw new TypeError('portable admission rejection lacks cleanup authority')
  }
  return Object.freeze({ kind: 'rejected', state })
}

function assertSuccessfulMaterialization(
  request: PlanSettlementRequest<SuccessfulTransferWorkerSettlement>,
  evidence: ExactPreparationEvidence,
): void {
  if (request.worker.status !== 'Succeeded') {
    throw new TypeError('portable settlement requires successful workers')
  }
  assertMaterializationSummary(request.materialization, evidence)
}

function assertMaterializationSummary(
  summary: MaterializationSummary,
  evidence: ExactPreparationEvidence,
): void {
  if (summary.entryCount !== evidence.entryCount ||
      summary.fileCount !== evidence.fileCount ||
      summary.directoryCount !== evidence.directoryCount ||
      summary.rawBytes !== evidence.selectedRawBytes) {
    throw new TypeError('portable materialization does not match exact preparation')
  }
}

function assertPortableEnvironmentEnvelope(environment: PortableExecutionEnvironment): void {
  const portable = environment.portable
  const target = environment.handoffTarget
  if (portable.kind !== 'portable-memory' || portable.persistence !== 'none' ||
      portable.maximumArtifactBytes !== DEFAULT_PORTABLE_ARTIFACT_LIMIT ||
      portable.assemblyPartBytes !== DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES ||
      portable.maximumParts !== DEFAULT_PORTABLE_MAXIMUM_PARTS ||
      portable.objectUrlLeaseMilliseconds !== BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS) {
    throw new TypeError('portable execution requires the frozen portable EnvironmentOffer')
  }
  if (target.kind !== 'browser-handoff' || target.persistence !== 'none' ||
      target.objectUrlLeaseMilliseconds !== BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS ||
      typeof target.supportsPortableArtifact !== 'boolean') {
    throw new TypeError('portable execution requires a browser-handoff EnvironmentOffer fact')
  }
}

function assertEnvironmentBinding(
  intent: ReceiveIntent,
  environment: PortableExecutionEnvironment,
): void {
  if (intent.plan.kind !== 'portable-handoff' ||
      environment.portable.maximumArtifactBytes !== intent.plan.portable.maximumArtifactBytes ||
      environment.portable.assemblyPartBytes !== intent.plan.portable.assemblyPartBytes ||
      environment.portable.maximumParts !== intent.plan.portable.maximumParts ||
      environment.portable.objectUrlLeaseMilliseconds !==
        intent.plan.portable.objectUrlLeaseMilliseconds ||
      environment.handoffTarget.objectUrlLeaseMilliseconds !==
        intent.plan.portable.objectUrlLeaseMilliseconds ||
      !environment.handoffTarget.supportsPortableArtifact) {
    throw new DOMException('Portable browser execution is unavailable', 'NotSupportedError')
  }
}

function assertRuntimePorts(ports: PortableExecutionRoutePorts): void {
  if (typeof ports.attemptId !== 'string' || ports.attemptId.length === 0 ||
      ports.publisher === null || typeof ports.publisher !== 'object' ||
      typeof ports.publisher.handoff !== 'function' ||
      ports.assembly === null || typeof ports.assembly !== 'object' ||
      typeof ports.assembly.Blob !== 'function' ||
      typeof ports.assembly.WritableStream !== 'function' ||
      ports.lifecycle === null || typeof ports.lifecycle !== 'object' ||
      typeof ports.lifecycle.rejectAdmission !== 'function' ||
      typeof ports.lifecycle.recordDownloadStarted !== 'function' ||
      typeof ports.lifecycle.recordAbort !== 'function') {
    throw new TypeError('portable execution route ports are incomplete')
  }
}

function validateDownloadState(
  intent: ReceiveIntent,
  attemptId: string,
  state: PortableDownloadState,
): PortableDownloadState {
  validateLifecycleIdentity(intent, state)
  if (state.kind !== 'download-started' ||
      state.attemptKind !== 'portable' ||
      state.attemptId !== attemptId) {
    throw new TypeError('portable lifecycle did not record DownloadStarted')
  }
  return state
}

function validateAbortState(
  intent: ReceiveIntent,
  cleanup: PortableAbortRecord['cleanup'],
  state: PortableAbortState,
): PortableAbortState {
  validateLifecycleIdentity(intent, state)
  if (cleanup === 'unknown') {
    if (state.kind !== 'needs-attention' ||
        (state.reason !== 'cleanup-unknown' && state.reason !== 'publication-unknown')) {
      throw new TypeError('unknown portable cleanup must stop in NeedsAttention')
    }
    return state
  }
  if (state.kind === 'restart-required') {
    if (state.reason !== 'portable-aborted' && state.reason !== 'preparation-invalidated') {
      throw new TypeError('portable restart state has an invalid reason')
    }
    return state
  }
  if (state.kind !== 'discarded' && state.kind !== 'needs-attention') {
    throw new TypeError('portable abort did not reach a stable non-resumable state')
  }
  return state
}

function validateLifecycleIdentity(
  intent: ReceiveIntent,
  state: ReceiveLifecycleState,
): void {
  if (state.operationId !== intent.operationId ||
      state.receiveIntentDigest !== intent.digest ||
      typeof state.generation !== 'bigint' ||
      state.generation < 1n) {
    throw new TypeError('portable lifecycle state belongs to another operation')
  }
}

function portableRestartReason(reason: unknown): PortableRestartReason {
  return reason instanceof Error && reason.name === 'PortableHandoffError' &&
      'restartReason' in reason && reason.restartReason === 'preparation-invalidated'
    ? 'preparation-invalidated'
    : 'portable-aborted'
}
