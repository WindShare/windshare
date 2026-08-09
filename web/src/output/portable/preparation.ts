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
import type { CanonicalModifiedTime } from '../../transfer/directory-admission'
import type {
  PreparationAdmissionReason,
  ReceiveLifecycleState,
} from '../workspace/state'
import type {
  PreparationDirectoryEntry,
  PreparationFileEntry,
  PreparationManifestEntry,
} from '../workspace/preparation'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalI64,
  canonicalIdentity,
  canonicalPath,
  canonicalRecord,
  canonicalU8,
  canonicalU32,
  canonicalU64,
  concatCanonicalBytes,
} from '../workspace/canonical'
import {
  planZipLayout,
  type SealedZipLayoutPlanV1,
} from '../zip-layout/layout'
import type { ZipEntrySpec } from '../zip-layout/policy'
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

const PORTABLE_PREPARATION_DOMAIN = 'windshare/portable-preparation/v1'
const PORTABLE_PREPARATION_PAGE_DOMAIN = 'windshare/portable-preparation/v1/page'
const PORTABLE_PREPARATION_PAGE_ENTRIES = 256
const PORTABLE_PREPARATION_METADATA_LIMIT = 16_777_216n
const CATALOG_IDENTITY_BYTES = 16
const DIGEST_BYTES = 32
const U64_MAXIMUM = 0xffff_ffff_ffff_ffffn
const UTF8_ENCODER = new TextEncoder()

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

class PortablePreparationAdmissionError extends TypeError {
  readonly reason: PreparationAdmissionReason
  readonly preparationManifestDigest: string | undefined

  constructor(
    reason: PreparationAdmissionReason,
    message: string,
    options?: ErrorOptions & { readonly preparationManifestDigest?: string },
  ) {
    super(message, options)
    this.name = 'PortablePreparationAdmissionError'
    this.reason = reason
    this.preparationManifestDigest = options?.preparationManifestDigest
  }
}

function validateOriginalPreparation(
  intent: Parameters<V2PortableOriginalExecutionRoute['prepare']>[0],
  evidence: ExactPreparationEvidence,
): PreparationFileEntry {
  validateEvidenceEnvelope(evidence)
  if (evidence.entryCount !== 1n || evidence.fileCount !== 1n ||
      evidence.directoryCount !== 0n || evidence.entries.length !== 1) {
    throw admissionError('generation-mismatch', 'portable original preparation is not one file')
  }
  const entry = evidence.entries[0]
  if (entry?.kind !== 'file' ||
      entry.fileId !== intent.artifact.fileId ||
      entry.sourcePath.join('/') !== intent.artifact.sourcePath ||
      entry.artifactPath.length !== 1 ||
      entry.artifactPath[0] !== intent.artifact.suggestedName ||
      evidence.selectedRawBytes !== entry.exactSize) {
    throw admissionError(
      'generation-mismatch',
      'portable original preparation changed the frozen file artifact',
    )
  }
  requireGenerationAuthority(evidence, entry.containingDirectoryId, entry.generation)
  return entry
}

function validateZipPreparation(
  intent: Parameters<V2PortableZipExecutionRoute['prepare']>[0],
  evidence: ExactPreparationEvidence,
): void {
  validateEvidenceEnvelope(evidence)
  const generations = generationMap(evidence)
  const sourceIdentities = new Set<string>()
  const roots: PreparationDirectoryEntry[] = []
  for (const entry of evidence.entries) {
    validateZipPreparationEntry(intent, entry, generations, sourceIdentities)
    if (entry.kind === 'directory' && entry.role === 'result-root') roots.push(entry)
  }
  if (roots.length !== 1 || roots[0]!.artifactPath.length !== 1) {
    throw admissionError('generation-mismatch', 'portable ZIP lacks one result root')
  }
  validateZipPreparationRoot(intent, roots[0]!)
}

function validateZipPreparationEntry(
  intent: Parameters<V2PortableZipExecutionRoute['prepare']>[0],
  entry: PreparationManifestEntry,
  generations: ReadonlyMap<string, string>,
  sourceIdentities: Set<string>,
): void {
  const sourceIdentity = entry.kind === 'directory'
    ? `directory:${entry.directoryId}`
    : `file:${entry.fileId}`
  if (sourceIdentities.has(sourceIdentity)) {
    throw admissionError('generation-mismatch', 'portable preparation repeats a source identity')
  }
  sourceIdentities.add(sourceIdentity)
  const directoryId = entry.kind === 'directory'
    ? entry.directoryId
    : entry.containingDirectoryId
  if (generations.get(directoryId) !== entry.generation) {
    throw admissionError('generation-mismatch', 'portable preparation lacks generation authority')
  }
  if (entry.artifactPath[0] !== intent.artifact.layout.name) {
    throw admissionError('generation-mismatch', 'portable ZIP entry escaped its result root')
  }
}

function validateZipPreparationRoot(
  intent: Parameters<V2PortableZipExecutionRoute['prepare']>[0],
  root: PreparationDirectoryEntry,
): void {
  const anchor = intent.artifact.layout.anchor
  if (anchor.kind === 'synthetic-root') {
    if (root.sourcePath.length !== 0 || root.directoryId !== intent.syntheticRoot) {
      throw admissionError('generation-mismatch', 'portable ZIP synthetic root changed')
    }
    return
  }
  if (root.directoryId !== anchor.directoryId ||
      root.sourcePath.join('/') !== anchor.sourcePath) {
    throw admissionError('generation-mismatch', 'portable ZIP result-root anchor changed')
  }
}

function validateEvidenceEnvelope(evidence: ExactPreparationEvidence): void {
  if (!Array.isArray(evidence.entries) || !Array.isArray(evidence.generations)) {
    throw admissionError('generation-mismatch', 'portable exact preparation is malformed')
  }
  const fileCount = BigInt(evidence.entries.filter(entry => entry.kind === 'file').length)
  const directoryCount = BigInt(evidence.entries.length) - fileCount
  let selectedRawBytes = 0n
  for (const entry of evidence.entries) {
    if (entry.kind === 'file') {
      selectedRawBytes = checkedAdd(selectedRawBytes, entry.exactSize)
    }
  }
  if (evidence.entryCount !== BigInt(evidence.entries.length) ||
      evidence.fileCount !== fileCount ||
      evidence.directoryCount !== directoryCount ||
      evidence.selectedRawBytes !== selectedRawBytes) {
    throw admissionError('generation-mismatch', 'portable preparation aggregates changed')
  }
  for (let index = 1; index < evidence.entries.length; index += 1) {
    if (comparePreparationEntries(evidence.entries[index - 1]!, evidence.entries[index]!) >= 0) {
      throw admissionError('generation-mismatch', 'portable preparation entries are not canonical')
    }
  }
  for (let index = 1; index < evidence.generations.length; index += 1) {
    if (compareUTF8(evidence.generations[index - 1]!.directoryId,
      evidence.generations[index]!.directoryId) >= 0) {
      throw admissionError('generation-mismatch', 'portable generation evidence is not canonical')
    }
  }
}

function generationMap(evidence: ExactPreparationEvidence): ReadonlyMap<string, string> {
  const result = new Map<string, string>()
  for (const generation of evidence.generations) {
    canonicalIdentity(generation.directoryId, CATALOG_IDENTITY_BYTES, 'directory ID')
    canonicalIdentity(generation.generation, CATALOG_IDENTITY_BYTES, 'directory generation')
    if (result.has(generation.directoryId)) {
      throw admissionError('generation-mismatch', 'portable preparation repeats a generation')
    }
    result.set(generation.directoryId, generation.generation)
  }
  return result
}

function requireGenerationAuthority(
  evidence: ExactPreparationEvidence,
  directoryId: string,
  generation: string,
): void {
  if (generationMap(evidence).get(directoryId) !== generation) {
    throw admissionError('generation-mismatch', 'portable original lacks generation authority')
  }
}

async function planPortableZipLayout(
  intent: Parameters<V2PortableZipExecutionRoute['prepare']>[0],
  evidence: ExactPreparationEvidence,
  preparationManifestDigest: string,
): Promise<SealedZipLayoutPlanV1> {
  try {
    return await planZipLayout({
      receiveIntentDigest: intent.digest,
      artifactDigest: intent.artifact.digest,
      preparationManifestDigest,
      entries: evidence.entries.map(zipEntrySpec),
    })
  } catch (error) {
    throw normalizeZipAdmissionError(error)
  }
}

function zipEntrySpec(entry: PreparationManifestEntry): ZipEntrySpec {
  const modifiedTimeMilliseconds = entry.modifiedTime === undefined
    ? undefined
    : entry.modifiedTime.seconds * 1_000n +
      BigInt(Math.trunc(entry.modifiedTime.nanoseconds / 1_000_000))
  if (entry.kind === 'directory') {
    return Object.freeze({
      kind: 'directory',
      path: entry.artifactPath,
      ...(modifiedTimeMilliseconds === undefined ? {} : { modifiedTimeMilliseconds }),
    })
  }
  return Object.freeze({
    kind: 'file',
    path: entry.artifactPath,
    exactSize: entry.exactSize,
    ...(modifiedTimeMilliseconds === undefined ? {} : { modifiedTimeMilliseconds }),
  })
}

async function sealPortablePreparation(
  intent: ReceiveIntent,
  evidence: ExactPreparationEvidence,
): Promise<string> {
  let metadataBytes = 0n
  const account = (bytes: Uint8Array): Uint8Array => {
    metadataBytes = checkedAdd(metadataBytes, BigInt(bytes.byteLength))
    if (metadataBytes > PORTABLE_PREPARATION_METADATA_LIMIT) {
      throw admissionError('metadata-limit', 'portable preparation metadata exceeds its hard limit')
    }
    return bytes
  }
  const generationDigests = await digestPages(
    evidence.generations,
    canonicalGeneration,
    account,
  )
  const entryDigests = await digestPages(
    evidence.entries,
    canonicalPreparationEntry,
    account,
  )
  const envelope = account(canonicalRecord(PORTABLE_PREPARATION_DOMAIN, 1, [
    canonicalFrame(canonicalIdentity(intent.digest, DIGEST_BYTES, 'receive intent digest')),
    canonicalFrame(canonicalIdentity(intent.artifact.digest, DIGEST_BYTES, 'artifact digest')),
    canonicalFrame(canonicalU64(evidence.entryCount)),
    canonicalFrame(canonicalU64(evidence.fileCount)),
    canonicalFrame(canonicalU64(evidence.directoryCount)),
    canonicalFrame(canonicalU64(evidence.selectedRawBytes)),
    canonicalFrame(canonicalU64(BigInt(generationDigests.length))),
    ...generationDigests.map(digest =>
      canonicalFrame(canonicalIdentity(digest, DIGEST_BYTES, 'generation page digest'))),
    canonicalFrame(canonicalU64(BigInt(entryDigests.length))),
    ...entryDigests.map(digest =>
      canonicalFrame(canonicalIdentity(digest, DIGEST_BYTES, 'entry page digest'))),
  ]))
  return canonicalDigest(envelope)
}

async function digestPages<Entry>(
  entries: readonly Entry[],
  encode: (entry: Entry) => Uint8Array,
  account: (bytes: Uint8Array) => Uint8Array,
): Promise<readonly string[]> {
  const digests: string[] = []
  for (let start = 0; start < entries.length; start += PORTABLE_PREPARATION_PAGE_ENTRIES) {
    const pageEntries = entries
      .slice(start, start + PORTABLE_PREPARATION_PAGE_ENTRIES)
      .map(entry => canonicalFrame(account(encode(entry))))
    const page = account(canonicalRecord(PORTABLE_PREPARATION_PAGE_DOMAIN, 1, [
      canonicalFrame(canonicalU64(BigInt(start / PORTABLE_PREPARATION_PAGE_ENTRIES))),
      canonicalFrame(canonicalU64(BigInt(pageEntries.length))),
      ...pageEntries,
    ]))
    digests.push(await canonicalDigest(page))
  }
  return Object.freeze(digests)
}

function canonicalGeneration(
  generation: ExactPreparationEvidence['generations'][number],
): Uint8Array {
  return concatCanonicalBytes([
    canonicalFrame(canonicalIdentity(
      generation.directoryId,
      CATALOG_IDENTITY_BYTES,
      'directory ID',
    )),
    canonicalFrame(canonicalIdentity(
      generation.generation,
      CATALOG_IDENTITY_BYTES,
      'directory generation',
    )),
  ])
}

function canonicalPreparationEntry(entry: PreparationManifestEntry): Uint8Array {
  const common = [
    canonicalFrame(canonicalEvidencePath(entry.sourcePath)),
    canonicalFrame(canonicalPath(entry.artifactPath)),
    canonicalFrame(canonicalModifiedTime(entry.modifiedTime)),
  ]
  if (entry.kind === 'directory') {
    return concatCanonicalBytes([
      canonicalU8(1),
      ...common,
      canonicalFrame(canonicalIdentity(entry.directoryId, CATALOG_IDENTITY_BYTES, 'directory ID')),
      canonicalFrame(canonicalIdentity(
        entry.generation,
        CATALOG_IDENTITY_BYTES,
        'directory generation',
      )),
      canonicalFrame(canonicalU8(directoryRoleByte(entry.role))),
    ])
  }
  return concatCanonicalBytes([
    canonicalU8(2),
    ...common,
    canonicalFrame(canonicalIdentity(entry.fileId, CATALOG_IDENTITY_BYTES, 'file ID')),
    canonicalFrame(canonicalIdentity(
      entry.containingDirectoryId,
      CATALOG_IDENTITY_BYTES,
      'containing directory ID',
    )),
    canonicalFrame(canonicalIdentity(
      entry.generation,
      CATALOG_IDENTITY_BYTES,
      'directory generation',
    )),
    canonicalFrame(canonicalU64(entry.exactSize)),
  ])
}

function canonicalEvidencePath(path: readonly string[]): Uint8Array {
  return path.length === 0 ? canonicalU64(0n) : canonicalPath(path)
}

function canonicalModifiedTime(value: CanonicalModifiedTime | undefined): Uint8Array {
  if (value === undefined) return canonicalU8(0)
  return concatCanonicalBytes([
    canonicalU8(1),
    canonicalFrame(canonicalI64(value.seconds)),
    canonicalFrame(canonicalU32(value.nanoseconds)),
    canonicalFrame(canonicalU8(value.precision)),
  ])
}

function directoryRoleByte(role: PreparationDirectoryEntry['role']): number {
  switch (role) {
    case 'result-root': return 1
    case 'necessary-ancestor': return 2
    case 'explicitly-selected-empty': return 3
  }
}

function requirePortableArtifactBudget(intent: ReceiveIntent, exactBytes: bigint): void {
  if (intent.plan.kind !== 'portable-handoff') {
    throw new TypeError('portable admission requires a portable intent')
  }
  if (typeof exactBytes !== 'bigint' || exactBytes < 0n || exactBytes > U64_MAXIMUM) {
    throw admissionError('arithmetic-overflow', 'portable artifact length is outside u64')
  }
  const binding = intent.plan.portable
  const requiredParts = exactBytes === 0n
    ? 0n
    : (exactBytes + binding.assemblyPartBytes - 1n) / binding.assemblyPartBytes
  if (exactBytes > binding.maximumArtifactBytes || requiredParts > binding.maximumParts) {
    throw admissionError('artifact-limit', 'portable artifact exceeds its frozen hard limit')
  }
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

function normalizeOriginalAdmissionError(error: unknown): unknown {
  if (error instanceof PortablePreparationAdmissionError ||
      error instanceof DOMException) return error
  if (error instanceof RangeError) {
    return admissionError('arithmetic-overflow', 'portable original preparation overflowed', error)
  }
  if (error instanceof TypeError) {
    return admissionError('generation-mismatch', 'portable original preparation is invalid', error)
  }
  return error
}

function normalizeZipAdmissionError(error: unknown): unknown {
  if (error instanceof PortablePreparationAdmissionError ||
      error instanceof DOMException) return error
  if (error instanceof RangeError) {
    return admissionError('entry-limit', 'portable ZIP layout exceeds its bounded policy', error)
  }
  if (error instanceof TypeError) {
    return admissionError('generation-mismatch', 'portable ZIP preparation is invalid', error)
  }
  return error
}

function admissionError(
  reason: PreparationAdmissionReason,
  message: string,
  cause?: unknown,
): PortablePreparationAdmissionError {
  return new PortablePreparationAdmissionError(
    reason,
    message,
    cause === undefined ? undefined : { cause },
  )
}

function comparePreparationEntries(
  left: PreparationManifestEntry,
  right: PreparationManifestEntry,
): number {
  const path = compareUTF8(left.artifactPath.join('/'), right.artifactPath.join('/'))
  if (path !== 0) return path
  if (left.kind === right.kind) return 0
  return left.kind === 'directory' ? -1 : 1
}

function compareUTF8(left: string, right: string): number {
  const a = UTF8_ENCODER.encode(left)
  const b = UTF8_ENCODER.encode(right)
  const length = Math.min(a.byteLength, b.byteLength)
  for (let index = 0; index < length; index += 1) {
    const difference = a[index]! - b[index]!
    if (difference !== 0) return difference
  }
  return a.byteLength - b.byteLength
}

function checkedAdd(left: bigint, right: bigint): bigint {
  if (typeof left !== 'bigint' || typeof right !== 'bigint' ||
      left < 0n || right < 0n || left > U64_MAXIMUM - right) {
    throw admissionError('arithmetic-overflow', 'portable preparation accounting overflowed u64')
  }
  return left + right
}
