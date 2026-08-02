import { Buffer } from 'node:buffer'

import { createOwnedEventChannel } from '../../process/owned-process-channel.mjs'
import {
  ARTIFACT_GUARD_TRACE_SCHEMA_VERSION,
  type ArtifactGuardTraceChannel,
  type ArtifactGuardTraceContextValue,
  type ArtifactGuardTraceEvent,
  type ArtifactGuardTraceFailure,
  type ArtifactGuardTraceIdentity,
  type ArtifactGuardTraceOutcome,
  type ArtifactGuardTraceSnapshot,
} from './contract.ts'

const MAXIMUM_TRACE_EVENTS = 256
const MAXIMUM_TRACE_BYTES = 8_388_608
const MAXIMUM_CONTEXT_ENTRIES = 32
const MAXIMUM_CONTEXT_STRING_BYTES = 1_024
const PORTABLE_IDENTITY = /^(?:[A-Za-z0-9]|[A-Za-z0-9][A-Za-z0-9._\/-]{0,510}[A-Za-z0-9])$/u
const PORTABLE_CONTEXT_KEY = /^[A-Za-z][A-Za-z0-9]{0,63}$/u
const CLEANUP_MILESTONE = /(?:cleanup|close|rollback)/iu
const OWNED_ARTIFACT_TRACE_SNAPSHOTS = new WeakSet<object>()

interface LifecycleState {
  readonly identity: ArtifactGuardTraceIdentity
  terminal: boolean
}

export class ArtifactGuardTraceJournal {
  readonly #channel = createOwnedEventChannel<ArtifactGuardTraceEvent>(
    MAXIMUM_TRACE_EVENTS,
    'artifact guard trace',
  )
  readonly #lifecycles = new Map<string, LifecycleState>()
  #observedBytes = 0
  #capturedBytes = 0
  #finished = false

  readonly view: ArtifactGuardTraceChannel

  constructor() {
    const channel = this.#channel.view
    const journal = this
    this.view = Object.freeze({
      snapshot: () => journal.snapshot(),
      [Symbol.asyncIterator]: () => channel[Symbol.asyncIterator](),
    })
  }

  start(identity: ArtifactGuardTraceIdentity, context: ArtifactGuardTraceEvent['context']): void {
    this.#mutate(() => {
      const checkedIdentity = canonicalIdentity(identity)
      if (this.#lifecycles.has(checkedIdentity.operationId)) {
        throw new Error(`artifact guard trace operation ${checkedIdentity.operationId} started more than once`)
      }
      this.#lifecycles.set(checkedIdentity.operationId, {
        identity: checkedIdentity,
        terminal: false,
      })
      this.#append(canonicalEvent(
        checkedIdentity,
        checkedIdentity.scenario === 'guard-suite' ? 'suite-started' : 'scan-started',
        'started',
        context,
      ))
    })
  }

  progress(
    identity: ArtifactGuardTraceIdentity,
    milestone: string,
    outcome: Exclude<ArtifactGuardTraceOutcome, 'started'>,
    context: ArtifactGuardTraceEvent['context'],
  ): void {
    this.#mutate(() => {
      const lifecycle = this.#requireActive(identity)
      if (milestone === 'suite-started' || milestone === 'scan-started' || milestone.endsWith('-terminal')) {
        throw new Error('artifact guard trace progress cannot use a lifecycle milestone')
      }
      this.#append(canonicalEvent(lifecycle.identity, milestone, outcome, context))
    })
  }

  terminal(
    identity: ArtifactGuardTraceIdentity,
    outcome: Exclude<ArtifactGuardTraceOutcome, 'started'>,
    cleanupOutcome: 'completed' | 'failed' | 'not-required',
    lastMilestone: string,
    context: ArtifactGuardTraceEvent['context'] = Object.freeze({}),
  ): void {
    this.#mutate(() => {
      const lifecycle = this.#requireActive(identity)
      if (CLEANUP_MILESTONE.test(lastMilestone)) {
        throw new Error('artifact guard terminal last milestone must describe workflow progress')
      }
      lifecycle.terminal = true
      this.#append(canonicalEvent(
        lifecycle.identity,
        lifecycle.identity.scenario === 'guard-suite' ? 'suite-terminal' : 'scan-terminal',
        outcome,
        {
          ...context,
          cleanupOutcome,
          lastMilestone,
        },
      ))
    })
  }

  replay(snapshot: ArtifactGuardTraceSnapshot): void {
    this.#mutate(() => {
      requireCompleteSnapshot(snapshot, 'child artifact scan trace')
      for (const event of snapshot.events) this.#replayEvent(event)
    })
  }

  finish(): void {
    if (this.#finished) {
      this.#channel.fail(new Error('artifact guard trace journal finished more than once'))
      return
    }
    this.#finished = true
    if (this.#lifecycles.size === 0) {
      this.#channel.fail(new Error('artifact guard trace journal has no lifecycle start'))
    }
    for (const lifecycle of this.#lifecycles.values()) {
      if (!lifecycle.terminal) {
        this.#channel.fail(new Error(
          `artifact guard trace operation ${lifecycle.identity.operationId} has no terminal event`,
        ))
      }
    }
    this.#channel.finish()
  }

  failure(): Error | undefined {
    return this.#channel.failure()
  }

  snapshot(): ArtifactGuardTraceSnapshot {
    const base = this.#channel.view.snapshot()
    const failure = this.#channel.failure()
    const snapshot = Object.freeze({
      ...base,
      events: Object.freeze([...base.events]),
      observedBytes: this.#observedBytes,
      capturedBytes: this.#capturedBytes,
      failure: failure === undefined ? null : traceFailure(failure),
    })
    OWNED_ARTIFACT_TRACE_SNAPSHOTS.add(snapshot)
    return snapshot
  }

  #replayEvent(candidate: ArtifactGuardTraceEvent): void {
    const identity = canonicalIdentity(candidate)
    const event = canonicalEvent(identity, candidate.milestone, candidate.outcome, candidate.context)
    const startMilestone = identity.scenario === 'guard-suite' ? 'suite-started' : 'scan-started'
    const terminalMilestone = identity.scenario === 'guard-suite' ? 'suite-terminal' : 'scan-terminal'
    if (event.milestone === startMilestone) {
      if (event.outcome !== 'started') throw new Error('artifact guard trace start outcome is invalid')
      if (this.#lifecycles.has(identity.operationId)) {
        throw new Error(`artifact guard trace operation ${identity.operationId} started more than once`)
      }
      this.#lifecycles.set(identity.operationId, { identity, terminal: false })
    } else if (event.milestone === terminalMilestone) {
      const lifecycle = this.#requireActive(identity)
      requireTerminalContext(event)
      lifecycle.terminal = true
    } else {
      this.#requireActive(identity)
      if (event.outcome === 'started') {
        throw new Error('artifact guard trace reserves the started outcome for lifecycle starts')
      }
    }
    this.#append(event)
  }

  #requireActive(identity: ArtifactGuardTraceIdentity): LifecycleState {
    const checked = canonicalIdentity(identity)
    const lifecycle = this.#lifecycles.get(checked.operationId)
    if (lifecycle === undefined) {
      throw new Error(`artifact guard trace operation ${checked.operationId} emitted before start`)
    }
    if (!sameIdentity(lifecycle.identity, checked)) {
      throw new Error(`artifact guard trace operation ${checked.operationId} changed identity`)
    }
    if (lifecycle.terminal) {
      throw new Error(`artifact guard trace operation ${checked.operationId} emitted after terminal`)
    }
    return lifecycle
  }

  #append(event: ArtifactGuardTraceEvent): void {
    const byteLength = Buffer.byteLength(JSON.stringify(event), 'utf8')
    this.#observedBytes = boundedCount(this.#observedBytes, byteLength)
    if (this.#capturedBytes <= MAXIMUM_TRACE_BYTES - byteLength) {
      this.#capturedBytes += byteLength
      this.#channel.append(event)
    } else {
      this.#channel.fail(new Error(
        `artifact guard trace exceeded its ${MAXIMUM_TRACE_BYTES}-byte capture authority`,
      ))
    }
  }

  #mutate(action: () => void): void {
    if (this.#finished) {
      this.#channel.fail(new Error('artifact guard trace journal received evidence after completion'))
      return
    }
    try {
      action()
    } catch (cause) {
      this.#channel.fail(cause)
    }
  }
}

export function requireCompleteArtifactGuardTrace(
  snapshot: ArtifactGuardTraceSnapshot,
  label = 'artifact guard trace',
): void {
  requireCompleteSnapshot(snapshot, label)
}

function requireCompleteSnapshot(snapshot: ArtifactGuardTraceSnapshot, label: string): void {
  const trustedLabel = typeof label === 'string' && label.length > 0 && label.length <= 128
    ? label
    : 'artifact guard trace'
  if (
    (typeof snapshot !== 'object' && typeof snapshot !== 'function') ||
    snapshot === null ||
    !OWNED_ARTIFACT_TRACE_SNAPSHOTS.has(snapshot)
  ) throw new Error(`${trustedLabel} does not come from its lifecycle owner`)
  if (
    !snapshot.completed ||
    snapshot.truncated ||
    snapshot.failure !== null ||
    snapshot.observedEvents !== snapshot.capturedEvents ||
    snapshot.observedBytes !== snapshot.capturedBytes
  ) throw new Error(`${trustedLabel} is incomplete`)
}

function canonicalIdentity(value: ArtifactGuardTraceIdentity): ArtifactGuardTraceIdentity {
  requirePortable(value.operationId, 'trace operation ID')
  requirePortable(value.runId, 'trace run ID')
  if (value.scenario !== 'guard-suite' && value.scenario !== 'artifact-scan') {
    throw new Error('artifact guard trace scenario is invalid')
  }
  if (value.suite !== 'main' && value.suite !== 'pion') {
    throw new Error('artifact guard trace suite is invalid')
  }
  if (value.scenario === 'artifact-scan') {
    if (value.browser !== 'chromium' && value.browser !== 'firefox' && value.browser !== 'webkit') {
      throw new Error('artifact guard scan trace browser is invalid')
    }
    if (!Number.isSafeInteger(value.sampleIndex) || value.sampleIndex! < 1) {
      throw new Error('artifact guard scan trace sample index is invalid')
    }
  } else if (value.browser !== undefined || value.sampleIndex !== undefined) {
    throw new Error('artifact guard suite trace cannot claim a sample identity')
  }
  return Object.freeze({
    operationId: value.operationId,
    runId: value.runId,
    scenario: value.scenario,
    suite: value.suite,
    ...(value.browser === undefined ? {} : { browser: value.browser }),
    ...(value.sampleIndex === undefined ? {} : { sampleIndex: value.sampleIndex }),
  })
}

function canonicalEvent(
  identity: ArtifactGuardTraceIdentity,
  milestone: string,
  outcome: ArtifactGuardTraceOutcome,
  context: ArtifactGuardTraceEvent['context'],
): ArtifactGuardTraceEvent {
  requirePortable(milestone, 'trace milestone')
  if (!['started', 'succeeded', 'failed', 'blocked'].includes(outcome)) {
    throw new Error('artifact guard trace outcome is invalid')
  }
  return Object.freeze({
    schemaVersion: ARTIFACT_GUARD_TRACE_SCHEMA_VERSION,
    component: 'artifact-guard',
    ...identity,
    milestone,
    outcome,
    context: canonicalContext(context),
  })
}

function canonicalContext(
  value: Readonly<Record<string, ArtifactGuardTraceContextValue>>,
): Readonly<Record<string, ArtifactGuardTraceContextValue>> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error('artifact guard trace context must be an object')
  }
  const descriptors = Object.getOwnPropertyDescriptors(value)
  if (
    Reflect.ownKeys(value).some((key) => typeof key !== 'string') ||
    Object.keys(descriptors).length > MAXIMUM_CONTEXT_ENTRIES
  ) throw new Error('artifact guard trace context shape is invalid')
  const result: Record<string, ArtifactGuardTraceContextValue> = {}
  for (const [key, descriptor] of Object.entries(descriptors)) {
    if (!PORTABLE_CONTEXT_KEY.test(key) || !descriptor.enumerable || !('value' in descriptor)) {
      throw new Error('artifact guard trace context property is invalid')
    }
    const entry = descriptor.value
    if (typeof entry === 'string') {
      if (Buffer.byteLength(entry, 'utf8') > MAXIMUM_CONTEXT_STRING_BYTES) {
        throw new Error('artifact guard trace context string exceeds its byte limit')
      }
    } else if (
      entry !== null &&
      typeof entry !== 'boolean' &&
      (typeof entry !== 'number' || !Number.isSafeInteger(entry))
    ) {
      throw new Error('artifact guard trace context value is not canonical')
    }
    result[key] = entry
  }
  return Object.freeze(result)
}

function requireTerminalContext(event: ArtifactGuardTraceEvent): void {
  if (
    typeof event.context.cleanupOutcome !== 'string' ||
    !['completed', 'failed', 'not-required'].includes(event.context.cleanupOutcome) ||
    typeof event.context.lastMilestone !== 'string' ||
    CLEANUP_MILESTONE.test(event.context.lastMilestone)
  ) throw new Error('artifact guard terminal trace context is invalid')
}

function sameIdentity(left: ArtifactGuardTraceIdentity, right: ArtifactGuardTraceIdentity): boolean {
  return left.operationId === right.operationId &&
    left.runId === right.runId &&
    left.scenario === right.scenario &&
    left.suite === right.suite &&
    left.browser === right.browser &&
    left.sampleIndex === right.sampleIndex
}

function requirePortable(value: string, label: string): void {
  if (!PORTABLE_IDENTITY.test(value)) throw new Error(`${label} is not portable`)
}

function boundedCount(current: number, additional: number): number {
  if (current > Number.MAX_SAFE_INTEGER - additional) {
    throw new Error('artifact guard trace observed byte count exceeded the safe integer range')
  }
  return current + additional
}

function traceFailure(cause: Error): ArtifactGuardTraceFailure {
  return Object.freeze({
    name: cause.name.normalize('NFC').slice(0, 128) || 'Error',
    message: cause.message.normalize('NFC').slice(0, 1_024) || 'artifact guard trace failed',
  })
}
