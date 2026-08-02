import { Buffer } from 'node:buffer'
import { isProxy } from 'node:util/types'

import { createOwnedEventChannel } from '../../../browser-evidence/process/owned-process-channel.mjs'
import {
  LINUX_TOPOLOGY_TRACE_SCHEMA_VERSION,
  type LinuxTopologyTraceChannel,
  type LinuxTopologyTraceContextValue,
  type LinuxTopologyTraceEvent,
  type LinuxTopologyTraceIdentity,
  type LinuxTopologyTraceOutcome,
  type LinuxTopologyTraceSnapshot,
} from './contract.ts'

const MAXIMUM_TRACE_EVENTS = 4_096
const MAXIMUM_TRACE_BYTES = 16_777_216
const MAXIMUM_EVENT_BYTES = 32_768
const MAXIMUM_CONTEXT_ENTRIES = 64
const MAXIMUM_CONTEXT_STRING_BYTES = 8_192
const PORTABLE_ID = /^(?:[A-Za-z0-9]|[A-Za-z0-9][A-Za-z0-9._-]{0,254}[A-Za-z0-9])$/u
const PORTABLE_MILESTONE = /^[a-z0-9]+(?:-[a-z0-9]+)*$/u
const PORTABLE_CONTEXT_KEY = /^[A-Za-z][A-Za-z0-9]{0,63}$/u
const HOUSEKEEPING_MILESTONE = /(?:cleanup|close|rollback|termination|settlement)/u

interface TraceLifecycle {
  readonly identity: LinuxTopologyTraceIdentity
  terminal: boolean
  lastWorkflowMilestone: string
}

/**
 * The process owner is the only writer. Consumers receive only the pull view, so
 * terminal and cleanup evidence cannot be suppressed by a caller callback.
 */
export class LinuxTopologyTraceJournal {
  readonly #channel = createOwnedEventChannel<LinuxTopologyTraceEvent>(
    MAXIMUM_TRACE_EVENTS,
    'Linux topology trace',
  )
  readonly #lifecycles = new Map<string, TraceLifecycle>()
  #observedBytes = 0
  #capturedBytes = 0
  #capturedEvents = 0
  #finished = false

  readonly view: LinuxTopologyTraceChannel

  constructor() {
    const channel = this.#channel.view
    this.view = Object.freeze({
      snapshot: () => this.snapshot(),
      [Symbol.asyncIterator]: () => channel[Symbol.asyncIterator](),
    })
  }

  start(
    identity: LinuxTopologyTraceIdentity,
    milestone: string,
    context: Readonly<Record<string, unknown>> = Object.freeze({}),
  ): LinuxTopologyTraceIdentity {
    return this.#mutate(() => {
      const canonicalIdentity = canonicalTraceIdentity(identity)
      const canonicalMilestone = canonicalTraceMilestone(milestone)
      const key = lifecycleKey(canonicalIdentity)
      if (this.#lifecycles.has(key)) {
        throw new Error('Linux topology trace lifecycle started more than once')
      }
      this.#lifecycles.set(key, {
        identity: canonicalIdentity,
        terminal: false,
        lastWorkflowMilestone: canonicalMilestone,
      })
      this.#append(canonicalTraceEvent(
        canonicalIdentity,
        canonicalMilestone,
        'started',
        context,
      ))
      return canonicalIdentity
    })
  }

  progress(
    identity: LinuxTopologyTraceIdentity,
    milestone: string,
    outcome: LinuxTopologyTraceOutcome,
    context: Readonly<Record<string, unknown>> = Object.freeze({}),
  ): void {
    this.#mutate(() => {
      const lifecycle = this.#requireActive(identity)
      const canonicalMilestone = canonicalTraceMilestone(milestone)
      if (canonicalMilestone.endsWith('-terminal')) {
        throw new Error('Linux topology trace progress cannot claim a terminal milestone')
      }
      if (!HOUSEKEEPING_MILESTONE.test(canonicalMilestone)) {
        lifecycle.lastWorkflowMilestone = canonicalMilestone
      }
      this.#append(canonicalTraceEvent(
        lifecycle.identity,
        canonicalMilestone,
        canonicalTraceOutcome(outcome),
        context,
      ))
    })
  }

  terminal(
    identity: LinuxTopologyTraceIdentity,
    milestone: string,
    outcome: Exclude<LinuxTopologyTraceOutcome, 'started'>,
    cleanupOutcome: 'completed' | 'failed' | 'not-required',
  ): void {
    this.#mutate(() => {
      const lifecycle = this.#requireActive(identity)
      const canonicalMilestone = canonicalTraceMilestone(milestone)
      if (!canonicalMilestone.endsWith('-terminal')) {
        throw new Error('Linux topology trace terminal milestone is invalid')
      }
      if (
        cleanupOutcome !== 'completed' &&
        cleanupOutcome !== 'failed' &&
        cleanupOutcome !== 'not-required'
      ) throw new Error('Linux topology trace cleanup outcome is invalid')
      if (outcome === 'succeeded' && cleanupOutcome === 'failed') {
        throw new Error('Linux topology trace cannot succeed with failed cleanup')
      }
      lifecycle.terminal = true
      this.#append(canonicalTraceEvent(
        lifecycle.identity,
        canonicalMilestone,
        outcome,
        Object.freeze({
          cleanupOutcome,
          lastMilestone: lifecycle.lastWorkflowMilestone,
        }),
      ))
    })
  }

  finish(): void {
    if (this.#finished) {
      const failure = this.#channel.failure()
      if (failure !== undefined) throw failure
      return
    }
    this.#finished = true
    for (const lifecycle of this.#lifecycles.values()) {
      if (!lifecycle.terminal) {
        this.#fail(new Error('Linux topology trace lifecycle lacks its terminal event'))
      }
    }
    this.#channel.finish()
    const snapshot = this.#channel.view.snapshot()
    if (
      snapshot.truncated ||
      snapshot.observedEvents !== snapshot.capturedEvents ||
      this.#observedBytes !== this.#capturedBytes
    ) this.#fail(new Error('Linux topology trace did not retain its complete bounded evidence'))
    const failure = this.#channel.failure()
    if (failure !== undefined) throw failure
  }

  assertHealthy(): void {
    const failure = this.#channel.failure()
    if (failure !== undefined) throw failure
  }

  snapshot(): LinuxTopologyTraceSnapshot {
    const snapshot = this.#channel.view.snapshot()
    const failure = this.#channel.failure()
    return Object.freeze({
      ...snapshot,
      events: Object.freeze([...snapshot.events]),
      observedBytes: this.#observedBytes,
      capturedBytes: this.#capturedBytes,
      failure: failure === undefined
        ? null
        : Object.freeze({
            name: 'LinuxTopologyTraceError',
            message: 'Linux topology trace evidence is invalid',
          }),
    })
  }

  #requireActive(identity: LinuxTopologyTraceIdentity): TraceLifecycle {
    const canonicalIdentity = canonicalTraceIdentity(identity)
    const lifecycle = this.#lifecycles.get(lifecycleKey(canonicalIdentity))
    if (lifecycle === undefined) {
      throw new Error('Linux topology trace progress preceded its lifecycle start')
    }
    if (!sameIdentity(lifecycle.identity, canonicalIdentity)) {
      throw new Error('Linux topology trace lifecycle identity changed')
    }
    if (lifecycle.terminal) {
      throw new Error('Linux topology trace progress followed its terminal event')
    }
    return lifecycle
  }

  #append(event: LinuxTopologyTraceEvent): void {
    const bytes = Buffer.byteLength(JSON.stringify(event), 'utf8')
    this.#observedBytes = boundedCount(this.#observedBytes, bytes)
    if (bytes > MAXIMUM_EVENT_BYTES || this.#capturedBytes > MAXIMUM_TRACE_BYTES - bytes) {
      throw new Error('Linux topology trace exceeded its byte capture authority')
    }
    const retained = this.#capturedEvents < MAXIMUM_TRACE_EVENTS
    this.#channel.append(event)
    if (retained) {
      this.#capturedEvents += 1
      this.#capturedBytes += bytes
    }
    const failure = this.#channel.failure()
    if (failure !== undefined) throw failure
  }

  #mutate<T>(action: () => T): T {
    if (this.#finished) {
      const failure = new Error('Linux topology trace received evidence after completion')
      this.#fail(failure)
      throw failure
    }
    try {
      return action()
    } catch (cause) {
      const failure = new Error('Linux topology trace rejected invalid evidence', { cause })
      this.#fail(failure)
      throw failure
    }
  }

  #fail(cause: unknown): void {
    this.#channel.fail(new Error('Linux topology trace evidence is invalid', { cause }))
  }

}

export async function settleLinuxTopologyTraceJournal<T>(
  operation: Promise<T>,
  journal: LinuxTopologyTraceJournal,
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
      'Linux topology operation and trace settlement both failed',
      { cause: settled.cause },
    )
  }
  if (settled.outcome === 'failed') throw settled.cause
  if (traceFailure !== undefined) throw traceFailure
  return settled.value
}

export function requireCompleteLinuxTopologyTrace(
  snapshot: LinuxTopologyTraceSnapshot,
  label = 'Linux topology trace',
): void {
  if (
    !snapshot.completed ||
    snapshot.truncated ||
    snapshot.failure !== null ||
    snapshot.observedEvents !== snapshot.capturedEvents ||
    snapshot.observedBytes !== snapshot.capturedBytes ||
    snapshot.events.length !== snapshot.capturedEvents ||
    !Object.isFrozen(snapshot) ||
    !Object.isFrozen(snapshot.events)
  ) throw new Error(`${label} is incomplete`)
}

function canonicalTraceIdentity(value: LinuxTopologyTraceIdentity): LinuxTopologyTraceIdentity {
  rejectProxy(value, 'Linux topology trace identity')
  requirePlainRecord(value, 'Linux topology trace identity')
  const descriptors = exactDataProperties(value, [
    'component', 'scenario', 'operationId', 'runId', 'profileId', 'browser', 'sampleOrdinal',
  ], 'Linux topology trace identity')
  const component = descriptors.component?.value
  const scenario = descriptors.scenario?.value
  if (
    component !== 'contained-browser-broker' &&
    component !== 'credential-broker-process-owner'
  ) throw new Error('Linux topology trace component is invalid')
  if (
    scenario !== 'contained-browser-sample' &&
    scenario !== 'credential-broker-exchange'
  ) throw new Error('Linux topology trace scenario is invalid')
  if (
    component === 'contained-browser-broker' !==
    (scenario === 'contained-browser-sample')
  ) throw new Error('Linux topology trace component and scenario disagree')
  const operationId = canonicalPortableId(descriptors.operationId?.value, 'operation ID')
  const runId = canonicalPortableId(descriptors.runId?.value, 'run ID')
  const profileId = descriptors.profileId?.value
  if (
    profileId !== 'scheduled-public-stun' &&
    profileId !== 'scheduled-restricted-udp' &&
    profileId !== 'scheduled-coturn'
  ) throw new Error('Linux topology trace profile is invalid')
  const browser = descriptors.browser?.value
  if (browser !== 'chromium' && browser !== 'firefox' && browser !== 'webkit') {
    throw new Error('Linux topology trace browser is invalid')
  }
  const sampleOrdinal = descriptors.sampleOrdinal?.value
  if (!Number.isSafeInteger(sampleOrdinal) || (sampleOrdinal as number) < 1) {
    throw new Error('Linux topology trace sample ordinal is invalid')
  }
  return Object.freeze({
    component,
    scenario,
    operationId,
    runId,
    profileId,
    browser,
    sampleOrdinal: sampleOrdinal as number,
  })
}

function canonicalTraceEvent(
  identity: LinuxTopologyTraceIdentity,
  milestone: string,
  outcome: LinuxTopologyTraceOutcome,
  context: Readonly<Record<string, unknown>>,
): LinuxTopologyTraceEvent {
  return Object.freeze({
    schemaVersion: LINUX_TOPOLOGY_TRACE_SCHEMA_VERSION,
    ...identity,
    milestone,
    outcome: canonicalTraceOutcome(outcome),
    context: canonicalTraceContext(context),
  })
}

function canonicalTraceOutcome(value: unknown): LinuxTopologyTraceOutcome {
  if (value !== 'started' && value !== 'succeeded' && value !== 'failed') {
    throw new Error('Linux topology trace outcome is invalid')
  }
  return value
}

function canonicalTraceMilestone(value: unknown): string {
  if (typeof value !== 'string' || !PORTABLE_MILESTONE.test(value) || value.length > 128) {
    throw new Error('Linux topology trace milestone is invalid')
  }
  return value
}

function canonicalTraceContext(
  value: Readonly<Record<string, unknown>>,
): Readonly<Record<string, LinuxTopologyTraceContextValue>> {
  rejectProxy(value, 'Linux topology trace context')
  requirePlainRecord(value, 'Linux topology trace context')
  const descriptors = Object.getOwnPropertyDescriptors(value)
  if (
    Reflect.ownKeys(value).some((key) => typeof key !== 'string') ||
    Object.keys(descriptors).length > MAXIMUM_CONTEXT_ENTRIES
  ) throw new Error('Linux topology trace context shape is invalid')
  const result = Object.create(null) as Record<string, LinuxTopologyTraceContextValue>
  for (const [key, descriptor] of Object.entries(descriptors)) {
    if (
      !PORTABLE_CONTEXT_KEY.test(key) ||
      !descriptor.enumerable ||
      !('value' in descriptor)
    ) throw new Error('Linux topology trace context property is invalid')
    const entry = descriptor.value
    if (typeof entry === 'string') {
      if (Buffer.byteLength(entry, 'utf8') > MAXIMUM_CONTEXT_STRING_BYTES) {
        throw new Error('Linux topology trace context string exceeds its byte limit')
      }
    } else if (
      entry !== null &&
      typeof entry !== 'boolean' &&
      (typeof entry !== 'number' || !Number.isSafeInteger(entry))
    ) throw new Error('Linux topology trace context value is invalid')
    Object.defineProperty(result, key, {
      value: entry,
      enumerable: true,
      configurable: false,
      writable: false,
    })
  }
  return Object.freeze(result)
}

function exactDataProperties(
  value: object,
  expected: readonly string[],
  label: string,
): Record<string, PropertyDescriptor> {
  const keys = Reflect.ownKeys(value)
  if (
    keys.length !== expected.length ||
    keys.some((key) => typeof key !== 'string' || !expected.includes(key))
  ) throw new Error(`${label} shape is invalid`)
  const descriptors = Object.getOwnPropertyDescriptors(value)
  for (const key of expected) {
    const descriptor = descriptors[key]
    if (
      descriptor === undefined ||
      !descriptor.enumerable ||
      !('value' in descriptor)
    ) throw new Error(`${label} property is invalid`)
  }
  return descriptors
}

function canonicalPortableId(value: unknown, label: string): string {
  if (typeof value !== 'string' || !PORTABLE_ID.test(value)) {
    throw new Error(`Linux topology trace ${label} is invalid`)
  }
  return value
}

function sameIdentity(
  left: LinuxTopologyTraceIdentity,
  right: LinuxTopologyTraceIdentity,
): boolean {
  return left.component === right.component &&
    left.scenario === right.scenario &&
    left.operationId === right.operationId &&
    left.runId === right.runId &&
    left.profileId === right.profileId &&
    left.browser === right.browser &&
    left.sampleOrdinal === right.sampleOrdinal
}

function lifecycleKey(identity: LinuxTopologyTraceIdentity): string {
  return `${identity.runId}|${identity.operationId}`
}

function boundedCount(current: number, additional: number): number {
  if (
    !Number.isSafeInteger(additional) ||
    additional < 0 ||
    current > Number.MAX_SAFE_INTEGER - additional
  ) throw new Error('Linux topology trace byte count exceeded the safe integer range')
  return current + additional
}

function rejectProxy(value: unknown, label: string): void {
  if ((typeof value === 'object' || typeof value === 'function') && value !== null && isProxy(value)) {
    throw new Error(`${label} cannot be a Proxy`)
  }
}

function requirePlainRecord(value: unknown, label: string): asserts value is object {
  if (
    typeof value !== 'object' ||
    value === null ||
    Array.isArray(value) ||
    Object.getPrototypeOf(value) !== Object.prototype
  ) throw new Error(`${label} must be a plain object`)
}
