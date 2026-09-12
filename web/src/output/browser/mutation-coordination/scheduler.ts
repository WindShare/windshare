import {
  FSAOperationMutationClosedError,
  FSATerminalMutationAlreadyBegunError,
  FSATerminalMutationUnavailableError,
  type FSAOperationMutationScheduler,
  type FSAFileMutationIdentity,
  type FSAFileMutationKind,
  type FSAMutationSchedulerDiagnosticsSnapshot,
  type FSAMutationSchedulerState,
  type FSANamespaceMutationKind,
  type FSAParentMutationIdentity,
  type FSATerminalDrain,
  type FSATerminalExclusiveAuthority,
  type FSATerminalMutationKind,
  type FSAWriterLifecycleLease,
  type FSAVerifiedFileMutationTarget,
} from './model'
import { FSAMutationSchedulerDiagnostics } from './scheduler-diagnostics'
import {
  observePerformance,
  performanceElapsedMilliseconds,
  performanceNowMilliseconds,
  type PerformanceSummaryObservations,
} from '../../diagnostics/performance-summary'
import type { PerformanceNamespaceKindV1 } from '../../../diagnostics/trace/transfer-payload'

export interface CreateFSAOperationMutationSchedulerOptions {
  readonly rootParent: FSAParentMutationIdentity
  readonly maximumActiveWriters: number
  readonly performance?: PerformanceSummaryObservations
}

interface ParentLane {
  readonly identity: FSAParentMutationIdentity
  readonly ordinal: number
  readonly namespaceQueue: NamespaceRequest[]
  activeNamespace: NamespaceRequest | undefined
}

interface FileLane {
  readonly target: FSAVerifiedFileMutationTarget
  readonly parent: ParentLane
  activeWriter: boolean
}

interface NamespaceRequest {
  readonly order: number
  readonly queuedAtMilliseconds: number | undefined
  readonly kind: FSANamespaceMutationKind | FSAFileMutationKind
  readonly lanes: readonly ParentLane[]
  readonly file: FileLane | undefined
  readonly operation: () => Promise<unknown>
  readonly resolve: (value: unknown) => void
  readonly reject: (reason: unknown) => void
}

interface WriterRequest {
  readonly order: number
  readonly queuedAtMilliseconds: number | undefined
  readonly lane: FileLane
  readonly resolve: (lease: FSAWriterLifecycleLease) => void
}

interface TerminalController {
  readonly kind: FSATerminalMutationKind
  claimed: boolean
}

interface Deferred {
  readonly promise: Promise<void>
  readonly resolve: () => void
}

export function createFSAOperationMutationScheduler(
  options: CreateFSAOperationMutationSchedulerOptions,
): FSAOperationMutationScheduler {
  return new OperationMutationScheduler(options)
}

class OperationMutationScheduler implements FSAOperationMutationScheduler {
  readonly #rootParent: FSAParentMutationIdentity
  readonly #maximumActiveWriters: number
  readonly #diagnostics: FSAMutationSchedulerDiagnostics
  readonly #performance: PerformanceSummaryObservations | undefined
  readonly #lanes = new Map<FSAParentMutationIdentity, ParentLane>()
  readonly #files = new Map<FSAFileMutationIdentity, FileLane>()
  readonly #namespaceRequests: NamespaceRequest[] = []
  readonly #writerRequests: WriterRequest[] = []
  #state: FSAMutationSchedulerState = 'accepting'
  #nextOrder = 0
  #nextLaneOrdinal = 0
  #activeWriters = 0
  #pumping = false
  #terminal: TerminalController | undefined
  #drain: Deferred | undefined
  #drainResolved = false
  #exclusiveSettlement: Promise<void> | undefined
  #closeRequested = false
  #closePromise: Promise<void> | undefined

  constructor(options: CreateFSAOperationMutationSchedulerOptions) {
    requireParentIdentity(options.rootParent)
    if (!Number.isSafeInteger(options.maximumActiveWriters) ||
        options.maximumActiveWriters <= 0) {
      throw new TypeError('FSA maximum active writers must be a positive safe integer')
    }
    this.#rootParent = options.rootParent
    this.#maximumActiveWriters = options.maximumActiveWriters
    this.#diagnostics = new FSAMutationSchedulerDiagnostics(options.maximumActiveWriters)
    this.#performance = options.performance
    this.#observeConcurrency()
  }

  runRootNamespace<T>(
    kind: FSANamespaceMutationKind,
    operation: () => Promise<T>,
  ): Promise<T> {
    return this.runNamespace([this.#rootParent], kind, operation)
  }

  runNamespace<T>(
    parents: readonly FSAParentMutationIdentity[],
    kind: FSANamespaceMutationKind,
    operation: () => Promise<T>,
  ): Promise<T> {
    requireNamespaceMutationKind(kind)
    return this.#enqueueNamespace(parents, kind, operation)
  }

  runFileMutation<T>(
    target: FSAVerifiedFileMutationTarget,
    kind: FSAFileMutationKind,
    operation: () => Promise<T>,
  ): Promise<T> {
    if (kind !== 'remove-file') throw new TypeError('FSA file mutation kind is invalid')
    requireFileTarget(target)
    requireOperation(operation)
    if (this.#state !== 'accepting') return Promise.reject(new FSAOperationMutationClosedError())
    const file = this.#file(target)
    return this.#enqueueNamespace([target.parent], kind, operation, file)
  }

  #enqueueNamespace<T>(
    parents: readonly FSAParentMutationIdentity[],
    kind: FSANamespaceMutationKind | FSAFileMutationKind,
    operation: () => Promise<T>,
    file?: FileLane,
  ): Promise<T> {
    requireOperation(operation)
    if (this.#state !== 'accepting') {
      return Promise.reject(new FSAOperationMutationClosedError())
    }

    const lanes = this.#normalizeParents(parents)
    const order = this.#nextSequence()
    const result = new Promise<T>((resolve, reject) => {
      const request: NamespaceRequest = {
        order,
        queuedAtMilliseconds: performanceNowMilliseconds(this.#performance),
        kind,
        lanes,
        file,
        operation,
        resolve: value => { resolve(value as T) },
        reject,
      }
      this.#namespaceRequests.push(request)
      for (const lane of lanes) {
        lane.namespaceQueue.push(request)
      }
      this.#diagnostics.namespaceQueued()
      this.#observeConcurrency()
    })
    this.#pump()
    return result
  }

  acquireWriter(target: FSAVerifiedFileMutationTarget): Promise<FSAWriterLifecycleLease> {
    requireFileTarget(target)
    if (this.#state !== 'accepting') {
      return Promise.reject(new FSAOperationMutationClosedError())
    }

    const lane = this.#file(target)
    const result = new Promise<FSAWriterLifecycleLease>((resolve) => {
      this.#writerRequests.push({
        order: this.#nextSequence(),
        queuedAtMilliseconds: performanceNowMilliseconds(this.#performance),
        lane,
        resolve,
      })
      this.#diagnostics.writerQueued()
      this.#observeConcurrency()
    })
    this.#pump()
    return result
  }

  beginTerminal(kind: FSATerminalMutationKind): FSATerminalDrain {
    requireTerminalMutationKind(kind)
    if (this.#state !== 'accepting' || this.#terminal !== undefined) {
      throw new FSATerminalMutationAlreadyBegunError()
    }

    const terminal: TerminalController = { kind, claimed: false }
    this.#terminal = terminal
    this.#beginDraining()
    const drained = this.#requireDrain().promise
    return Object.freeze({
      kind,
      drained,
      runExclusive: <T>(
        operation: (authority: FSATerminalExclusiveAuthority) => Promise<T>,
      ) => this.#runTerminalExclusive(terminal, operation),
    })
  }

  close(): Promise<void> {
    if (this.#closePromise !== undefined) return this.#closePromise
    if (this.#state === 'closed') return Promise.resolve()

    this.#closeRequested = true
    if (this.#state === 'accepting') this.#beginDraining()
    const drained = this.#requireDrain().promise
    this.#closePromise = (async () => {
      await drained
      const exclusiveSettlement = this.#exclusiveSettlement
      if (exclusiveSettlement !== undefined) await exclusiveSettlement
      if (this.#state !== 'closed') this.#transitionClosed()
    })()
    return this.#closePromise
  }

  diagnostics(): FSAMutationSchedulerDiagnosticsSnapshot {
    return this.#diagnostics.snapshot()
  }

  #normalizeParents(parents: readonly FSAParentMutationIdentity[]): readonly ParentLane[] {
    if (parents.length === 0) {
      throw new TypeError('FSA namespace mutation requires at least one verified parent')
    }
    const seen = new Set<FSAParentMutationIdentity>()
    const lanes = parents.map((parent) => {
      requireParentIdentity(parent)
      if (seen.has(parent)) {
        throw new TypeError('FSA namespace mutation parents must be unique')
      }
      seen.add(parent)
      return this.#lane(parent)
    })
    lanes.sort((left, right) => left.ordinal - right.ordinal)
    return Object.freeze(lanes)
  }

  #lane(identity: FSAParentMutationIdentity): ParentLane {
    const existing = this.#lanes.get(identity)
    if (existing !== undefined) return existing
    const lane: ParentLane = {
      identity,
      ordinal: this.#nextLaneOrdinal,
      namespaceQueue: [],
      activeNamespace: undefined,
    }
    this.#nextLaneOrdinal += 1
    this.#lanes.set(identity, lane)
    return lane
  }

  #file(target: FSAVerifiedFileMutationTarget): FileLane {
    const existing = this.#files.get(target.file)
    if (existing !== undefined) {
      if (existing.target.parent !== target.parent) {
        throw new TypeError('FSA file mutation identity changed its verified parent')
      }
      return existing
    }
    const file: FileLane = {
      target: Object.freeze({ parent: target.parent, file: target.file }),
      parent: this.#lane(target.parent),
      activeWriter: false,
    }
    this.#files.set(target.file, file)
    return file
  }

  #nextSequence(): number {
    const sequence = this.#nextOrder
    this.#nextOrder += 1
    return sequence
  }

  #pump(): void {
    if (this.#pumping) return
    this.#pumping = true
    try {
      let progressed: boolean
      do {
        progressed = this.#startReadyNamespaceRequests()
        progressed = this.#admitEligibleWriters() || progressed
      } while (progressed)
      this.#resolveDrainIfIdle()
    } finally {
      this.#pumping = false
    }
  }

  #startReadyNamespaceRequests(): boolean {
    let progressed = false
    for (let index = 0; index < this.#namespaceRequests.length;) {
      const request = this.#namespaceRequests[index]!
      if (!this.#namespaceReady(request)) {
        index += 1
        continue
      }
      this.#namespaceRequests.splice(index, 1)
      for (const lane of request.lanes) {
        const queuedIndex = lane.namespaceQueue.indexOf(request)
        if (queuedIndex < 0) {
          throw new Error('FSA namespace lane order was corrupted')
        }
        lane.namespaceQueue.splice(queuedIndex, 1)
        lane.activeNamespace = request
      }
      this.#diagnostics.namespaceStarted()
      this.#observeConcurrency()
      this.#executeNamespace(request, performanceNowMilliseconds(this.#performance))
      progressed = true
    }
    return progressed
  }

  #namespaceReady(request: NamespaceRequest): boolean {
    return request.lanes.every((lane) =>
      lane.activeNamespace === undefined &&
      lane.namespaceQueue.find(candidate => this.#fileMutationReady(candidate)) === request,
    )
  }

  #fileMutationReady(request: NamespaceRequest): boolean {
    const file = request.file
    if (file === undefined) return true
    // Waiting for one file to close must not reserve its entire parent. Sibling
    // inspection/creation can use the short namespace lane while this file drains.
    return !file.activeWriter && !this.#writerRequests.some(
      writer => writer.lane === file && writer.order < request.order,
    )
  }

  #executeNamespace(
    request: NamespaceRequest,
    startedAtMilliseconds: number | undefined,
  ): void {
    void (async () => {
      let succeeded = false
      try {
        const value = await request.operation()
        succeeded = true
        request.resolve(value)
      } catch (cause) {
        request.reject(cause)
      }
      for (const lane of request.lanes) {
        if (lane.activeNamespace !== request) {
          throw new Error('FSA namespace active lane was corrupted')
        }
        lane.activeNamespace = undefined
      }
      this.#diagnostics.namespaceSettled(succeeded)
      if (succeeded) {
        this.#observeQueueRun(
          'namespace',
          request.queuedAtMilliseconds,
          startedAtMilliseconds,
          performanceNowMilliseconds(this.#performance),
          performanceNamespaceKind(request.kind),
        )
      }
      this.#observeConcurrency()
      this.#pump()
    })()
  }

  #admitEligibleWriters(): boolean {
    let progressed = false
    while (this.#activeWriters < this.#maximumActiveWriters) {
      const index = this.#writerRequests.findIndex(request => this.#writerEligible(request))
      if (index < 0) break
      const [request] = this.#writerRequests.splice(index, 1)
      if (request === undefined) throw new Error('FSA writer queue was corrupted')
      this.#activeWriters += 1
      request.lane.activeWriter = true
      this.#diagnostics.writerAcquired()
      const startedAtMilliseconds = performanceNowMilliseconds(this.#performance)
      this.#observeConcurrency()
      request.resolve(this.#writerLease(
        request.lane,
        request.queuedAtMilliseconds,
        startedAtMilliseconds,
      ))
      progressed = true
    }
    return progressed
  }

  #writerEligible(request: WriterRequest): boolean {
    const lane = request.lane
    if (lane.activeWriter || lane.parent.activeNamespace?.file === lane) return false
    return !lane.parent.namespaceQueue.some(mutation =>
      mutation.file === lane && mutation.order < request.order)
  }

  #writerLease(
    lane: FileLane,
    queuedAtMilliseconds: number | undefined,
    startedAtMilliseconds: number | undefined,
  ): FSAWriterLifecycleLease {
    let released = false
    return Object.freeze({
      target: lane.target,
      release: () => {
        if (released) return
        released = true
        lane.activeWriter = false
        this.#activeWriters -= 1
        if (this.#activeWriters < 0) {
          throw new Error('FSA writer lease accounting underflowed')
        }
        this.#diagnostics.writerReleased()
        this.#observeQueueRun(
          'writer',
          queuedAtMilliseconds,
          startedAtMilliseconds,
          performanceNowMilliseconds(this.#performance),
        )
        this.#observeConcurrency()
        this.#pump()
      },
    })
  }

  #beginDraining(): void {
    this.#state = 'draining'
    this.#diagnostics.transition('draining')
    this.#drain = deferred()
    this.#pump()
  }

  #resolveDrainIfIdle(): void {
    if (this.#state !== 'draining' || this.#drainResolved || this.#hasOutstandingWork()) return
    this.#drainResolved = true
    this.#requireDrain().resolve()
  }

  #hasOutstandingWork(): boolean {
    if (this.#activeWriters !== 0 ||
        this.#writerRequests.length !== 0 ||
        this.#namespaceRequests.length !== 0) {
      return true
    }
    for (const lane of this.#lanes.values()) {
      if (lane.activeNamespace !== undefined || lane.namespaceQueue.length !== 0) return true
    }
    return false
  }

  #runTerminalExclusive<T>(
    terminal: TerminalController,
    operation: (authority: FSATerminalExclusiveAuthority) => Promise<T>,
  ): Promise<T> {
    requireOperation(operation)
    if (terminal !== this.#terminal ||
        terminal.claimed ||
        this.#closeRequested ||
        this.#state !== 'draining') {
      return Promise.reject(new FSATerminalMutationUnavailableError())
    }
    terminal.claimed = true

    const authority = Object.freeze({
      kind: terminal.kind,
    }) as unknown as FSATerminalExclusiveAuthority
    const result = (async () => {
      await this.#requireDrain().promise
      if (this.#state !== 'draining') throw new FSATerminalMutationUnavailableError()
      let succeeded = false
      try {
        const value = await operation(authority)
        succeeded = true
        return value
      } finally {
        this.#diagnostics.terminalSettled(succeeded)
        this.#transitionClosed()
      }
    })()
    this.#exclusiveSettlement = result.then(
      () => undefined,
      () => undefined,
    )
    return result
  }

  #transitionClosed(): void {
    if (this.#state === 'closed') return
    if (this.#hasOutstandingWork()) {
      throw new Error('FSA mutation scheduler cannot close with active work')
    }
    this.#state = 'closed'
    this.#diagnostics.transition('closed')
  }

  #observeConcurrency(): void {
    const snapshot = this.#diagnostics.snapshot()
    observePerformance(this.#performance, summary => summary.observeConcurrency({
      activeWriters: snapshot.activeWriters,
      queuedWriters: snapshot.queuedWriters,
      activeNamespace: snapshot.activeNamespaceMutations,
      queuedNamespace: snapshot.queuedNamespaceMutations,
    }))
  }

  #observeQueueRun(
    lane: 'writer' | 'namespace',
    queuedAtMilliseconds: number | undefined,
    startedAtMilliseconds: number | undefined,
    completedAtMilliseconds: number | undefined,
    namespaceKind?: PerformanceNamespaceKindV1,
  ): void {
    const waitMilliseconds = performanceElapsedMilliseconds(
      queuedAtMilliseconds,
      startedAtMilliseconds,
    )
    const runMilliseconds = performanceElapsedMilliseconds(
      startedAtMilliseconds,
      completedAtMilliseconds,
    )
    if (waitMilliseconds === undefined || runMilliseconds === undefined) return
    observePerformance(this.#performance, summary =>
      summary.observeQueueRun(lane, waitMilliseconds, runMilliseconds, namespaceKind))
  }

  #requireDrain(): Deferred {
    if (this.#drain === undefined) {
      throw new Error('FSA mutation scheduler drain was not initialized')
    }
    return this.#drain
  }
}

function performanceNamespaceKind(
  kind: FSANamespaceMutationKind | FSAFileMutationKind,
): PerformanceNamespaceKindV1 {
  switch (kind) {
    case 'inspect-entry': return 'inspect_entry'
    case 'reserve-name': return 'reserve_name'
    case 'create-directory': return 'create_directory'
    case 'create-file': return 'create_file'
    case 'remove-file': return 'remove_entry'
    case 'repair-compatible-name': return 'repair_compatible_name'
  }
}

function deferred(): Deferred {
  let resolve!: () => void
  const promise = new Promise<void>((complete) => { resolve = complete })
  return { promise, resolve }
}

function requireParentIdentity(parent: FSAParentMutationIdentity): void {
  if (typeof parent !== 'symbol') {
    throw new TypeError('FSA parent mutation identity must be an opaque authority token')
  }
}

function requireFileTarget(target: FSAVerifiedFileMutationTarget): void {
  requireParentIdentity(target.parent)
  if (typeof target.file !== 'symbol') {
    throw new TypeError('FSA file mutation identity must be an opaque authority token')
  }
}

function requireOperation(operation: unknown): asserts operation is () => Promise<unknown> {
  if (typeof operation !== 'function') {
    throw new TypeError('FSA mutation operation must be a function')
  }
}

function requireNamespaceMutationKind(kind: FSANamespaceMutationKind): void {
  switch (kind) {
    case 'inspect-entry':
    case 'reserve-name':
    case 'create-directory':
    case 'create-file':
    case 'repair-compatible-name':
      return
  }
  throw new TypeError('FSA namespace mutation kind is invalid')
}

function requireTerminalMutationKind(kind: FSATerminalMutationKind): void {
  switch (kind) {
    case 'settle-operation':
    case 'pause-operation':
    case 'stop-operation':
    case 'discard-operation':
    case 'repair-operation':
      return
  }
  throw new TypeError('FSA terminal mutation kind is invalid')
}
