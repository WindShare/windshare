import { isProxy } from 'node:util/types'

import { createOwnedEventChannel } from '../../browser-evidence/process/owned-process-channel.mjs'
import {
  NETWORK_MATRIX_TRACE_LIFECYCLE_MILESTONES,
  canonicalNetworkMatrixTraceEvent,
  canonicalNetworkMatrixTraceIdentity,
  networkMatrixTraceLifecycleKey,
  networkMatrixTraceUtf8Bytes,
  requireNetworkMatrixTraceTerminalContext,
  type NetworkMatrixTraceChannel,
  type NetworkMatrixTraceEvent,
  type NetworkMatrixTraceIdentity,
  type NetworkMatrixTraceJournal,
  type NetworkMatrixTraceSnapshot,
} from './contract.ts'

const MAXIMUM_TRACE_JOURNAL_BYTES = 67_108_864
const HOUSEKEEPING_MILESTONE_PATTERN = /(?:cleanup|close|rollback|ownership-retry|settlement)/u

interface TraceLifecycleState {
  started: boolean
  terminal: boolean
  lastWorkflowMilestone: string | null
}

/**
 * The expected lifecycle set is constructor-owned. Execution can therefore prove
 * that suppressed work was explicitly terminalized instead of silently shrinking
 * the evidence universe after a failure.
 */
export function createNetworkMatrixTraceJournal(
  expectedIdentities: readonly NetworkMatrixTraceIdentity[],
  maximumEvents: number,
  maximumBytes: number,
  label: string,
): NetworkMatrixTraceJournal {
  requirePositiveInteger(maximumEvents, 'network matrix trace journal event limit')
  requirePositiveInteger(maximumBytes, 'network matrix trace journal byte limit')
  if (maximumBytes > MAXIMUM_TRACE_JOURNAL_BYTES) {
    throw new Error('network matrix trace journal byte limit exceeds its safe authority')
  }
  if (typeof label !== 'string' || label.length < 1 || label.length > 256) {
    throw new Error('network matrix trace journal label is invalid')
  }
  const owner = new OwnedNetworkMatrixTraceJournal(
    expectedIdentities,
    maximumEvents,
    maximumBytes,
    label,
  )
  return Object.freeze({
    view: owner.view,
    append: (event: NetworkMatrixTraceEvent) => owner.append(event),
    finish: () => owner.finish(),
  })
}

/**
 * Settlement owns both failure domains so an operational failure cannot hide
 * incomplete evidence, and evidence failure cannot replace the original cause.
 */
export async function settleNetworkMatrixTraceJournal<T>(
  operation: Promise<T>,
  journal: Pick<NetworkMatrixTraceJournal, 'finish'>,
): Promise<T> {
  const settled = await operation.then(
    (value) => Object.freeze({ outcome: 'succeeded' as const, value }),
    (cause: unknown) => Object.freeze({ outcome: 'failed' as const, cause }),
  )
  let traceFailure: unknown
  try {
    journal.finish()
  } catch (cause) {
    traceFailure = cause
  }
  if (settled.outcome === 'failed' && traceFailure !== undefined) {
    throw new AggregateError(
      [settled.cause, traceFailure],
      'network matrix operation and trace settlement both failed',
      { cause: settled.cause },
    )
  }
  if (settled.outcome === 'failed') throw settled.cause
  if (traceFailure !== undefined) throw traceFailure
  return settled.value
}

class OwnedNetworkMatrixTraceJournal implements NetworkMatrixTraceJournal {
  readonly #channel
  readonly #maximumBytes: number
  readonly #label: string
  readonly #lifecycles = new Map<string, TraceLifecycleState>()
  readonly #operationIdentities = new Map<string, string>()
  #observedEvents = 0
  #observedBytes = 0
  #capturedBytes = 0
  #failure: Error | undefined
  #finished = false

  constructor(
    expectedIdentities: readonly NetworkMatrixTraceIdentity[],
    maximumEvents: number,
    maximumBytes: number,
    label: string,
  ) {
    if (
      isProxy(expectedIdentities) ||
      !Array.isArray(expectedIdentities) ||
      expectedIdentities.length === 0
    ) {
      throw new Error(`${label} requires at least one expected lifecycle identity`)
    }
    this.#channel = createOwnedEventChannel<NetworkMatrixTraceEvent>(maximumEvents, label)
    this.#maximumBytes = maximumBytes
    this.#label = label
    for (const identity of expectedIdentities) this.#registerExpectedIdentity(identity)
  }

  get view(): NetworkMatrixTraceChannel {
    const journal = this
    const channel = this.#channel.view
    return Object.freeze({
      snapshot: () => journal.#snapshot(),
      [Symbol.asyncIterator]: () => channel[Symbol.asyncIterator](),
    })
  }

  append(event: NetworkMatrixTraceEvent): void {
    this.#observedEvents += 1
    if (this.#finished) {
      this.#recordFailure(new Error(`${this.#label} received an event after terminal closure`))
      return
    }
    let canonical: NetworkMatrixTraceEvent
    let encodedBytes: number
    try {
      canonical = canonicalNetworkMatrixTraceEvent(event)
      encodedBytes = networkMatrixTraceUtf8Bytes(canonical)
      this.#observeLifecycle(canonical)
    } catch (cause) {
      this.#recordFailure(asError(cause, `${this.#label} rejected a malformed event`))
      return
    }
    this.#observedBytes = boundedCount(this.#observedBytes, encodedBytes, this.#label)
    if (this.#observedBytes > this.#maximumBytes) {
      this.#recordFailure(new Error(
        `${this.#label} exceeded its ${this.#maximumBytes}-byte capture authority`,
      ))
      return
    }
    this.#capturedBytes += encodedBytes
    this.#channel.append(canonical)
  }

  finish(): void {
    if (this.#finished) {
      if (this.#failure !== undefined) throw this.#failure
      return
    }
    this.#finished = true
    for (const [identity, state] of this.#lifecycles) {
      if (!state.started || !state.terminal) {
        this.#recordFailure(new Error(
          `${this.#label} lifecycle ${identity} did not publish exactly one start and terminal`,
        ))
      }
    }
    this.#channel.finish()
    const snapshot = this.#channel.view.snapshot()
    if (
      snapshot.truncated ||
      snapshot.observedEvents !== snapshot.capturedEvents ||
      this.#observedEvents !== snapshot.capturedEvents ||
      this.#observedBytes !== this.#capturedBytes
    ) {
      this.#recordFailure(new Error(
        `${this.#label} did not retain its complete bounded evidence`,
      ))
    }
    if (this.#failure !== undefined) throw this.#failure
  }

  #registerExpectedIdentity(identity: NetworkMatrixTraceIdentity): void {
    const canonical = canonicalNetworkMatrixTraceIdentity(identity)
    const lifecycleKey = networkMatrixTraceLifecycleKey(canonical)
    if (this.#lifecycles.has(lifecycleKey)) {
      throw new Error('network matrix trace lifecycle identity was expected twice')
    }
    const operationKey = `${canonical.runId}|${canonical.operationId}`
    if (this.#operationIdentities.has(operationKey)) {
      throw new Error('network matrix trace operation ID is not unique within its run')
    }
    this.#operationIdentities.set(operationKey, lifecycleKey)
    this.#lifecycles.set(lifecycleKey, {
      started: false,
      terminal: false,
      lastWorkflowMilestone: null,
    })
  }

  #observeLifecycle(event: NetworkMatrixTraceEvent): void {
    const milestones = NETWORK_MATRIX_TRACE_LIFECYCLE_MILESTONES[event.scenario]
    const key = networkMatrixTraceLifecycleKey(event)
    const existing = this.#lifecycles.get(key)
    if (event.milestone === milestones.start) {
      if (event.outcome !== 'started' || existing === undefined || existing.started) {
        throw new Error('network matrix trace lifecycle published an unexpected or duplicate start')
      }
      existing.started = true
      existing.lastWorkflowMilestone = event.milestone
      return
    }
    if (existing === undefined || !existing.started) {
      throw new Error('network matrix trace lifecycle published progress before its expected start')
    }
    if (existing.terminal) {
      throw new Error('network matrix trace lifecycle published an event after its terminal')
    }
    if (event.milestone === milestones.terminal) {
      if (event.outcome === 'started') {
        throw new Error('network matrix trace lifecycle terminal cannot remain started')
      }
      requireNetworkMatrixTraceTerminalContext(event)
      if (event.context?.lastMilestone !== existing.lastWorkflowMilestone) {
        throw new Error('network matrix trace terminal last milestone differs from journal-observed workflow progress')
      }
      if (event.outcome === 'succeeded' && event.context?.cleanupOutcome === 'failed') {
        throw new Error('network matrix trace succeeded terminal contradicts failed cleanup')
      }
      existing.terminal = true
      return
    }
    if (!HOUSEKEEPING_MILESTONE_PATTERN.test(event.milestone)) {
      existing.lastWorkflowMilestone = event.milestone
    }
  }

  #recordFailure(cause: Error): void {
    this.#failure ??= cause
    this.#channel.fail(cause)
  }

  #snapshot(): NetworkMatrixTraceSnapshot {
    const snapshot = this.#channel.view.snapshot()
    return Object.freeze({
      events: snapshot.events,
      observedEvents: this.#observedEvents,
      capturedEvents: snapshot.capturedEvents,
      observedBytes: this.#observedBytes,
      capturedBytes: this.#capturedBytes,
      truncated:
        snapshot.truncated ||
        this.#observedEvents !== snapshot.capturedEvents ||
        this.#observedBytes !== this.#capturedBytes,
      completed: snapshot.completed,
      failure: this.#failure === undefined
        ? null
        : Object.freeze({
            name: this.#failure.name.slice(0, 128),
            message: this.#failure.message.slice(0, 512),
          }),
    })
  }
}

function requirePositiveInteger(value: number, label: string): void {
  if (!Number.isSafeInteger(value) || value < 1) throw new Error(`${label} must be positive`)
}

function boundedCount(current: number, additional: number, label: string): number {
  if (current > Number.MAX_SAFE_INTEGER - additional) {
    throw new Error(`${label} byte count exceeded the safe integer range`)
  }
  return current + additional
}

function asError(value: unknown, fallback: string): Error {
  // The rejected value remains an opaque cause; the journal snapshot only reads
  // this framework-owned wrapper and can never invoke dependency accessors.
  return new Error(fallback, { cause: value })
}
