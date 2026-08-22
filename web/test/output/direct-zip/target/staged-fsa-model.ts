import {
  chainDirectZipEpochDigestV1,
  digestDirectZipArchiveBytes,
  directZipEpochGenesisRoot,
} from '../../../../src/output/direct-zip/format'
import type {
  DirectZipCommittedEpochProofV1,
  DirectZipFileBinding,
  DirectZipParentBinding,
  DirectZipReservationCandidate,
  DirectZipReservationCandidateDraft,
  DirectZipReservationRetirementReason,
  DirectZipTargetObservationV1,
  DirectZipTargetTraceEvent,
} from '../../../../src/output/direct-zip/target'
import type {
  DirectZipFileSnapshotPort,
  DirectZipFileSystemPort,
  DirectZipHandleBindingPort,
  DirectZipOperationLease,
  DirectZipParentLock,
  DirectZipReservationCandidatePort,
  DirectZipWritablePort,
} from '../../../../src/output/direct-zip/target'
import type { DirectZipTargetDependencies } from '../../../../src/output/direct-zip/target'

export type StagedFsaFaultStage =
  | 'permission-query'
  | 'permission-request'
  | 'lookup'
  | 'create'
  | 'snapshot'
  | 'writable-open'
  | 'write'
  | 'truncate'
  | 'close-before-publication'
  | 'close-after-publication'
  | 'abort'
  | 'remove-before'
  | 'remove-after'

type StagedEntry = StagedFileNode | Readonly<{ readonly kind: 'directory'; readonly id: number }>

interface StagedFileNode {
  readonly kind: 'file'
  readonly id: number
  bytes: Uint8Array
  lastModified: number
}

export interface StagedParentHandle {
  readonly kind: 'directory'
  readonly id: number
  readonly name: string
}

export interface StagedFileHandle {
  readonly kind: 'file'
  readonly node: StagedFileNode
}

export class StagedFsaModel implements
  DirectZipFileSystemPort<StagedParentHandle, StagedFileHandle>,
  DirectZipHandleBindingPort<StagedParentHandle, StagedFileHandle>,
  DirectZipReservationCandidatePort<StagedParentHandle> {
  readonly parent: StagedParentHandle = Object.freeze({ kind: 'directory', id: 1, name: 'downloads' })
  readonly trace: DirectZipTargetTraceEvent[] = []
  readonly calls: string[] = []
  readonly candidates: DirectZipReservationCandidateDraft<StagedParentHandle>[] = []
  readonly retired: {
    readonly candidate: DirectZipReservationCandidate<StagedParentHandle>
    readonly reason: DirectZipReservationRetirementReason
  }[] = []
  queryPermissionState: PermissionState | 'unsupported' = 'granted'
  requestPermissionState: PermissionState | 'unsupported' = 'granted'

  readonly #entries = new Map<string, StagedEntry>()
  readonly #faults = new Map<StagedFsaFaultStage, unknown[]>()
  readonly #hooks = new Map<StagedFsaFaultStage, (() => void)[]>()
  #nextNodeId = 10
  #nextRandomByte = 1
  #clock = 100
  #operationHeld = false
  #parentHeld = false

  dependencies(): DirectZipTargetDependencies<StagedParentHandle, StagedFileHandle> {
    return Object.freeze({
      fileSystem: this,
      handleBindings: this,
      reservations: this,
      operationLeases: Object.freeze({ acquire: async () => this.#acquireOperation() }),
      parentLocks: Object.freeze({ acquire: async () => this.#acquireParent() }),
      random: this,
      trace: (event: DirectZipTargetTraceEvent) => this.trace.push(event),
    })
  }

  parentBinding(): DirectZipParentBinding<StagedParentHandle> {
    return Object.freeze({
      handleRef: 'parent:downloads',
      bindingDigest: bytes(32, 0x31),
      persistedHandle: this.parent,
    })
  }

  faultOnce(stage: StagedFsaFaultStage, error: unknown): void {
    const faults = this.#faults.get(stage) ?? []
    faults.push(error)
    this.#faults.set(stage, faults)
  }

  hookOnce(stage: StagedFsaFaultStage, hook: () => void): void {
    const hooks = this.#hooks.get(stage) ?? []
    hooks.push(hook)
    this.#hooks.set(stage, hooks)
  }

  occupyDirectory(name: string): void {
    this.#entries.set(name, Object.freeze({ kind: 'directory', id: this.#nextNodeId++ }))
  }

  installFile(name: string, contents: Uint8Array): StagedFileHandle {
    const node = this.#fileNode(contents)
    this.#entries.set(name, node)
    return Object.freeze({ kind: 'file', node })
  }

  replaceFile(name: string, contents: Uint8Array): StagedFileHandle {
    return this.installFile(name, contents)
  }

  deleteFile(name: string): void {
    this.#entries.delete(name)
  }

  fileBytes(name: string): Uint8Array | undefined {
    const entry = this.#entries.get(name)
    return entry?.kind === 'file' ? Uint8Array.from(entry.bytes) : undefined
  }

  bytes(length: number): Uint8Array {
    const result = bytes(length, this.#nextRandomByte)
    this.#nextRandomByte += 1
    return result
  }

  async queryPermission(): Promise<PermissionState | 'unsupported'> {
    this.calls.push('permission:query')
    this.#throwFault('permission-query')
    return this.queryPermissionState
  }

  async requestPermission(): Promise<PermissionState | 'unsupported'> {
    this.calls.push('permission:request')
    this.#throwFault('permission-request')
    return this.requestPermissionState
  }

  async lookupExactName(
    _parent: StagedParentHandle,
    stableName: string,
  ) {
    this.calls.push(`lookup:${stableName}`)
    this.#throwFault('lookup')
    const entry = this.#entries.get(stableName)
    if (entry === undefined) return Object.freeze({ kind: 'absent' as const })
    if (entry.kind === 'directory') return Object.freeze({ kind: 'occupied-non-file' as const })
    return Object.freeze({
      kind: 'file' as const,
      handle: Object.freeze({ kind: 'file' as const, node: entry }),
    })
  }

  async createFile(_parent: StagedParentHandle, stableName: string): Promise<StagedFileHandle> {
    this.calls.push(`create:${stableName}`)
    this.#throwFault('create')
    const existing = this.#entries.get(stableName)
    if (existing?.kind === 'directory') throw new DOMException('occupied directory', 'TypeMismatchError')
    const node = existing ?? this.#fileNode(new Uint8Array())
    if (node.kind !== 'file') throw new Error('staged file creation invariant failed')
    this.#entries.set(stableName, node)
    return Object.freeze({ kind: 'file', node })
  }

  async snapshot(file: StagedFileHandle): Promise<DirectZipFileSnapshotPort> {
    this.calls.push(`snapshot:${file.node.id}`)
    this.#throwFault('snapshot')
    const contents = Uint8Array.from(file.node.bytes)
    return Object.freeze({
      size: BigInt(contents.byteLength),
      lastModified: file.node.lastModified,
      read: async (start: bigint, end: bigint) => {
        const numericStart = Number(start)
        const numericEnd = Number(end)
        return Uint8Array.from(contents.subarray(numericStart, numericEnd))
      },
    })
  }

  async createWritable(
    file: StagedFileHandle,
    keepExistingData: true | false,
  ): Promise<DirectZipWritablePort> {
    this.calls.push(`writable:${file.node.id}:${keepExistingData}`)
    this.#throwFault('writable-open')
    let staged = keepExistingData ? Uint8Array.from(file.node.bytes) : new Uint8Array()
    let settled = false
    return Object.freeze({
      write: async (position: bigint, source: Uint8Array) => {
        requireOpen(settled)
        this.#throwFault('write')
        staged = writeAt(staged, Number(position), source)
      },
      truncate: async (size: bigint) => {
        requireOpen(settled)
        this.#throwFault('truncate')
        staged = truncate(staged, Number(size))
      },
      close: async () => {
        requireOpen(settled)
        settled = true
        this.#runHook('close-before-publication')
        this.#throwFault('close-before-publication')
        file.node.bytes = Uint8Array.from(staged)
        file.node.lastModified = ++this.#clock
        this.#runHook('close-after-publication')
        this.#throwFault('close-after-publication')
      },
      abort: async () => {
        requireOpen(settled)
        settled = true
        this.#runHook('abort')
        this.#throwFault('abort')
      },
    })
  }

  async removeExactName(_parent: StagedParentHandle, stableName: string): Promise<void> {
    this.calls.push(`remove:${stableName}`)
    this.#runHook('remove-before')
    this.#throwFault('remove-before')
    this.#entries.delete(stableName)
    this.#runHook('remove-after')
    this.#throwFault('remove-after')
  }

  async compareParent(
    binding: DirectZipParentBinding<StagedParentHandle>,
    currentParent: StagedParentHandle,
  ) {
    return binding.persistedHandle.id === currentParent.id ? 'same' as const : 'different' as const
  }

  async compareFile(
    binding: DirectZipFileBinding<StagedFileHandle>,
    currentFile: StagedFileHandle,
  ) {
    return binding.persistedHandle.node.id === currentFile.node.id ? 'same' as const : 'different' as const
  }

  async compareCurrentFiles(left: StagedFileHandle, right: StagedFileHandle) {
    return left.node.id === right.node.id ? 'same' as const : 'different' as const
  }

  async bindFile(input: Readonly<{
    readonly stableName: string
    readonly file: StagedFileHandle
  }>): Promise<DirectZipFileBinding<StagedFileHandle>> {
    return Object.freeze({
      handleRef: `file:${input.stableName}:${input.file.node.id}`,
      bindingDigest: bytes(32, input.file.node.id),
      persistedHandle: input.file,
    })
  }

  async persistCandidate(
    draft: DirectZipReservationCandidateDraft<StagedParentHandle>,
  ) {
    this.calls.push(`candidate:persist:${draft.stableName}`)
    this.candidates.push(draft)
    const seed = draft.candidateId[0] ?? 1
    return Object.freeze({ targetRef: bytes(32, seed + 20), bindingDigest: bytes(32, seed + 40) })
  }

  async retireCandidate(
    candidate: DirectZipReservationCandidate<StagedParentHandle>,
    reason: DirectZipReservationRetirementReason,
  ): Promise<void> {
    this.calls.push(`candidate:retire:${reason}`)
    this.retired.push({ candidate, reason })
  }

  #acquireOperation(): DirectZipOperationLease {
    if (this.#operationHeld) throw new DOMException('operation busy', 'InvalidStateError')
    this.#operationHeld = true
    this.calls.push('lock:operation:acquire')
    return Object.freeze({
      leaseId: 'lease-1',
      generation: 1n,
      release: async () => {
        this.calls.push('lock:operation:release')
        this.#operationHeld = false
      },
    })
  }

  #acquireParent(): DirectZipParentLock {
    if (!this.#operationHeld || this.#parentHeld) {
      throw new DOMException('parent lock ordering failed', 'InvalidStateError')
    }
    this.#parentHeld = true
    this.calls.push('lock:parent:acquire')
    return Object.freeze({
      name: 'parent-lock',
      release: async () => {
        this.calls.push('lock:parent:release')
        this.#parentHeld = false
      },
    })
  }

  #fileNode(contents: Uint8Array): StagedFileNode {
    return {
      kind: 'file',
      id: this.#nextNodeId++,
      bytes: Uint8Array.from(contents),
      lastModified: ++this.#clock,
    }
  }

  #throwFault(stage: StagedFsaFaultStage): void {
    const faults = this.#faults.get(stage)
    const error = faults?.shift()
    if (error !== undefined) throw error
  }

  #runHook(stage: StagedFsaFaultStage): void {
    this.#hooks.get(stage)?.shift()?.()
  }
}

export function bootstrapCheckpoint(
  archive: Uint8Array,
  observation: DirectZipTargetObservationV1,
): Readonly<{
  readonly committedLength: bigint
  readonly observation: DirectZipTargetObservationV1
  readonly committedEpochs: readonly DirectZipCommittedEpochProofV1[]
}> {
  const contentDigest = digestDirectZipArchiveBytes(archive)
  const predecessorRoot = directZipEpochGenesisRoot()
  const end = BigInt(archive.byteLength)
  const epochRoot = chainDirectZipEpochDigestV1({
    predecessorRoot,
    start: 0n,
    end,
    contentDigest,
  })
  return Object.freeze({
    committedLength: end,
    observation,
    committedEpochs: Object.freeze([Object.freeze({
      start: 0n,
      end,
      predecessorRoot,
      epochRoot,
    })]),
  })
}

export function bytes(length: number, value: number): Uint8Array<ArrayBuffer> {
  return new Uint8Array(new ArrayBuffer(length)).fill(value & 0xff)
}

function writeAt(
  target: Uint8Array,
  position: number,
  source: Uint8Array,
): Uint8Array<ArrayBuffer> {
  const next = new Uint8Array(Math.max(target.byteLength, position + source.byteLength))
  next.set(target)
  next.set(source, position)
  return next
}

function truncate(target: Uint8Array, size: number): Uint8Array<ArrayBuffer> {
  const next = new Uint8Array(size)
  next.set(target.subarray(0, size))
  return next
}

function requireOpen(settled: boolean): void {
  if (settled) throw new DOMException('staged writable is settled', 'InvalidStateError')
}
