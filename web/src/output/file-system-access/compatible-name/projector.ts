import type { CompatibleNameLedger } from './ledger'
import type {
  CompatibleNameFooterObservationV1,
  CompatibleNameFooterState,
  CompatibleNameMappingV1,
  CompatibleNamePairPlacement,
  CompatibleNameTerminalFooterState,
} from './model'
import {
  compatibleNameSidecarPlacement,
  decodeCompatibleNameSidecar,
  encodeCompatibleNameSidecarFooter,
  encodeCompatibleNameSidecarHeader,
  encodeCompatibleNameSidecarMapping,
  type CompatibleNameSidecarCheckpointV1,
  type CompatibleNameSidecarMappingV1,
} from './sidecar-codec'

const TEXT_ENCODER = new TextEncoder()
const BACKGROUND_FLUSH_PASS_LIMIT = 2

export type CompatibleNameProjectorLedger = Pick<
  CompatibleNameLedger,
  'scanCommittedMappings'
>

/**
 * Implementations must re-verify the persisted sidecar ownership correlation before
 * every mutation and must open, mutate, and close their own writer. Keeping this port
 * separate prevents background projection from entering the root-wide mutation tail.
 */
export interface CompatibleNameOwnedSidecarWriter {
  readOwnedBytes(): Promise<Uint8Array>
  appendOwnedCheckpoint(bytes: Uint8Array): Promise<void>
  truncateOwnedBytes(byteLength: number): Promise<void>
  replaceOwnedCheckpoint(bytes: Uint8Array): Promise<void>
}

export type CompatibleNameProjectorTraceEvent = Readonly<{
  operationId: string
  stage: 'restart' | 'background' | 'terminal'
  decision:
    | 'checkpoint-observed'
    | 'tail-truncated'
    | 'sidecar-rebuilt'
    | 'batch-appended'
    | 'flush-failed'
    | 'terminal-validated'
  committedCount: number
  footerState: CompatibleNameFooterState
  appendedCount?: number
  cause?: unknown
}>

export interface CompatibleNameProjectorOptions {
  readonly operationId: string
  readonly pairPlacement: CompatibleNamePairPlacement
  readonly ledger: CompatibleNameProjectorLedger
  readonly writer: CompatibleNameOwnedSidecarWriter
  /** The callback durably qualifies each validated filesystem checkpoint before publication. */
  readonly checkpointed?: (observation: CompatibleNameFooterObservationV1) => Promise<void>
  readonly trace?: (event: CompatibleNameProjectorTraceEvent) => void
}

export interface CompatibleNameProjector {
  /** A commit notification never exposes sidecar latency to the committing worker. */
  markDirty(): void
  synchronizeActive(reconcile?: () => Promise<void>): Promise<CompatibleNameFooterObservationV1>
  /** Returns the latest completely closed and validated footer without forcing catch-up. */
  observeFooter(): CompatibleNameFooterObservationV1
  /** Completion, failure, and explicit Stop share this single terminal reconciliation. */
  drainTerminal(
    state: CompatibleNameTerminalFooterState,
  ): Promise<CompatibleNameFooterObservationV1>
}

export class CompatibleNameProjectorError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'CompatibleNameProjectorError'
  }
}

class CoalescingCompatibleNameProjector implements CompatibleNameProjector {
  readonly #operationId: string
  readonly #placement: ReturnType<typeof compatibleNameSidecarPlacement>
  readonly #ledger: CompatibleNameProjectorLedger
  readonly #writer: CompatibleNameOwnedSidecarWriter
  readonly #checkpointed: ((observation: CompatibleNameFooterObservationV1) => Promise<void>) | undefined
  readonly #trace: ((event: CompatibleNameProjectorTraceEvent) => void) | undefined

  #checkpoint!: CompatibleNameSidecarCheckpointV1
  #synchronizing = false
  #dirty = false
  #flushPromise: Promise<void> | undefined
  #terminalState: CompatibleNameTerminalFooterState | undefined
  #terminalPromise: Promise<CompatibleNameFooterObservationV1> | undefined

  constructor(options: CompatibleNameProjectorOptions) {
    this.#operationId = options.operationId
    this.#placement = compatibleNameSidecarPlacement(options.pairPlacement)
    this.#ledger = options.ledger
    this.#writer = options.writer
    this.#checkpointed = options.checkpointed
    this.#trace = options.trace
  }

  async initialize(): Promise<void> {
    this.#checkpoint = await this.#reconcileStoredCheckpoint('active', 'restart')
    await this.#publishCheckpoint()
  }

  markDirty(): void {
    if (this.#terminalState !== undefined || this.#checkpoint.footer.state !== 'active') {
      throw new CompatibleNameProjectorError(
        'compatible-name mapping committed after terminal projection began',
      )
    }
    this.#dirty = true
    this.#startBackgroundFlush()
  }

  observeFooter(): CompatibleNameFooterObservationV1 {
    return Object.freeze({
      committedCount: this.#checkpoint.footer.committedCount,
      state: this.#checkpoint.footer.state,
    })
  }

  drainTerminal(
    state: CompatibleNameTerminalFooterState,
  ): Promise<CompatibleNameFooterObservationV1> {
    assertTerminalFooterState(state)
    if (this.#terminalState !== undefined) {
      if (state !== this.#terminalState) {
        return Promise.reject(new CompatibleNameProjectorError(
          'compatible-name terminal projection was requested with a different outcome',
        ))
      }
      return this.#terminalPromise as Promise<CompatibleNameFooterObservationV1>
    }

    this.#terminalState = state
    this.#terminalPromise = this.#drainTerminalOnce(state)
    return this.#terminalPromise
  }

  async synchronizeActive(reconcile?: () => Promise<void>): Promise<CompatibleNameFooterObservationV1> {
    if (this.#terminalState !== undefined || this.#synchronizing) {
      throw new CompatibleNameProjectorError('active synchronization requires exclusive active authority')
    }
    this.#synchronizing = true
    try {
      await this.#flushPromise?.catch(() => undefined)
      // Replay can commit several mappings before failing. Keeping all its dirty
      // notifications inside this scope prevents a writer surviving failed reopen.
      await reconcile?.()
      this.#checkpoint = await this.#reconcileStoredCheckpoint('active', 'restart')
      if (this.#checkpoint.footer.state !== 'active') {
        throw new CompatibleNameProjectorError('active synchronization encountered a terminal footer')
      }
      this.#dirty = false
      await this.#publishCheckpoint()
      return this.observeFooter()
    } finally {
      this.#synchronizing = false
    }
  }

  #startBackgroundFlush(): void {
    if (this.#synchronizing || this.#flushPromise !== undefined || !this.#dirty || this.#terminalState !== undefined) return

    const flush = this.#flushBackground()
    this.#flushPromise = flush
    flush.then(
      () => this.#finishBackgroundFlush(flush, true),
      (cause: unknown) => {
        this.#emit({
          stage: 'background',
          decision: 'flush-failed',
          committedCount: this.#checkpoint.footer.committedCount,
          footerState: this.#checkpoint.footer.state,
          cause,
        })
        this.#finishBackgroundFlush(flush, false)
      },
    )
  }

  #finishBackgroundFlush(flush: Promise<void>, succeeded: boolean): void {
    if (this.#flushPromise !== flush) return
    this.#flushPromise = undefined
    // Persistent failures wait for a later commit notification or the terminal cut;
    // immediately respawning here would create an unbounded retry loop.
    if (succeeded && this.#dirty && this.#terminalState === undefined) {
      this.#startBackgroundFlush()
    }
  }

  async #flushBackground(): Promise<void> {
    for (let pass = 0; pass < BACKGROUND_FLUSH_PASS_LIMIT; pass += 1) {
      if (!this.#dirty || this.#terminalState !== undefined) return
      this.#dirty = false
      await this.#appendCommittedSuffix('active', 'background')
    }
  }

  async #drainTerminalOnce(
    state: CompatibleNameTerminalFooterState,
  ): Promise<CompatibleNameFooterObservationV1> {
    const backgroundFlush = this.#flushPromise
    if (backgroundFlush !== undefined) {
      // A failed background write can leave a torn tail. Terminal reconciliation
      // deliberately re-reads storage, so the initiating failure is diagnostic only.
      await backgroundFlush.catch(() => undefined)
    }

    let observation!: CompatibleNameFooterObservationV1
    const terminalFlush = (async () => {
      this.#checkpoint = await this.#reconcileStoredCheckpoint(state, 'terminal')
      this.#dirty = false
      observation = this.observeFooter()
      await this.#publishCheckpoint(observation)
    })()
    if (this.#flushPromise !== undefined) {
      throw new CompatibleNameProjectorError('compatible-name sidecar writer overlap detected')
    }
    this.#flushPromise = terminalFlush

    try {
      await terminalFlush
      this.#emit({
        stage: 'terminal',
        decision: 'terminal-validated',
        committedCount: observation.committedCount,
        footerState: observation.state,
      })
      return observation
    } finally {
      if (this.#flushPromise === terminalFlush) this.#flushPromise = undefined
    }
  }

  async #appendCommittedSuffix(
    footerState: CompatibleNameFooterState,
    stage: CompatibleNameProjectorTraceEvent['stage'],
  ): Promise<void> {
    if (this.#checkpoint.footer.state !== 'active') {
      throw new CompatibleNameProjectorError(
        'compatible-name sidecar cannot append after a terminal footer',
      )
    }
    const cursor = this.#checkpoint.footer.committedCount
    const suffix = await this.#scanCommittedMappings(cursor)
    if (suffix.length === 0 && footerState === 'active') return
    this.#checkpoint = await this.#appendAndValidate(this.#checkpoint, suffix, footerState)
    await this.#publishCheckpoint()
    this.#emit({
      stage,
      decision: 'batch-appended',
      committedCount: this.#checkpoint.footer.committedCount,
      footerState,
      appendedCount: suffix.length,
    })
  }

  async #reconcileStoredCheckpoint(
    requestedState: CompatibleNameFooterState,
    stage: 'restart' | 'terminal',
  ): Promise<CompatibleNameSidecarCheckpointV1> {
    // A parseable footer is not proof that its mappings still match durable authority.
    // Reopen and explicit synchronization validate the complete prefix; background
    // commits continue using incremental suffix scans.
    const committed = await this.#scanCommittedMappings(0)
    let checkpoint: CompatibleNameSidecarCheckpointV1
    let suffix: readonly CompatibleNameMappingV1[]
    try {
      checkpoint = decodeCompatibleNameSidecar(await this.#writer.readOwnedBytes())
      this.#assertCheckpointIdentity(checkpoint)
      const count = checkpoint.footer.committedCount
      if (count > committed.length ||
          !sidecarMappingsEqual(checkpoint.mappings, committed.slice(0, count).map(sidecarMapping))) {
        throw new CompatibleNameProjectorError('sidecar prefix disagrees with the committed ledger')
      }
      suffix = committed.slice(count)
    } catch (cause) {
      return this.#rebuildAndValidate(committed, requestedState, stage, cause)
    }

    this.#emit({
      stage,
      decision: 'checkpoint-observed',
      committedCount: checkpoint.footer.committedCount,
      footerState: checkpoint.footer.state,
    })

    if (checkpoint.footer.state !== 'active' &&
        (suffix.length !== 0 ||
          (requestedState !== 'active' && checkpoint.footer.state !== requestedState))) {
      throw new CompatibleNameProjectorError(
        'compatible-name terminal footer disagrees with durable ledger state',
      )
    }

    if (checkpoint.trailingByteLength > 0) {
      try {
        await this.#writer.truncateOwnedBytes(checkpoint.checkpointByteLength)
        checkpoint = await this.#readAndValidateExact(
          checkpoint.mappings,
          checkpoint.footer.state,
        )
        this.#emit({
          stage,
          decision: 'tail-truncated',
          committedCount: checkpoint.footer.committedCount,
          footerState: checkpoint.footer.state,
        })
      } catch (cause) {
        const rebuildState = checkpoint.footer.state === 'active'
          ? requestedState
          : checkpoint.footer.state
        const committed = await this.#scanCommittedMappings(0)
        return this.#rebuildAndValidate(committed, rebuildState, stage, cause)
      }
    }

    if (checkpoint.footer.state !== 'active') return checkpoint

    if (requestedState === 'active' && suffix.length === 0) return checkpoint
    const reconciled = await this.#appendAndValidate(checkpoint, suffix, requestedState)
    this.#emit({
      stage,
      decision: 'batch-appended',
      committedCount: reconciled.footer.committedCount,
      footerState: reconciled.footer.state,
      appendedCount: suffix.length,
    })
    return reconciled
  }

  async #appendAndValidate(
    checkpoint: CompatibleNameSidecarCheckpointV1,
    suffix: readonly CompatibleNameMappingV1[],
    footerState: CompatibleNameFooterState,
  ): Promise<CompatibleNameSidecarCheckpointV1> {
    const mappings = Object.freeze([
      ...checkpoint.mappings,
      ...suffix.map(sidecarMapping),
    ])
    const committedCount = mappings.length
    const bytes = TEXT_ENCODER.encode(
      suffix.map(mapping => encodeCompatibleNameSidecarMapping(sidecarMapping(mapping))).join('') +
      encodeCompatibleNameSidecarFooter({ committedCount, state: footerState }),
    )
    await this.#writer.appendOwnedCheckpoint(bytes)
    return this.#readAndValidateExact(mappings, footerState)
  }

  async #rebuildAndValidate(
    committed: readonly CompatibleNameMappingV1[],
    footerState: CompatibleNameFooterState,
    stage: 'restart' | 'terminal',
    cause: unknown,
  ): Promise<CompatibleNameSidecarCheckpointV1> {
    const mappings = committed.map(sidecarMapping)
    const bytes = TEXT_ENCODER.encode(
      encodeCompatibleNameSidecarHeader({
        operationId: this.#operationId,
        placement: this.#placement,
      }) +
      mappings.map(encodeCompatibleNameSidecarMapping).join('') +
      encodeCompatibleNameSidecarFooter({
        committedCount: mappings.length,
        state: footerState,
      }),
    )
    await this.#writer.replaceOwnedCheckpoint(bytes)
    const rebuilt = await this.#readAndValidateExact(mappings, footerState)
    this.#emit({
      stage,
      decision: 'sidecar-rebuilt',
      committedCount: rebuilt.footer.committedCount,
      footerState: rebuilt.footer.state,
      appendedCount: mappings.length,
      cause,
    })
    return rebuilt
  }

  async #readAndValidateExact(
    expectedMappings: readonly CompatibleNameSidecarMappingV1[],
    expectedState: CompatibleNameFooterState,
  ): Promise<CompatibleNameSidecarCheckpointV1> {
    const checkpoint = decodeCompatibleNameSidecar(await this.#writer.readOwnedBytes())
    this.#assertCheckpointIdentity(checkpoint)
    if (checkpoint.trailingByteLength !== 0 || checkpoint.footer.state !== expectedState ||
        checkpoint.footer.committedCount !== expectedMappings.length ||
        !sidecarMappingsEqual(checkpoint.mappings, expectedMappings)) {
      throw new CompatibleNameProjectorError(
        'compatible-name sidecar did not close with the expected checkpoint',
      )
    }
    return checkpoint
  }

  async #scanCommittedMappings(afterOrdinal: number): Promise<readonly CompatibleNameMappingV1[]> {
    const mappings = await this.#ledger.scanCommittedMappings(this.#operationId, afterOrdinal)
    let expectedOrdinal = afterOrdinal + 1
    for (const mapping of mappings) {
      if (mapping.operationId !== this.#operationId || mapping.commitState !== 'committed' ||
          mapping.ownershipState !== 'owned' || mapping.commitOrdinal !== expectedOrdinal) {
        throw new CompatibleNameProjectorError(
          'compatible-name ledger returned a non-contiguous committed mapping suffix',
        )
      }
      expectedOrdinal += 1
    }
    return mappings
  }

  #assertCheckpointIdentity(checkpoint: CompatibleNameSidecarCheckpointV1): void {
    if (checkpoint.header.operationId !== this.#operationId ||
        checkpoint.header.placement !== this.#placement) {
      throw new CompatibleNameProjectorError(
        'compatible-name sidecar header does not belong to this operation',
      )
    }
  }

  async #publishCheckpoint(
    observation: CompatibleNameFooterObservationV1 = this.observeFooter(),
  ): Promise<void> {
    await this.#checkpointed?.(observation)
  }

  #emit(
    event: Omit<CompatibleNameProjectorTraceEvent, 'operationId'>,
  ): void {
    if (this.#trace === undefined) return
    try {
      this.#trace(Object.freeze({ operationId: this.#operationId, ...event }))
    } catch {
      // Diagnostics must never become a sidecar mutation failure.
    }
  }
}

export async function createCompatibleNameProjector(
  options: CompatibleNameProjectorOptions,
): Promise<CompatibleNameProjector> {
  const projector = new CoalescingCompatibleNameProjector(options)
  await projector.initialize()
  return projector
}

function sidecarMapping(mapping: CompatibleNameMappingV1): CompatibleNameSidecarMappingV1 {
  if (mapping.commitOrdinal === undefined) {
    throw new CompatibleNameProjectorError('compatible-name mapping has no commit ordinal')
  }
  return Object.freeze({
    ordinal: mapping.commitOrdinal,
    entryKind: mapping.entryKind,
    logicalPath: mapping.logicalPath,
    physicalComponent: mapping.physicalComponent,
  })
}

function sidecarMappingsEqual(
  left: readonly CompatibleNameSidecarMappingV1[],
  right: readonly CompatibleNameSidecarMappingV1[],
): boolean {
  if (left.length !== right.length) return false
  return left.every((mapping, index) => {
    const expected = right[index]
    return expected !== undefined && mapping.ordinal === expected.ordinal &&
      mapping.entryKind === expected.entryKind &&
      mapping.physicalComponent === expected.physicalComponent &&
      mapping.logicalPath.length === expected.logicalPath.length &&
      mapping.logicalPath.every((component, pathIndex) =>
        component === expected.logicalPath[pathIndex])
  })
}

function assertTerminalFooterState(
  state: CompatibleNameTerminalFooterState,
): void {
  if (state !== 'completed' && state !== 'stopped' && state !== 'failed') {
    throw new CompatibleNameProjectorError(
      'compatible-name terminal projection requires a terminal footer state',
    )
  }
}
