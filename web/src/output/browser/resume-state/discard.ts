import type {
  PersistedOutputRecord,
} from '../../persistence/journal'
import {
  pausedTaskDescriptorKey,
  pausedTaskDescriptorNamespace,
  type PausedTaskDescriptorV1,
} from '../../resume/descriptor'
import {
  observePausedTask,
  ResumeStateDiscardKind,
  ResumeStateBusyError,
  type ResumeStateDiscardResult,
  type ResumeStateOperationRequest,
} from '../../resume/authority'
import {
  FILE_SYSTEM_ACCESS_BACKEND,
  ORIGIN_PRIVATE_BACKEND,
} from '../../capability/contract'
import {
  openOriginPrivateStagingNamespace,
  removeOriginPrivateStagingNamespace,
  type OriginPrivateStagingNamespace,
} from '../../origin-private/session'
import { OriginPrivateZipExporter } from '../../origin-private/zip-exporter'
import { PersistentJournalView } from '../../persistent-tree/journal-view'
import { BrowserFileSystemTree } from '../filesystem-tree'
import { IndexedDbOutputRepository } from '../indexeddb-repository'
import {
  acquireBrowserFileSystemAccessSessionLease,
  acquireBrowserOutputSessionLease,
  BrowserOutputSessionBusyError,
  type BrowserFileSystemAccessSessionLease,
  type BrowserOutputSessionLease,
} from '../session-lease'
import {
  abortAcquiredOutput,
  PausedTaskCapabilityError,
  type BrowserPausedTaskDependencies,
} from './capability'
import {
  discardMarkerMatchesPin,
  discardMarkerPhaseMatchesTask,
  resumeStateInventoryDigest,
  sameOptionalDiscardMarker,
  sameRecordInventory,
  scanAllRecords,
  type BrowserResumeStatePin,
} from './inventory'
import {
  completedFiles,
  discardFileSystemAccessObjects,
  preflightResumeState,
  readCurrentResumeState,
  sameDirectoryEntry,
  verifyPreparedRootIdentity,
  type IndexedDbResumeStateDiscardMarker,
  type StoredRootCapability,
} from './records'

export async function discardPinnedResumeState(
  databaseName: string,
  pin: BrowserResumeStatePin,
  request: ResumeStateOperationRequest,
  dependencies: BrowserPausedTaskDependencies,
): Promise<ResumeStateDiscardResult> {
  const descriptor = pin.descriptor
  observePausedTask(
    dependencies.onTrace,
    'paused-task-discard-started',
    descriptor,
    { decision: 'current-share-confirmed' },
  )
  if (descriptor.intent.output.backend === FILE_SYSTEM_ACCESS_BACKEND) {
    return discardFileSystemAccessResumeState(
      databaseName,
      pin,
      request,
      dependencies,
    )
  }
  if (descriptor.intent.output.backend === ORIGIN_PRIVATE_BACKEND) {
    return discardOriginPrivateResumeState(
      databaseName,
      pin,
      request,
      dependencies,
    )
  }
  throw new PausedTaskCapabilityError(
    'unsupported',
    'Paused task backend cannot be discarded by this browser',
  )
}

async function discardFileSystemAccessResumeState(
  databaseName: string,
  pin: BrowserResumeStatePin,
  request: ResumeStateOperationRequest,
  dependencies: BrowserPausedTaskDependencies,
): Promise<ResumeStateDiscardResult> {
  // Permission acquisition must remain in the direct user-action stack.
  const permissionOperation = dependencies.permission.requestWritePermission(pin.capability.handle)
  let permission: PermissionState
  try {
    permission = await permissionOperation
  } catch (error) {
    throw new PausedTaskCapabilityError(
      'permission-denied',
      'File System Access permission renewal failed',
      { cause: error },
    )
  }
  if (permission !== 'granted') {
    throw new PausedTaskCapabilityError(
      'permission-denied',
      'File System Access permission was not granted',
    )
  }
  return discardCertifiedResumeState({
    databaseName,
    pin,
    request,
    dependencies,
    certifiedRoot: pin.capability.handle,
  })
}

async function discardOriginPrivateResumeState(
  databaseName: string,
  pin: BrowserResumeStatePin,
  request: ResumeStateOperationRequest,
  dependencies: BrowserPausedTaskDependencies,
): Promise<ResumeStateDiscardResult> {
  // Both capabilities begin before the first await to preserve user activation.
  const outputOperation = beginOriginPrivateDiscardOutput(pin, request)
  let rootOperation: Promise<FileSystemDirectoryHandle>
  try {
    rootOperation = dependencies.originPrivateStorage.getDirectory()
  } catch (error) {
    if (outputOperation !== undefined) await abortAcquiredOutput(outputOperation, error)
    throw error
  }

  let root: FileSystemDirectoryHandle
  let output: WritableStream<Uint8Array> | undefined
  try {
    ;[root, output] = await Promise.all([
      rootOperation,
      outputOperation ?? Promise.resolve(undefined),
    ])
  } catch (error) {
    if (outputOperation !== undefined) await abortAcquiredOutput(outputOperation, error)
    throw error
  }
  if (!await sameDirectoryEntry(root, pin.capability.handle)) {
    const error = new PausedTaskCapabilityError(
      'stale',
      'The origin-private root capability no longer matches the paused task',
    )
    if (output !== undefined) await output.abort(error).catch(() => undefined)
    throw error
  }

  const exporter = output === undefined ? undefined : new OriginPrivateZipExporter(output)
  try {
    return await discardCertifiedResumeState({
      databaseName,
      pin,
      request,
      dependencies,
      certifiedRoot: root,
      ...(exporter === undefined ? {} : { exporter }),
    })
  } catch (error) {
    await exporter?.abort(error).catch(() => undefined)
    throw error
  }
}

function beginOriginPrivateDiscardOutput(
  pin: BrowserResumeStatePin,
  request: ResumeStateOperationRequest,
): Promise<WritableStream<Uint8Array>> | undefined {
  if (completedFiles(pin.committed).length === 0 || pin.discardMarker !== undefined) {
    return undefined
  }
  if (request.acquireOriginPrivateOutput === undefined) {
    throw new PausedTaskCapabilityError(
      'unsupported',
      'Discarding completed OPFS members requires a partial ZIP destination',
    )
  }
  return request.acquireOriginPrivateOutput()
}

interface CertifiedResumeStateDiscard {
  readonly databaseName: string
  readonly pin: BrowserResumeStatePin
  readonly request: ResumeStateOperationRequest
  readonly dependencies: BrowserPausedTaskDependencies
  readonly certifiedRoot: FileSystemDirectoryHandle
  readonly exporter?: OriginPrivateZipExporter
}

async function discardCertifiedResumeState(
  options: CertifiedResumeStateDiscard,
): Promise<ResumeStateDiscardResult> {
  const { pin, dependencies, databaseName } = options
  const descriptor = pin.descriptor
  const current = await readCurrentResumeState(databaseName, descriptor)
  if (current === undefined) {
    await abandonDiscardExport(options.exporter, new Error('Paused task was already removed'))
    return Object.freeze({ kind: ResumeStateDiscardKind.AlreadyAbsent })
  }
  if (!await sameDirectoryEntry(current.handle, pin.capability.handle)) {
    throw new PausedTaskCapabilityError(
      'stale',
      'The paused task root capability was replaced after inventory',
    )
  }
  await verifyPreparedRootIdentity(
    databaseName,
    descriptor.intent.output.backend,
    descriptor.intent.output.target,
    options.certifiedRoot,
  )

  const binding = pausedTaskDescriptorNamespace(descriptor)
  const repository = await IndexedDbOutputRepository.openExisting(databaseName, binding)
  let lease: BrowserResumeStateDiscardLease | undefined
  try {
    lease = await acquireResumeStateDiscardLease(repository)
    const validation = await validatePinnedResumeState(
      options,
      current,
      repository,
    )
    if (validation.kind === 'settled') return validation.result

    if (validation.value.discardMarker?.phase === 'exporting') {
      await abandonDiscardExport(
        options.exporter,
        new Error('A prior partial ZIP export has an ambiguous result'),
      )
      return discardNeedsAttention(dependencies, descriptor, 'export-failed')
    }

    const objects = await prepareResumeStateDiscardObjects(
      options,
      repository,
      lease,
      validation.value.committed,
      validation.value.candidates,
      validation.value.discardMarker,
    )
    if (objects.kind === 'settled') return objects.result
    options.request.signal?.throwIfAborted()

    const marker = validation.value.discardMarker ??
      await beginCertifiedResumeStateDiscard(
        repository,
        descriptor,
        validation.value.inventoryDigest,
        validation.value.committed,
      )
    const exported = await exportCompletedOriginPrivateMembers(
      options,
      binding,
      repository,
      objects.value.tree,
      validation.value.committed,
      marker,
    )
    if (exported.kind === 'settled') return exported.result

    const discarded = await commitCertifiedResumeStateDiscard(
      options,
      repository,
      objects.value,
      validation.value.committed,
      validation.value.candidates,
      marker,
    )
    if (!discarded) {
      return discardNeedsAttention(dependencies, descriptor, 'discard-incomplete')
    }

    const complete = completedFiles(validation.value.committed)
    observePausedTask(
      dependencies.onTrace,
      'paused-task-discard-completed',
      descriptor,
      {
        decision: exported.value.exportedPartialZip
          ? 'completed-members-exported-before-discard'
          : 'completed-files-preserved-before-discard',
      },
    )
    return Object.freeze({
      kind: ResumeStateDiscardKind.Discarded,
      preservedCompletedFiles: complete.length,
      exportedPartialZip: exported.value.exportedPartialZip,
    })
  } finally {
    repository.close()
    await lease?.release().catch(() => undefined)
  }
}

type ResumeStateDiscardStep<T> =
  | Readonly<{ kind: 'ready'; value: T }>
  | Readonly<{ kind: 'settled'; result: ResumeStateDiscardResult }>

interface ValidatedResumeState {
  readonly committed: readonly PersistedOutputRecord[]
  readonly candidates: readonly PersistedOutputRecord[]
  readonly inventoryDigest: string
  readonly discardMarker?: IndexedDbResumeStateDiscardMarker
}

interface PreparedResumeStateObjects {
  readonly tree: BrowserFileSystemTree
  readonly namespace?: OriginPrivateStagingNamespace
  readonly originPrivateStagingAlreadyAbsent?: true
}

async function acquireResumeStateDiscardLease(
  repository: IndexedDbOutputRepository,
): Promise<BrowserResumeStateDiscardLease> {
  try {
    return repository.binding.backend === FILE_SYSTEM_ACCESS_BACKEND
      ? await acquireBrowserFileSystemAccessSessionLease(repository.binding)
      : await acquireBrowserOutputSessionLease(repository.binding)
  } catch (error) {
    if (error instanceof BrowserOutputSessionBusyError) throw new ResumeStateBusyError()
    throw error
  }
}

async function validatePinnedResumeState(
  options: CertifiedResumeStateDiscard,
  beforeLease: StoredRootCapability,
  repository: IndexedDbOutputRepository,
): Promise<ResumeStateDiscardStep<ValidatedResumeState>> {
  const { descriptor } = options.pin
  const current = await readCurrentResumeState(options.databaseName, descriptor)
  if (current === undefined) {
    await abandonDiscardExport(options.exporter, new Error('Paused task was already removed'))
    return settledDiscard({ kind: ResumeStateDiscardKind.AlreadyAbsent })
  }
  if (!await sameDirectoryEntry(current.handle, beforeLease.handle)) {
    await abandonDiscardExport(options.exporter, new Error('Paused task changed after inventory'))
    return settledDiscard(discardNeedsAttention(
      options.dependencies,
      descriptor,
      'checkpoint-changed',
    ))
  }

  const [committed, candidates, discardMarker] = await Promise.all([
    scanAllRecords((scan) => repository.scanCommitted(scan)),
    scanAllRecords((scan) => repository.scanCandidates(scan)),
    repository.readResumeStateDiscard(),
  ])
  const inventoryDigest = await resumeStateInventoryDigest(committed, candidates)
  if (!sameRecordInventory(committed, options.pin.committed) ||
      !sameRecordInventory(candidates, options.pin.candidates) ||
      inventoryDigest !== options.pin.inventoryDigest ||
      !sameOptionalDiscardMarker(discardMarker, options.pin.discardMarker) ||
      (discardMarker !== undefined && (!discardMarkerMatchesPin(
        discardMarker,
        descriptor,
        inventoryDigest,
      ) || !discardMarkerPhaseMatchesTask(discardMarker, descriptor, committed)))) {
    await abandonDiscardExport(
      options.exporter,
      new Error('Checkpoint journal changed after inventory'),
    )
    return settledDiscard(discardNeedsAttention(
      options.dependencies,
      descriptor,
      'checkpoint-changed',
    ))
  }
  return readyDiscard(Object.freeze({
    committed,
    candidates,
    inventoryDigest,
    ...(discardMarker === undefined ? {} : { discardMarker }),
  }))
}

async function prepareResumeStateDiscardObjects(
  options: CertifiedResumeStateDiscard,
  repository: IndexedDbOutputRepository,
  lease: BrowserResumeStateDiscardLease,
  committed: readonly PersistedOutputRecord[],
  candidates: readonly PersistedOutputRecord[],
  discardMarker: IndexedDbResumeStateDiscardMarker | undefined,
): Promise<ResumeStateDiscardStep<PreparedResumeStateObjects>> {
  const descriptor = options.pin.descriptor
  let namespace: OriginPrivateStagingNamespace | undefined
  try {
    namespace = await openOriginPrivateDiscardNamespace(options)
  } catch (error) {
    if (!errorNamed(error, 'NotFoundError')) throw error
    if (descriptor.intent.output.backend === ORIGIN_PRIVATE_BACKEND &&
        discardMarker !== undefined && discardMarker.phase !== 'exporting') {
      return readyDiscard(Object.freeze({
        tree: resumeStateDiscardTree(
          descriptor,
          options.certifiedRoot,
          repository,
          lease,
        ),
        originPrivateStagingAlreadyAbsent: true,
      }))
    }
    await abandonDiscardExport(options.exporter, error)
    return settledDiscard(discardNeedsAttention(
      options.dependencies,
      descriptor,
      'output-changed',
    ))
  }
  const root = namespace?.root ?? options.certifiedRoot
  const tree = resumeStateDiscardTree(descriptor, root, repository, lease)
  if (!await preflightResumeState(
    tree,
    committed,
    candidates,
    discardMarker,
  )) {
    await abandonDiscardExport(
      options.exporter,
      new Error('Paused output changed before discard'),
    )
    return settledDiscard(discardNeedsAttention(
      options.dependencies,
      descriptor,
      'output-changed',
    ))
  }
  return readyDiscard(Object.freeze({
    tree,
    ...(namespace === undefined ? {} : { namespace }),
  }))
}

type BrowserResumeStateDiscardLease =
  | BrowserOutputSessionLease
  | BrowserFileSystemAccessSessionLease

function resumeStateDiscardTree(
  descriptor: PausedTaskDescriptorV1,
  root: FileSystemDirectoryHandle,
  repository: IndexedDbOutputRepository,
  lease: BrowserResumeStateDiscardLease,
): BrowserFileSystemTree {
  if (descriptor.intent.output.backend === FILE_SYSTEM_ACCESS_BACKEND) {
    if (!('mutations' in lease)) {
      throw new TypeError('FSA discard requires the shared-root mutation authority')
    }
    return BrowserFileSystemTree.forSharedRoot({
      root,
      handles: repository,
      mutations: lease.mutations,
    })
  }
  return BrowserFileSystemTree.forIsolatedNamespace({ root, handles: repository })
}

function openOriginPrivateDiscardNamespace(
  options: CertifiedResumeStateDiscard,
): Promise<OriginPrivateStagingNamespace | undefined> {
  const descriptor = options.pin.descriptor
  return descriptor.intent.output.backend === ORIGIN_PRIVATE_BACKEND
    ? openOriginPrivateStagingNamespace(options.certifiedRoot, descriptor.intent.digest)
    : Promise.resolve(undefined)
}

function beginCertifiedResumeStateDiscard(
  repository: IndexedDbOutputRepository,
  descriptor: PausedTaskDescriptorV1,
  inventoryDigest: string,
  committed: readonly PersistedOutputRecord[],
): Promise<IndexedDbResumeStateDiscardMarker> {
  const requiresExport = descriptor.intent.output.backend === ORIGIN_PRIVATE_BACKEND &&
    completedFiles(committed).length > 0
  return repository.beginResumeStateDiscard({
    descriptorKey: pausedTaskDescriptorKey(descriptor),
    rootCapabilityRef: descriptor.rootCapabilityRef,
    backend: descriptor.intent.output.backend,
    inventoryDigest,
    phase: requiresExport ? 'exporting' : 'retiring',
  })
}

async function exportCompletedOriginPrivateMembers(
  options: CertifiedResumeStateDiscard,
  binding: ReturnType<typeof pausedTaskDescriptorNamespace>,
  repository: IndexedDbOutputRepository,
  tree: BrowserFileSystemTree,
  committed: readonly PersistedOutputRecord[],
  marker: IndexedDbResumeStateDiscardMarker,
): Promise<ResumeStateDiscardStep<{ readonly exportedPartialZip: boolean }>> {
  const descriptor = options.pin.descriptor
  if (descriptor.intent.output.backend !== ORIGIN_PRIVATE_BACKEND ||
      completedFiles(committed).length === 0) {
    return readyDiscard(Object.freeze({ exportedPartialZip: false }))
  }
  if (marker.phase === 'exported') {
    return readyDiscard(Object.freeze({ exportedPartialZip: true }))
  }
  if (marker.phase !== 'exporting' || options.exporter === undefined) {
    return settledDiscard(discardNeedsAttention(
      options.dependencies,
      descriptor,
      'export-failed',
    ))
  }
  try {
    await options.exporter.exportPartial(
      new PersistentJournalView(binding, repository, tree).catalog(),
      options.request.signal ?? new AbortController().signal,
    )
    await repository.advanceResumeStateDiscard(marker, 'exported')
    return readyDiscard(Object.freeze({ exportedPartialZip: true }))
  } catch {
    return settledDiscard(discardNeedsAttention(
      options.dependencies,
      descriptor,
      'export-failed',
    ))
  }
}

async function commitCertifiedResumeStateDiscard(
  options: CertifiedResumeStateDiscard,
  repository: IndexedDbOutputRepository,
  objects: PreparedResumeStateObjects,
  committed: readonly PersistedOutputRecord[],
  candidates: readonly PersistedOutputRecord[],
  marker: IndexedDbResumeStateDiscardMarker,
): Promise<boolean> {
  try {
    if (options.pin.descriptor.intent.output.backend === ORIGIN_PRIVATE_BACKEND) {
      if (objects.namespace !== undefined) {
        await removeOriginPrivateStagingNamespace(objects.namespace)
      } else if (objects.originPrivateStagingAlreadyAbsent !== true) {
        throw new Error('OPFS discard lost its certified staging namespace')
      }
    } else {
      await discardFileSystemAccessObjects(objects.tree, committed, candidates)
      // Completed files and their ancestors are outside discard authority. A
      // second proof closes the user-mutation window around incomplete removal.
      if (!await preflightResumeState(objects.tree, committed, candidates, marker)) {
        throw new Error('FSA output changed while resumable state was being discarded')
      }
    }
    await repository.commitResumeStateDiscard(
      pausedTaskDescriptorKey(options.pin.descriptor),
      options.pin.descriptor.rootCapabilityRef,
    )
    return true
  } catch {
    return false
  }
}

function readyDiscard<T>(value: T): ResumeStateDiscardStep<T> {
  return Object.freeze({ kind: 'ready', value })
}

function settledDiscard(result: ResumeStateDiscardResult): ResumeStateDiscardStep<never> {
  return Object.freeze({ kind: 'settled', result })
}

async function abandonDiscardExport(
  exporter: OriginPrivateZipExporter | undefined,
  reason: unknown,
): Promise<void> {
  await exporter?.abort(reason).catch(() => undefined)
}

function discardNeedsAttention(
  dependencies: BrowserPausedTaskDependencies,
  descriptor: PausedTaskDescriptorV1,
  reason: Extract<ResumeStateDiscardResult, { kind: 'NeedsAttention' }>['reason'],
): ResumeStateDiscardResult {
  observePausedTask(
    dependencies.onTrace,
    'paused-task-discard-needs-attention',
    descriptor,
    { decision: reason },
  )
  return Object.freeze({ kind: ResumeStateDiscardKind.NeedsAttention, reason })
}

function errorNamed(error: unknown, name: string): boolean {
  return typeof error === 'object' && error !== null &&
    'name' in error && (error as { readonly name?: unknown }).name === name
}
