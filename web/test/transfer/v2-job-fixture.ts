import type { V2CatalogClient } from '../../src/catalog/v2-client'
import type { V2CommittedDirectory } from '../../src/catalog/v2-page-store'
import type { V2CatalogEntry, V2CatalogPage } from '../../src/catalog/v2-records'
import { V2SelectionPolicy, type V2FrozenSelectionPolicy } from '../../src/catalog/v2-selection'
import { ByteRangeSet, FileGeometry, byteRange, type ByteRange } from '../../src/content/geometry'
import type { V2BlockRangeReader } from '../../src/content/v2-broker'
import type { V2OpenedRevision, V2RevisionReader } from '../../src/content/v2-session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { ReceiveLifecycleState } from '../../src/output/workspace/state'
import { DirectoryAdmissionLedger } from '../../src/transfer/directory-admission-ledger'
import {
  createDirectoryAdmissionScope,
  isolatedDirectorySettlement,
} from '../../src/transfer/directory-admission'
import { FaultScope, OutputFaultCode, outputFault } from '../../src/transfer/fault'
import {
  createCatalogRootDirectoryTreeArtifact,
  createDestinationReservationID,
  createDirectAtomicPlan,
  createDirectTreePlan,
  createManagedAtomicReservation,
  createNativeContainerRootReservation,
  createOperationID,
  createOriginalFileArtifact,
  createPortableBinding,
  createPortableHandoffPlan,
  createPortablePlanID,
  createReceiveIntent,
  createSelectionSpec,
  createSyntheticSelectionResultRoot,
  createWorkspaceBinding,
  createWorkspaceID,
  createWorkspaceThenPublishPlan,
  createZipArchiveArtifact,
  selectionRulesSpecFromPolicy,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import {
  VerifiedDurableRanges,
  outputCapabilities,
  outputSessionIdentity,
  snapshotOpenedOutputRevision,
  snapshotOutputFile,
  type DirectAtomicExecution,
  type DirectTreeExecution,
  type ExactPreparationEvidence,
  type ExactSingleFileEvidence,
  type IncrementalDirectoryOutput,
  type OutputFileRequest,
  type OutputSession,
  type PortableExecution,
  type V2PlanExecutionAuthority,
  type WorkspaceExecution,
} from '../../src/transfer/output-session'
import { TransferJob } from '../../src/transfer/v2-job'
import { V2FileRevisionChangedError } from '../../src/transfer/job/failures'

export function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

export function identityText(first: number): string {
  return encodeBase64Url(identity(first))
}

export function digestIdentity(first: number): string {
  const value = new Uint8Array(32)
  value[0] = first
  return encodeBase64Url(value)
}

export function catalogCommitment(first = 1): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(32)
  value[0] = first
  return value
}

export function directoryEntry(
  id: Uint8Array<ArrayBuffer>,
  name: string,
): Extract<V2CatalogEntry, { kind: 'directory' }> {
  return Object.freeze({ kind: 'directory', id, idText: encodeBase64Url(id), name })
}

export function fileEntry(
  id: Uint8Array<ArrayBuffer>,
  name: string,
  expectedSize: bigint,
): Extract<V2CatalogEntry, { kind: 'file' }> {
  return Object.freeze({ kind: 'file', id, idText: encodeBase64Url(id), name, expectedSize })
}

export interface CatalogDirectoryFixture {
  readonly id: Uint8Array<ArrayBuffer>
  readonly entries: readonly V2CatalogEntry[]
  readonly generation?: Uint8Array<ArrayBuffer>
  readonly omittedCount?: bigint
  readonly loadFailure?: unknown
  readonly beforePages?: () => void | Promise<void>
}

export function catalogFixture(directories: readonly CatalogDirectoryFixture[]): {
  readonly catalog: V2CatalogClient
  readonly loads: string[]
  readonly pageReads: string[]
} {
  const byId = new Map(directories.map(directory => [encodeBase64Url(directory.id), directory]))
  const loads: string[] = []
  const pageReads: string[] = []
  const catalog = {
    loadDirectory: async (id: Uint8Array) => {
      const idText = encodeBase64Url(id)
      loads.push(idText)
      const fixture = byId.get(idText)
      if (fixture?.loadFailure !== undefined) throw fixture.loadFailure
      if (fixture === undefined) throw new Error('catalog fixture loaded an unknown directory')
      return committedDirectory(fixture)
    },
    pages: async function* (directory: V2CommittedDirectory) {
      const fixture = byId.get(encodeBase64Url(directory.directoryId))
      if (fixture === undefined) throw new Error('catalog fixture lost a committed directory')
      await fixture.beforePages?.()
      pageReads.push(encodeBase64Url(directory.directoryId))
      yield catalogPage(fixture)
    },
  } as unknown as V2CatalogClient
  return { catalog, loads, pageReads }
}

function committedDirectory(fixture: CatalogDirectoryFixture): V2CommittedDirectory {
  return Object.freeze({
    directoryId: fixture.id.slice(),
    directoryIdText: encodeBase64Url(fixture.id),
    generation: (fixture.generation ?? identity(90)).slice(),
    generationText: encodeBase64Url(fixture.generation ?? identity(90)),
    pageCount: 1,
    entryCount: fixture.entries.length,
    omittedCount: fixture.omittedCount ?? 0n,
    terminalCommitment: catalogCommitment(fixture.id[0] ?? 1),
  })
}

function catalogPage(fixture: CatalogDirectoryFixture): V2CatalogPage {
  const entries = [...fixture.entries]
    .map(entry => Object.freeze({ ...entry, idText: encodeBase64Url(entry.id) }))
    .sort((left, right) => compareUtf8(left.name, right.name))
  return Object.freeze({
    shareInstance: identity(1),
    directoryId: fixture.id.slice(),
    directoryIdText: encodeBase64Url(fixture.id),
    generation: (fixture.generation ?? identity(90)).slice(),
    generationText: encodeBase64Url(fixture.generation ?? identity(90)),
    pageIndex: 0,
    terminal: true,
    previousCommitment: new Uint8Array(32),
    entries: Object.freeze(entries),
    omittedCount: fixture.omittedCount ?? 0n,
    objectCommitment: catalogCommitment(fixture.id[0] ?? 1),
    senderObjectBytes: 1,
  })
}

export function selectOnlyFile(file: Extract<V2CatalogEntry, { kind: 'file' }>): V2SelectionPolicy {
  const selection = new V2SelectionPolicy(false)
  selection.toggle(file, [identityText(2)])
  return selection
}

export type TestPlanKind = ReceiveIntent['plan']['kind']
export type TestArtifactKind = 'directory-tree' | 'original-file' | 'zip-archive'

export async function receiveIntentFixture(input: {
  readonly planKind: TestPlanKind
  readonly artifactKind: TestArtifactKind
  readonly selection: V2FrozenSelectionPolicy | V2SelectionPolicy
  readonly file?: Extract<V2CatalogEntry, { kind: 'file' }>
}): Promise<ReceiveIntent> {
  const selection = 'snapshot' in input.selection ? input.selection.snapshot() : input.selection
  const selectionSpec = await createSelectionSpec({
    shareInstance: identityText(1),
    syntheticRoot: identityText(2),
    rules: selectionRulesSpecFromPolicy(selection),
  })
  let artifact: ReceiveIntent['artifact']
  if (input.artifactKind === 'directory-tree') {
    artifact = await createCatalogRootDirectoryTreeArtifact()
  } else if (input.artifactKind === 'original-file') {
    artifact = await createOriginalFileArtifact({
      fileId: requiredFile(input.file).idText,
      sourcePath: requiredFile(input.file).name,
      suggestedName: requiredFile(input.file).name,
    })
  } else {
    artifact = await createZipArchiveArtifact(createSyntheticSelectionResultRoot())
  }
  const operationId = createOperationID()
  let plan: ReceiveIntent['plan']
  switch (input.planKind) {
    case 'direct-tree': {
      const reservation = await createNativeContainerRootReservation({
        operationId,
        reservationId: createDestinationReservationID(),
        artifact,
        authorityRef: digestIdentity(41),
      })
      plan = await createDirectTreePlan(artifact, reservation)
      break
    }
    case 'direct-atomic': {
      let requestedName = 'invalid'
      if (artifact.kind === 'original-file' || artifact.kind === 'zip-archive') {
        requestedName = artifact.suggestedName
      }
      const reservation = await createManagedAtomicReservation({
        operationId,
        reservationId: createDestinationReservationID(),
        artifact,
        authorityRef: digestIdentity(42),
        nameAuthority: 'application-chosen',
        requestedName,
        reservedName: requestedName,
        collisionIndex: 0,
      })
      plan = await createDirectAtomicPlan(artifact, reservation)
      break
    }
    case 'workspace-then-publish': {
      const workspace = await createWorkspaceBinding({
        operationId,
        workspaceId: createWorkspaceID(),
        artifact,
        repositoryRef: digestIdentity(43),
      })
      plan = await createWorkspaceThenPublishPlan(artifact, workspace)
      break
    }
    case 'portable-handoff': {
      const portable = await createPortableBinding({
        operationId,
        portablePlanId: createPortablePlanID(),
        artifact,
      })
      plan = await createPortableHandoffPlan(artifact, portable)
      break
    }
  }
  return createReceiveIntent({ selection: selectionSpec, artifact, plan })
}

function requiredFile(
  file: Extract<V2CatalogEntry, { kind: 'file' }> | undefined,
): Extract<V2CatalogEntry, { kind: 'file' }> {
  if (file === undefined) throw new TypeError('original-file fixture requires a file')
  return file
}

export interface ReaderFixtureOptions {
  readonly failRevisionFor?: string
  readonly failBlockFor?: string
  readonly beforeOpen?: (fileId: string) => void | Promise<void>
  readonly beforeRead?: (fileId: string) => void | Promise<void>
}

export function readerFixture(
  files: readonly Extract<V2CatalogEntry, { kind: 'file' }>[],
  events: string[] = [],
  options: ReaderFixtureOptions = {},
): {
  readonly revisions: V2RevisionReader
  readonly broker: V2BlockRangeReader
  readonly revisionRequests: string[]
  readonly blockRequests: string[]
  readonly releases: string[]
} {
  const byId = new Map(files.map(file => [file.idText, file]))
  const revisionRequests: string[] = []
  const blockRequests: string[] = []
  const releases: string[] = []
  const revisions: V2RevisionReader = {
    open: async (id, signal) => {
      signal?.throwIfAborted()
      const idText = encodeBase64Url(id)
      revisionRequests.push(idText)
      events.push(`revision:${idText}`)
      await options.beforeOpen?.(idText)
      if (options.failRevisionFor === idText) {
        throw new V2FileRevisionChangedError('fixture revision failure')
      }
      const file = byId.get(idText)
      if (file === undefined) throw new Error('fixture opened an unknown file')
      return openedRevision(file, async () => { releases.push(idText) })
    },
  }
  const broker: V2BlockRangeReader = {
    readRange: async function* (descriptor, _leaseId, range, request) {
      request?.signal?.throwIfAborted()
      blockRequests.push(descriptor.fileIdText)
      events.push(`block:${descriptor.fileIdText}`)
      await options.beforeRead?.(descriptor.fileIdText)
      if (options.failBlockFor === descriptor.fileIdText) throw new Error('fixture block failure')
      const length = Number(range.end - range.start)
      yield Object.freeze({ offset: range.start, data: new Uint8Array(length).fill(7) })
    },
  }
  return { revisions, broker, revisionRequests, blockRequests, releases }
}

function openedRevision(
  file: Extract<V2CatalogEntry, { kind: 'file' }>,
  release: () => Promise<void>,
): V2OpenedRevision {
  const revision = identity((file.id[0] ?? 1) + 40)
  return Object.freeze({
    descriptor: Object.freeze({
      shareInstance: identity(1),
      shareInstanceId: identityText(1),
      fileId: file.id.slice(),
      fileIdText: file.idText,
      fileRevision: revision,
      fileRevisionText: encodeBase64Url(revision),
      exactSize: file.expectedSize,
      geometry: new FileGeometry(file.expectedSize, 2n),
      ...(file.modifiedTime === undefined ? {} : { modifiedTime: file.modifiedTime }),
    }),
    leaseId: identity(13),
    release,
  })
}

export interface TestOutputOptions {
  readonly durability?: 'None' | 'ProcessRestart'
  readonly initialRanges?: readonly ByteRange[]
  readonly failBegin?: boolean
  readonly failWrite?: boolean
  readonly failCommit?: boolean
  readonly retirement?: 'FileIsolated' | 'JobOutputCompromised'
}

export interface TestOutput extends OutputSession {
  readonly events: string[]
  readonly requests: OutputFileRequest[]
  readonly writes: ReadonlyArray<Readonly<{ offset: bigint; bytes: number }>>
  readonly commits: string[]
  readonly retirements: unknown[]
}

export function testOutput(events: string[] = [], options: TestOutputOptions = {}): TestOutput {
  const identity = outputSessionIdentity({ backend: 'test-output', outputSessionId: 'test-session' })
  const durability = options.durability ?? 'None'
  const capabilities = outputCapabilities({
    durability,
    randomWrite: durability !== 'None',
    fileFailureIsolation: true,
    modificationTime: true,
  })
  const requests: OutputFileRequest[] = []
  const writes: Array<Readonly<{ offset: bigint; bytes: number }>> = []
  const commits: string[] = []
  const retirements: unknown[] = []
  return {
    identity,
    capabilities,
    events,
    requests,
    writes,
    commits,
    retirements,
    beginFile: async (request, signal) => {
      requests.push(request)
      events.push('begin-request')
      const revision = snapshotOpenedOutputRevision(await request.openRevision(signal))
      events.push('revision-opened')
      if (options.failBegin === true) throw new Error('fixture begin failure')
      const file = snapshotOutputFile({
        source: revision,
        sourcePath: request.sourcePath,
        artifactPath: request.artifactPath,
        exactSize: revision.exactSize,
        ...(request.parentAdmission === undefined ? {} : { parentAdmission: request.parentAdmission }),
        ...(request.modifiedTime === undefined ? {} : { modifiedTime: request.modifiedTime }),
      })
      const ownership = Object.freeze({
        ...identity,
        canonicalPath: file.artifactPath,
        ownedFileIdentity: `owned:${file.source.fileId}`,
      })
      let durable = new ByteRangeSet(file.exactSize, options.initialRanges ?? [])
      let pending = new ByteRangeSet(file.exactSize, [])
      events.push('transaction-created')
      return Object.freeze({
        revision,
        durableRanges: new VerifiedDurableRanges(
          ownership,
          file.source,
          file.exactSize,
          durable.ranges,
        ),
        transaction: {
          writeRange: async (offset: bigint, data: Uint8Array, writeSignal: AbortSignal) => {
            writeSignal.throwIfAborted()
            events.push('write')
            if (options.failWrite === true) throw new Error('fixture write failure')
            writes.push(Object.freeze({ offset, bytes: data.byteLength }))
            if (durability !== 'None') {
              pending = pending.union(new ByteRangeSet(
                file.exactSize,
                [byteRange(offset, offset + BigInt(data.byteLength))],
              ))
            }
          },
          checkpoint: async () => {
            if (durability !== 'None') {
              durable = durable.union(pending)
              pending = new ByteRangeSet(file.exactSize, [])
            }
            return new VerifiedDurableRanges(
              ownership,
              file.source,
              file.exactSize,
              durable.ranges,
            )
          },
          commit: async () => {
            events.push('commit')
            if (options.failCommit === true) throw new Error('fixture commit failure')
            commits.push(file.source.fileId)
          },
          retire: async (reason: unknown) => {
            retirements.push(reason)
            return options.retirement ?? 'FileIsolated'
          },
          pause: async () => undefined,
        },
      })
    },
  }
}

export interface TestPlanAuthority extends V2PlanExecutionAuthority {
  readonly routes: TestPlanKind[]
  readonly preparations: ExactPreparationEvidence[]
  readonly singleFileAdmissions: ExactSingleFileEvidence[]
  readonly settlements: string[]
  readonly pauses: string[]
  readonly admissionFailures: unknown[]
  readonly unknownSettlements: string[]
  readonly pauseSignals: AbortSignal[]
  readonly output: TestOutput
}

export function planAuthorityFixture(input: {
  readonly output?: TestOutput
  readonly rejectPreparation?: boolean
  readonly failSettlement?: boolean
  readonly failPause?: boolean
  readonly failUnknownSettlement?: boolean
  readonly hangPause?: boolean
  readonly failDirectoryFinalizePath?: string
  readonly invalidPauseLifecycle?: boolean
  readonly onWorkspaceOriginalAdmission?: (evidence: ExactSingleFileEvidence) => void
} = {}): TestPlanAuthority {
  const output = input.output ?? testOutput()
  const routes: TestPlanKind[] = []
  const preparations: ExactPreparationEvidence[] = []
  const singleFileAdmissions: ExactSingleFileEvidence[] = []
  const settlements: string[] = []
  const pauses: string[] = []
  const admissionFailures: unknown[] = []
  const unknownSettlements: string[] = []
  const pauseSignals: AbortSignal[] = []

  const pause = async (intent: ReceiveIntent): Promise<ReceiveLifecycleState> => {
    pauses.push(intent.plan.kind)
    if (input.hangPause === true) return new Promise<never>(() => undefined)
    if (input.failPause === true) throw new Error('fixture pause failure')
    if (input.invalidPauseLifecycle === true) return downloadStartedState(intent)
    return pauseState(intent)
  }
  const workspaceExecution = (intent: ReceiveIntent): WorkspaceExecution => ({
    planKind: 'workspace-then-publish',
    output,
    settle: async ({ worker }) => {
      settlements.push(`workspace-then-publish:${worker.status}`)
      if (input.failSettlement === true) throw new Error('fixture settlement failure')
      return materializationSealedState(intent)
    },
    pause: async (_request, signal) => {
      pauseSignals.push(signal)
      return pause(intent)
    },
  })
  const authority: V2PlanExecutionAuthority = {
    openDirectTree: async (intent) => {
      routes.push('direct-tree')
      const directories = await directoryOutput(intent, input.failDirectoryFinalizePath)
      const execution: DirectTreeExecution = {
        planKind: 'direct-tree',
        output,
        directories,
        settle: async ({ worker }) => {
          settlements.push(`direct-tree:${worker.status}`)
          if (input.failSettlement === true) throw new Error('fixture settlement failure')
          return worker.status === 'Succeeded' ? publishedState(intent) : partialState(intent, worker.failureCount)
        },
        pause: async (_request, signal) => {
          pauseSignals.push(signal)
          return pause(intent)
        },
      }
      return execution
    },
    openDirectAtomic: async (intent) => {
      routes.push('direct-atomic')
      const execution: DirectAtomicExecution = {
        planKind: 'direct-atomic',
        output,
        ...(intent.artifact.kind === 'zip-archive'
          ? { directories: await directoryOutput(intent, input.failDirectoryFinalizePath) }
          : {}),
        settle: async ({ worker }) => {
          settlements.push(`direct-atomic:${worker.status}`)
          if (input.failSettlement === true) throw new Error('fixture settlement failure')
          return publishedState(intent)
        },
        pause: async (_request, signal) => {
          pauseSignals.push(signal)
          return pause(intent)
        },
      }
      return execution
    },
    openWorkspaceOriginal: async (intent, evidence) => {
      routes.push('workspace-then-publish')
      singleFileAdmissions.push(evidence)
      input.onWorkspaceOriginalAdmission?.(evidence)
      if (input.rejectPreparation === true) {
        return Object.freeze({ kind: 'rejected', state: discardedState(intent) })
      }
      return Object.freeze({ kind: 'accepted', execution: workspaceExecution(intent) })
    },
    prepareWorkspaceZip: async (intent, evidence) => {
      routes.push('workspace-then-publish')
      preparations.push(evidence)
      if (input.rejectPreparation === true) {
        return Object.freeze({ kind: 'rejected', state: discardedState(intent) })
      }
      const execution = workspaceExecution(intent)
      return Object.freeze({ kind: 'accepted', execution })
    },
    preparePortable: async (intent, evidence) => {
      routes.push('portable-handoff')
      preparations.push(evidence)
      if (input.rejectPreparation === true) {
        return Object.freeze({ kind: 'rejected', state: discardedState(intent) })
      }
      const execution: PortableExecution = {
        planKind: 'portable-handoff',
        output,
        settle: async ({ worker }) => {
          settlements.push(`portable-handoff:${worker.status}`)
          if (input.failSettlement === true) throw new Error('fixture settlement failure')
          return downloadStartedState(intent)
        },
        pause: async (_request, signal) => {
          pauseSignals.push(signal)
          return pause(intent)
        },
      }
      return Object.freeze({ kind: 'accepted', execution })
    },
    settleExecutionAdmissionFailure: async (intent, reason) => {
      admissionFailures.push(reason)
      return discardedState(intent)
    },
    recordSettlementUnknown: async (intent) => {
      unknownSettlements.push(intent.plan.kind)
      if (input.failUnknownSettlement === true) {
        throw new Error('fixture unknown-settlement failure')
      }
      return needsAttentionState(intent)
    },
  }
  return Object.assign(authority, {
    routes,
    preparations,
    singleFileAdmissions,
    settlements,
    pauses,
    admissionFailures,
    unknownSettlements,
    pauseSignals,
    output,
  })
}

async function directoryOutput(
  intent: ReceiveIntent,
  failFinalizePath?: string,
): Promise<IncrementalDirectoryOutput> {
  const scope = await createDirectoryAdmissionScope(intent)
  const ledger = new DirectoryAdmissionLedger(scope)
  const port: IncrementalDirectoryOutput = {
    admitDirectory: (request, signal) =>
      ledger.admitDirectory(request.source, signal),
    finalizeDirectory: async (admission, signal) => {
      const settled = await ledger.finalizeDirectory(admission, signal)
      return admission.path.join('/') === failFinalizePath
        ? isolatedDirectorySettlement(
            admission,
            outputFault(FaultScope.DirectoryLocal, OutputFaultCode.DirectoryMetadata),
          )
        : settled
    },
  }
  return Object.freeze(port)
}

export function transferJobFixture(input: {
  readonly catalog: V2CatalogClient
  readonly selection: V2FrozenSelectionPolicy | V2SelectionPolicy
  readonly intent: ReceiveIntent
  readonly plans: V2PlanExecutionAuthority
  readonly revisions: V2RevisionReader
  readonly broker: V2BlockRangeReader
  readonly onTrace?: ConstructorParameters<typeof TransferJob>[0]['onTrace']
  readonly maximumConcurrentFiles?: number
  readonly outputSettlementTimeoutMilliseconds?: number
}): TransferJob {
  return new TransferJob({
    descriptor: {
      shareInstance: identity(1),
      syntheticRoot: identity(2),
      syntheticRootId: identityText(2),
      chunkSize: 2,
    } as never,
    catalog: input.catalog,
    selection: input.selection,
    revisions: input.revisions,
    broker: input.broker,
    lanes: { size: 1 },
    plans: input.plans,
    intent: input.intent,
    ...(input.onTrace === undefined ? {} : { onTrace: input.onTrace }),
    ...(input.maximumConcurrentFiles === undefined
      ? {}
      : { maximumConcurrentFiles: input.maximumConcurrentFiles }),
    ...(input.outputSettlementTimeoutMilliseconds === undefined
      ? {}
      : { outputSettlementTimeoutMilliseconds: input.outputSettlementTimeoutMilliseconds }),
  })
}

function publishedState(intent: ReceiveIntent): ReceiveLifecycleState {
  return Object.freeze({
    ...stateIdentity(intent),
    kind: 'published',
    receiptDigest: digestIdentity(71),
    cleanupState: 'clean',
  })
}

function partialState(intent: ReceiveIntent, failures: number): ReceiveLifecycleState {
  return Object.freeze({
    ...stateIdentity(intent),
    kind: 'partial-directory',
    reason: 'failures',
    successCount: 0n,
    failureCount: BigInt(failures),
    receiptDigest: digestIdentity(72),
  })
}

function restartState(
  intent: ReceiveIntent,
  reason: Extract<ReceiveLifecycleState, { kind: 'restart-required' }>['reason'],
): ReceiveLifecycleState {
  return Object.freeze({
    ...stateIdentity(intent),
    kind: 'restart-required',
    reason,
    receiptDigest: digestIdentity(73),
  })
}

function materializationSealedState(intent: ReceiveIntent): ReceiveLifecycleState {
  return Object.freeze({
    ...stateIdentity(intent),
    kind: 'materialization-sealed',
    sealedMaterializationDigest: digestIdentity(74),
  })
}

function discardedState(intent: ReceiveIntent): ReceiveLifecycleState {
  return Object.freeze({
    ...stateIdentity(intent),
    kind: 'discarded',
    cleanupReceiptDigest: digestIdentity(78),
  })
}

function downloadStartedState(intent: ReceiveIntent): ReceiveLifecycleState {
  return Object.freeze({
    ...stateIdentity(intent),
    kind: 'download-started',
    attemptKind: 'portable',
    attemptId: identityText(75),
  })
}

function needsAttentionState(intent: ReceiveIntent): Extract<ReceiveLifecycleState, { kind: 'needs-attention' }> {
  return Object.freeze({
    ...stateIdentity(intent),
    kind: 'needs-attention',
    reason: 'target-ownership-unknown',
    lastVerifiedRecordDigest: digestIdentity(76),
  })
}

function pauseState(intent: ReceiveIntent): ReceiveLifecycleState {
  switch (intent.plan.kind) {
    case 'direct-tree': return partialState(intent, 1)
    case 'workspace-then-publish':
      return Object.freeze({
        ...stateIdentity(intent),
        kind: 'resumable-receive',
        checkpointSetDigest: digestIdentity(77),
        completedFileCount: 0n,
        completedBytes: 0n,
        expiresAt: 1000,
      })
    case 'direct-atomic': return restartState(intent, 'direct-atomic-rolled-back')
    case 'portable-handoff': return restartState(intent, 'portable-aborted')
  }
}

function stateIdentity(intent: ReceiveIntent) {
  return Object.freeze({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    generation: 2n,
  })
}

function compareUtf8(left: string, right: string): number {
  const encoder = new TextEncoder()
  const leftBytes = encoder.encode(left)
  const rightBytes = encoder.encode(right)
  const shared = Math.min(leftBytes.byteLength, rightBytes.byteLength)
  for (let index = 0; index < shared; index += 1) {
    const difference = leftBytes[index]! - rightBytes[index]!
    if (difference !== 0) return difference
  }
  return leftBytes.byteLength - rightBytes.byteLength
}
