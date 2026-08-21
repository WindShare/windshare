import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createCompleteDirectoryResultRoot,
  createDirectTreePlan,
  createReceiveIntent,
  createResultRootDirectoryTreeArtifact,
  createSelectionSpec,
  type DirectoryTreeArtifact,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import { fsaParentOffer } from '../../src/output/capability/acquisition'
import type { AcquiredFSAParentAuthority } from '../../src/output/capability/contract'
import { IndexedDbReceiveOperationRepository } from '../../src/output/browser/indexeddb-repository'
import {
  prepareFSAOperationBindingTransition,
  verifyFSAOperationBinding,
} from '../../src/output/browser/indexeddb-root-binding'
import {
  acquireFSARootMutationLease,
  fsaRootMutationLockName,
  type FSARootMutationLease,
} from '../../src/output/browser/namespace-mutation'
import type { FileCheckpointV2 } from '../../src/output/persistence/checkpoint'
import {
  assembleNewFileSystemAccessOutput,
  reserveNewFileSystemAccessOutput,
  type FileSystemAccessOutputSession,
} from '../../src/output/file-system-access/session'
import {
  openFSAFileCheckpointRepository,
  scanAllFSAFileCheckpoints,
  type FSAFileCheckpointRepository,
} from '../../src/output/file-system-access/checkpoint-repository'
import type { PersistentFileTransactionPort } from '../../src/output/persistent-tree/contracts'

const PROBE_ARTIFACT_ROOT = 'windshare-fsa-path-proof'
const PROBE_PATH = Object.freeze(['.git', 'hooks', 'applypatch-msg.sample'])
const PROBE_BYTES = Uint8Array.of(0x57, 0x53)
const PICKER_OPTIONS = Object.freeze({
  id: 'windshare-windows-fsa-path-proof',
  mode: 'readwrite' as const,
})

interface NativeEvent {
  readonly sequence: number
  readonly stage: string
  readonly api: string
  readonly transition: 'start' | 'return' | 'reject'
  readonly receiver: HandleDescription
  readonly arguments?: unknown
  readonly returned?: unknown
  readonly error?: ErrorEvidence
}

interface HandleDescription {
  readonly kind: string | null
  readonly name: string | null
  readonly writerId?: string
}

interface ErrorEvidence {
  readonly constructor: string | null
  readonly javascriptKind: 'type-error' | 'dom-exception' | 'unknown'
  readonly domExceptionName: string | null
  readonly message: string
  readonly stack: string | null
  readonly thrownType: string
  readonly thrownValue: string
}

interface Instrumentation {
  readonly events: NativeEvent[]
  readonly writerFacts: {
    obtained: number
    writeStarted: number
    writeReturned: number
    writeRejected: number
    closeStarted: number
    closeReturned: number
    closeRejected: number
  }
  withStage<T>(stage: string, operation: () => Promise<T>): Promise<T>
  record(
    api: string,
    transition: NativeEvent['transition'],
    receiver: HandleDescription,
    details?: Pick<NativeEvent, 'arguments' | 'returned' | 'error'>,
  ): void
  restore(): void
}

interface ProbeRuntime {
  readonly selectedParent: FileSystemDirectoryHandle
  readonly intent: ReceiveIntent
  readonly repository: IndexedDbReceiveOperationRepository
  readonly checkpoints: FSAFileCheckpointRepository
  readonly rootLease: FSARootMutationLease
  readonly session: FileSystemAccessOutputSession
  readonly resultName: string
  readonly lockName: string
}

interface OptionalDirectoryObservation {
  readonly handle?: FileSystemDirectoryHandle
  readonly observation: unknown
}

interface OptionalFileObservation {
  readonly handle?: FileSystemFileHandle
  readonly size?: number
  readonly observation: unknown
}

export async function runWindowsFSAPathProbe(): Promise<unknown> {
  const probeId = crypto.randomUUID()
  const databaseName = `windshare-windows-fsa-path-proof-${probeId}`
  const instrumentation = installNativeInstrumentation()
  const snapshots: unknown[] = []
  let runtime: ProbeRuntime | undefined
  let transaction: PersistentFileTransactionPort | undefined
  let rejection: Readonly<{ phase: string; error: ErrorEvidence }> | null = null
  let probeFailure: ErrorEvidence | null = null
  let cleanupFailure: ErrorEvidence | null = null

  try {
    instrumentation.record('showDirectoryPicker', 'start', { kind: 'window', name: null }, {
      arguments: PICKER_OPTIONS,
    })
    const picker = (globalThis as typeof globalThis & {
      showDirectoryPicker?: (
        options: typeof PICKER_OPTIONS,
      ) => Promise<FileSystemDirectoryHandle>
    }).showDirectoryPicker
    if (picker === undefined) {
      throw new DOMException('showDirectoryPicker is unavailable', 'NotSupportedError')
    }
    const selectedParent = await picker(PICKER_OPTIONS).then((handle) => {
      instrumentation.record('showDirectoryPicker', 'return', describeHandle(handle), {
        returned: describeHandle(handle),
      })
      return handle
    }, (error: unknown) => {
      instrumentation.record('showDirectoryPicker', 'reject', { kind: 'window', name: null }, {
        error: describeError(error),
      })
      throw error
    })
    snapshots.push(await instrumentation.withStage(
      'observe.after-picker',
      () => observeSelectedParent('after-picker', selectedParent),
    ))

    runtime = await instrumentation.withStage(
      'windshare.reserve-and-bind',
      () => bindProbe(databaseName, selectedParent),
    )
    snapshots.push(await instrumentation.withStage(
      'observe.after-binding-before-activation',
      () => observeRuntime('after-binding-before-activation', runtime!),
    ))

    await instrumentation.withStage('windshare.activate-result-root', () => runtime!.session.activate())
    snapshots.push(await instrumentation.withStage(
      'observe.after-result-root',
      () => observeRuntime('after-result-root', runtime!),
    ))

    await instrumentation.withStage(
      'windshare.ensure-directory:.git',
      () => runtime!.session.ensureDirectory(['.git']).then(() => undefined),
    )
    snapshots.push(await instrumentation.withStage(
      'observe.after-.git',
      () => observeRuntime('after-.git', runtime!),
    ))

    await instrumentation.withStage(
      'windshare.ensure-directory:.git/hooks',
      () => runtime!.session.ensureDirectory(['.git', 'hooks']).then(() => undefined),
    )
    snapshots.push(await instrumentation.withStage(
      'observe.after-.git-hooks',
      () => observeRuntime('after-.git-hooks', runtime!),
    ))

    try {
      transaction = await instrumentation.withStage(
        'windshare.begin-file:.git/hooks/applypatch-msg.sample',
        () => runtime!.session.beginFile({
          artifactPath: PROBE_PATH,
          openRevision: async () => ({
            fileId: identity(93),
            fileRevision: identity(94),
            exactSize: BigInt(PROBE_BYTES.byteLength),
          }),
        }),
      )
    } catch (error) {
      rejection = Object.freeze({ phase: 'begin-file', error: describeError(error) })
    }
    snapshots.push(await instrumentation.withStage(
      'observe.after-begin-file',
      () => observeRuntime('after-begin-file', runtime!),
    ))

    if (transaction !== undefined) {
      try {
        await instrumentation.withStage(
          'windshare.first-write:.git/hooks/applypatch-msg.sample',
          () => transaction!.writeRange(0n, PROBE_BYTES),
        )
      } catch (error) {
        rejection = Object.freeze({ phase: 'first-write', error: describeError(error) })
      }
      snapshots.push(await instrumentation.withStage(
        'observe.after-first-write',
        () => observeRuntime('after-first-write', runtime!),
      ))
    }

    snapshots.push(await instrumentation.withStage(
      'observe.after-quiescence',
      async () => {
        await runtime!.rootLease.authority.run('settle-operation', async () => undefined)
        return observeRuntime('after-quiescence', runtime!)
      },
    ))
  } catch (error) {
    probeFailure = describeError(error)
  } finally {
    if (runtime !== undefined) {
      try {
        await instrumentation.withStage('windshare.close-session', () => runtime!.session.close())
      } catch (error) {
        cleanupFailure = describeError(error)
      }
      runtime.checkpoints.close()
      runtime.repository.close()
      try {
        snapshots.push(await instrumentation.withStage(
          'observe.after-durable-reopen',
          () => observeDurableReopen('after-durable-reopen', databaseName, runtime!),
        ))
      } catch (error) {
        cleanupFailure ??= describeError(error)
      }
    }
    instrumentation.restore()
  }

  return Object.freeze({
    schema: 'windshare/windows-fsa-path-proof/v1',
    probeId,
    databaseName,
    artifactPath: PROBE_PATH,
    userAgent: navigator.userAgent,
    platform: (navigator as Navigator & { userAgentData?: { readonly platform?: string } })
      .userAgentData?.platform ?? navigator.platform,
    rejection,
    probeFailure,
    cleanupFailure,
    writer: Object.freeze({ ...instrumentation.writerFacts }),
    snapshots: Object.freeze(snapshots),
    events: Object.freeze(instrumentation.events),
  })
}

async function bindProbe(
  databaseName: string,
  selectedParent: FileSystemDirectoryHandle,
): Promise<ProbeRuntime> {
  const artifact = await probeArtifact()
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  const rootLease = await acquireFSARootMutationLease(selectedParent)
  try {
    const reserved = await reserveNewFileSystemAccessOutput({
      authority: acquiredParent(selectedParent),
      artifact,
      rootLease,
      operationId: identity(90),
      reservationId: identity(91),
      authorityRef: identity(92, 32),
    })
    const selection = await createSelectionSpec({
      shareInstance: identity(1),
      syntheticRoot: identity(2),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    })
    const intent = await createReceiveIntent({
      selection,
      artifact,
      plan: await createDirectTreePlan(artifact, reserved.reservation),
    })
    const prepared = await prepareFSAOperationBindingTransition({
      repository,
      intent,
      parent: selectedParent,
    })
    await repository.commitTransition({ operationId: intent.operationId, ...prepared.transition })
    const binding = await verifyFSAOperationBinding({
      repository,
      intent,
      expectedParent: selectedParent,
    })
    const session = await assembleNewFileSystemAccessOutput({
      binding,
      operationRepository: repository,
      rootLease,
      databaseName,
    })
    const checkpoints = await openFSAFileCheckpointRepository(
      { databaseName },
      intent,
      binding.reservation,
    )
    return Object.freeze({
      selectedParent,
      intent,
      repository,
      checkpoints,
      rootLease,
      session,
      resultName: binding.reservation.physicalName,
      lockName: await fsaRootMutationLockName(selectedParent),
    })
  } catch (error) {
    await rootLease.release().catch(() => undefined)
    repository.close()
    throw error
  }
}

async function observeSelectedParent(
  label: string,
  selectedParent: FileSystemDirectoryHandle,
): Promise<unknown> {
  return Object.freeze({
    label,
    selectedParent: describeHandle(selectedParent),
    permission: await permissions(selectedParent),
    expectedResultRoot: await entry(selectedParent, PROBE_ARTIFACT_ROOT, 'directory'),
  })
}

async function observeRuntime(label: string, runtime: ProbeRuntime): Promise<unknown> {
  const namespace = await observeNamespace(runtime.selectedParent, runtime.resultName)
  const committed = await scanAllFSAFileCheckpoints(runtime.checkpoints, 'committed')
  const candidates = await scanAllFSAFileCheckpoints(runtime.checkpoints, 'candidates')
  const operationHandles = await runtime.repository.listHandles(runtime.intent.operationId)
  const fileHandles = await runtime.checkpoints.listHandles()
  const binding = await verifyFSAOperationBinding({
    repository: runtime.repository,
    intent: runtime.intent,
  })
  const lockState = await navigator.locks.query()
  return Object.freeze({
    label,
    namespace,
    permissions: {
      selectedParent: await permissions(runtime.selectedParent),
      resultRoot: await optionalPermissions(namespace.resultRootHandle),
      hooks: await optionalPermissions(namespace.hooksHandle),
    },
    authority: {
      selectedParentMatchesPersistedParent: await runtime.selectedParent.isSameEntry(binding.parent),
      lockName: runtime.lockName,
      webLockHeld: lockState.held?.some((lock) => lock.name === runtime.lockName) ?? false,
      webLockPending: lockState.pending?.some((lock) => lock.name === runtime.lockName) ?? false,
    },
    operationHandles: await describePersistedHandles(operationHandles, namespace.handlesByName),
    fileHandles: await describePersistedHandles(fileHandles, namespace.handlesByName),
    checkpoints: checkpointFacts(candidates, committed),
  })
}

async function observeDurableReopen(
  label: string,
  databaseName: string,
  runtime: ProbeRuntime,
): Promise<unknown> {
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  const checkpoints = await openFSAFileCheckpointRepository(
    { databaseName },
    runtime.intent,
    runtime.session.reservation,
  )
  try {
    const binding = await verifyFSAOperationBinding({ repository, intent: runtime.intent })
    const namespace = await observeNamespace(binding.parent, runtime.resultName)
    const committed = await scanAllFSAFileCheckpoints(checkpoints, 'committed')
    const candidates = await scanAllFSAFileCheckpoints(checkpoints, 'candidates')
    const lockState = await navigator.locks.query()
    let mutationAuthority: 'closed' | 'unexpectedly-accepting'
    try {
      await runtime.rootLease.authority.run('settle-operation', async () => undefined)
      mutationAuthority = 'unexpectedly-accepting'
    } catch {
      mutationAuthority = 'closed'
    }
    return Object.freeze({
      label,
      namespace,
      permissions: {
        persistedParent: await permissions(binding.parent),
        resultRoot: await optionalPermissions(namespace.resultRootHandle),
        hooks: await optionalPermissions(namespace.hooksHandle),
      },
      authority: {
        selectedParentMatchesPersistedParent: await runtime.selectedParent.isSameEntry(binding.parent),
        webLockHeld: lockState.held?.some((lock) => lock.name === runtime.lockName) ?? false,
        webLockPending: lockState.pending?.some((lock) => lock.name === runtime.lockName) ?? false,
        mutationAuthority,
      },
      operationHandles: await describePersistedHandles(
        await repository.listHandles(runtime.intent.operationId),
        namespace.handlesByName,
      ),
      fileHandles: await describePersistedHandles(
        await checkpoints.listHandles(),
        namespace.handlesByName,
      ),
      checkpoints: checkpointFacts(candidates, committed),
    })
  } finally {
    checkpoints.close()
    repository.close()
  }
}

async function observeNamespace(
  parent: FileSystemDirectoryHandle,
  resultName: string,
): Promise<{
  readonly resultRoot: unknown
  readonly dotGit: unknown
  readonly hooks: unknown
  readonly target: unknown
  readonly targetCommittedBytes: string
  readonly resultRootHandle?: FileSystemDirectoryHandle
  readonly hooksHandle?: FileSystemDirectoryHandle
  readonly handlesByName: ReadonlyMap<string, FileSystemHandle>
}> {
  const handles = new Map<string, FileSystemHandle>([[parent.name, parent]])
  const resultRoot = await optionalDirectory(parent, resultName)
  if (resultRoot.handle !== undefined) handles.set(resultRoot.handle.name, resultRoot.handle)
  const dotGit: OptionalDirectoryObservation = resultRoot.handle === undefined
    ? { observation: absentObservation('directory', '.git', 'ancestor-absent') }
    : await optionalDirectory(resultRoot.handle, '.git')
  if (dotGit.handle !== undefined) handles.set(dotGit.handle.name, dotGit.handle)
  const hooks: OptionalDirectoryObservation = dotGit.handle === undefined
    ? { observation: absentObservation('directory', 'hooks', 'ancestor-absent') }
    : await optionalDirectory(dotGit.handle, 'hooks')
  if (hooks.handle !== undefined) handles.set(hooks.handle.name, hooks.handle)
  const target: OptionalFileObservation = hooks.handle === undefined
    ? { observation: absentObservation('file', PROBE_PATH.at(-1)!, 'ancestor-absent') }
    : await optionalFile(hooks.handle, PROBE_PATH.at(-1)!)
  if (target.handle !== undefined) handles.set(target.handle.name, target.handle)
  return Object.freeze({
    resultRoot: resultRoot.observation,
    dotGit: dotGit.observation,
    hooks: hooks.observation,
    target: target.observation,
    targetCommittedBytes: target.size?.toString() ?? '0',
    ...(resultRoot.handle === undefined ? {} : { resultRootHandle: resultRoot.handle }),
    ...(hooks.handle === undefined ? {} : { hooksHandle: hooks.handle }),
    handlesByName: handles,
  })
}

async function optionalDirectory(
  parent: FileSystemDirectoryHandle,
  name: string,
): Promise<OptionalDirectoryObservation> {
  try {
    const handle = await parent.getDirectoryHandle(name)
    return { handle, observation: { kind: 'directory', name, state: 'present' } }
  } catch (error) {
    return {
      observation: error instanceof DOMException && error.name === 'NotFoundError'
        ? absentObservation('directory', name, 'not-found')
        : { kind: 'directory', name, state: 'ambiguous', error: describeError(error) },
    }
  }
}

async function optionalFile(
  parent: FileSystemDirectoryHandle,
  name: string,
): Promise<OptionalFileObservation> {
  try {
    const handle = await parent.getFileHandle(name)
    const size = (await handle.getFile()).size
    return { handle, size, observation: { kind: 'file', name, state: 'present', size } }
  } catch (error) {
    return {
      observation: error instanceof DOMException && error.name === 'NotFoundError'
        ? absentObservation('file', name, 'not-found')
        : { kind: 'file', name, state: 'ambiguous', error: describeError(error) },
    }
  }
}

async function entry(
  parent: FileSystemDirectoryHandle,
  name: string,
  kind: 'directory' | 'file',
): Promise<unknown> {
  return kind === 'directory'
    ? (await optionalDirectory(parent, name)).observation
    : (await optionalFile(parent, name)).observation
}

function absentObservation(kind: 'directory' | 'file', name: string, reason: string): unknown {
  return Object.freeze({ kind, name, state: 'absent', reason })
}

async function describePersistedHandles(
  records: readonly Readonly<{
    id: string
    kind: string | number
    ownedObjectId?: string
    handle: unknown
  }>[],
  currentByName: ReadonlyMap<string, FileSystemHandle>,
): Promise<readonly unknown[]> {
  return Promise.all(records.map(async (record) => {
    const persisted = asFileSystemHandle(record.handle)
    const current = persisted === undefined ? undefined : currentByName.get(persisted.name)
    return Object.freeze({
      id: record.id,
      kind: record.kind,
      ownedObjectId: record.ownedObjectId ?? null,
      handle: describeHandle(persisted),
      currentEntryFoundByName: current !== undefined,
      currentEntryMatchesPersisted: current === undefined || persisted === undefined
        ? null
        : await current.isSameEntry(persisted),
    })
  }))
}

function checkpointFacts(
  candidates: readonly FileCheckpointV2[],
  committed: readonly FileCheckpointV2[],
): unknown {
  return Object.freeze({
    candidateCount: candidates.length,
    committedCount: committed.length,
    committedBytes: committed.reduce((sum, checkpoint) =>
      sum + checkpoint.verifiedRanges.reduce((rangeSum, range) => rangeSum + range.end - range.start, 0n), 0n
    ).toString(),
    candidates: candidates.map(checkpointEvidence),
    committed: committed.map(checkpointEvidence),
  })
}

function checkpointEvidence(checkpoint: FileCheckpointV2): unknown {
  return Object.freeze({
    recordId: checkpoint.recordId,
    canonicalPath: checkpoint.canonicalPath,
    exactSize: checkpoint.exactSize.toString(),
    ownedObjectId: checkpoint.ownedObjectId,
    stateGeneration: checkpoint.stateGeneration.toString(),
    checkpointGeneration: checkpoint.checkpointGeneration.toString(),
    commitState: checkpoint.commitState,
    verifiedRanges: checkpoint.verifiedRanges.map((range) => ({
      start: range.start.toString(),
      end: range.end.toString(),
    })),
  })
}

async function permissions(handle: FileSystemHandle): Promise<unknown> {
  const capable = handle as FileSystemHandle & {
    queryPermission?: (descriptor?: { readonly mode?: 'read' | 'readwrite' }) => Promise<PermissionState>
  }
  if (capable.queryPermission === undefined) return { read: 'unsupported', readwrite: 'unsupported' }
  return Object.freeze({
    read: await capable.queryPermission({ mode: 'read' }),
    readwrite: await capable.queryPermission({ mode: 'readwrite' }),
  })
}

function optionalPermissions(handle: FileSystemHandle | undefined): Promise<unknown> {
  return handle === undefined ? Promise.resolve(null) : permissions(handle)
}

function acquiredParent(parent: FileSystemDirectoryHandle): AcquiredFSAParentAuthority {
  const offer = fsaParentOffer()
  return Object.freeze({
    kind: 'fsa-parent-directory-authority',
    targetRouteId: offer.routeId,
    offer,
    parent,
  })
}

function probeArtifact(): Promise<DirectoryTreeArtifact> {
  return createResultRootDirectoryTreeArtifact(
    createCompleteDirectoryResultRoot(identity(95), PROBE_ARTIFACT_ROOT),
  )
}

function identity(seed: number, width = 16): string {
  const value = new Uint8Array(width)
  value[0] = seed
  value[value.length - 1] = seed ^ 0xff
  return encodeBase64Url(value)
}

function installNativeInstrumentation(): Instrumentation {
  const events: NativeEvent[] = []
  const restorers: Array<() => void> = []
  const writerIds = new WeakMap<object, string>()
  const writerFacts = {
    obtained: 0,
    writeStarted: 0,
    writeReturned: 0,
    writeRejected: 0,
    closeStarted: 0,
    closeReturned: 0,
    closeRejected: 0,
  }
  let sequence = 0
  let stage = 'probe.setup'
  let nextWriterId = 1
  const record: Instrumentation['record'] = (api, transition, receiver, details = {}) => {
    events.push(Object.freeze({ sequence: ++sequence, stage, api, transition, receiver, ...details }))
  }
  const constructors = globalThis as unknown as Record<
    string,
    { prototype?: Record<string, unknown> } | undefined
  >

  for (const [constructorName, methods] of [
    ['FileSystemHandle', ['isSameEntry', 'queryPermission', 'requestPermission']],
    ['FileSystemDirectoryHandle', ['getFileHandle', 'getDirectoryHandle', 'removeEntry']],
    ['FileSystemFileHandle', ['createWritable', 'getFile']],
    ['FileSystemWritableFileStream', ['write', 'close']],
  ] as const) {
    const prototype = constructors[constructorName]?.prototype
    if (prototype === undefined) continue
    for (const method of methods) {
      const descriptor = Object.getOwnPropertyDescriptor(prototype, method)
      if (descriptor === undefined || typeof descriptor.value !== 'function') continue
      const original = descriptor.value as (this: unknown, ...args: unknown[]) => unknown
      Object.defineProperty(prototype, method, {
        ...descriptor,
        value: async function (this: unknown, ...args: unknown[]): Promise<unknown> {
          const receiver = describeHandle(this, writerIds)
          record(method, 'start', receiver, { arguments: describeArguments(method, args) })
          if (method === 'write') writerFacts.writeStarted += 1
          if (method === 'close') writerFacts.closeStarted += 1
          try {
            const returned = await original.apply(this, args)
            if (method === 'createWritable' && typeof returned === 'object' && returned !== null) {
              writerIds.set(returned, `writer-${nextWriterId++}`)
              writerFacts.obtained += 1
            }
            if (method === 'write') writerFacts.writeReturned += 1
            if (method === 'close') writerFacts.closeReturned += 1
            record(method, 'return', describeHandle(this, writerIds), {
              returned: describeReturned(method, returned, writerIds),
            })
            return returned
          } catch (error) {
            if (method === 'write') writerFacts.writeRejected += 1
            if (method === 'close') writerFacts.closeRejected += 1
            record(method, 'reject', describeHandle(this, writerIds), { error: describeError(error) })
            throw error
          }
        },
      })
      restorers.push(() => Object.defineProperty(prototype, method, descriptor))
    }
  }

  return {
    events,
    writerFacts,
    async withStage<T>(nextStage: string, operation: () => Promise<T>): Promise<T> {
      const previous = stage
      stage = nextStage
      try {
        return await operation()
      } finally {
        stage = previous
      }
    },
    record,
    restore(): void {
      for (const restore of restorers.reverse()) restore()
    },
  }
}

function describeArguments(api: string, args: readonly unknown[]): unknown {
  if (api === 'write') {
    const value = args[0]
    if (value instanceof Uint8Array) return { byteLength: value.byteLength }
    if (typeof value === 'object' && value !== null) {
      const command = value as { type?: unknown; position?: unknown; data?: unknown }
      return {
        type: command.type ?? null,
        position: typeof command.position === 'number' ? command.position : null,
        byteLength: command.data instanceof Uint8Array ? command.data.byteLength : null,
      }
    }
  }
  return args.map((argument) => {
    if (typeof argument === 'object' && argument !== null) return { ...argument }
    return argument
  })
}

function describeReturned(
  api: string,
  returned: unknown,
  writerIds: WeakMap<object, string>,
): unknown {
  if (api === 'getFile') {
    return returned instanceof File ? { name: returned.name, size: returned.size } : null
  }
  if (typeof returned === 'object' && returned !== null) {
    return describeHandle(returned, writerIds)
  }
  return returned ?? null
}

function describeHandle(
  value: unknown,
  writerIds?: WeakMap<object, string>,
): HandleDescription {
  if (typeof value !== 'object' || value === null) return { kind: null, name: null }
  const candidate = value as { kind?: unknown; name?: unknown }
  const writerId = writerIds?.get(value)
  let kind: string | null
  if (typeof candidate.kind === 'string') kind = candidate.kind
  else if (writerId !== undefined) kind = 'writer'
  else kind = value.constructor?.name ?? null
  return Object.freeze({
    kind,
    name: typeof candidate.name === 'string' ? candidate.name : null,
    ...(writerId === undefined ? {} : { writerId }),
  })
}

function describeError(error: unknown): ErrorEvidence {
  const isError = error instanceof Error
  const isDomException = error instanceof DOMException
  let javascriptKind: ErrorEvidence['javascriptKind'] = 'unknown'
  if (isDomException) javascriptKind = 'dom-exception'
  else if (error instanceof TypeError) javascriptKind = 'type-error'
  return Object.freeze({
    constructor: isError ? error.constructor.name : null,
    javascriptKind,
    domExceptionName: isDomException ? error.name : null,
    message: isError ? error.message : String(error),
    stack: isError ? error.stack ?? null : null,
    thrownType: typeof error,
    thrownValue: String(error).slice(0, 2_048),
  })
}

function asFileSystemHandle(value: unknown): FileSystemHandle | undefined {
  if (typeof value !== 'object' || value === null) return undefined
  const candidate = value as { kind?: unknown; name?: unknown; isSameEntry?: unknown }
  return (candidate.kind === 'file' || candidate.kind === 'directory') &&
      typeof candidate.name === 'string' && typeof candidate.isSameEntry === 'function'
    ? value as FileSystemHandle
    : undefined
}
