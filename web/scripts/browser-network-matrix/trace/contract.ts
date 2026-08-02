import { isProxy } from 'node:util/types'

import type { NetworkMatrixIdentity } from '../manifest.ts'
import { requireOperationId, requireRunId } from '../contract-support.ts'
import { NETWORK_MATRIX_BROWSERS, NETWORK_MATRIX_PROFILE_IDS } from '../vocabulary.ts'
import type { OwnedEventSnapshot } from '../../browser-evidence/process/owned-process-channel.mjs'

export const NETWORK_MATRIX_TRACE_SCHEMA_VERSION =
  'windshare.browser-network-matrix-trace/v1' as const
export const NETWORK_MATRIX_MAXIMUM_TRACE_EVENT_BYTES = 32_768 as const
export const NETWORK_MATRIX_MAXIMUM_TRACE_CONTEXT_DEPTH = 12 as const
export const NETWORK_MATRIX_MAXIMUM_TRACE_CONTEXT_ENTRIES = 256 as const
export const NETWORK_MATRIX_MAXIMUM_TRACE_STRING_BYTES = 8_192 as const
export const NETWORK_MATRIX_MAXIMUM_TRACE_KEY_BYTES = 256 as const

const PORTABLE_MILESTONE_PATTERN = /^[a-z0-9]+(?:-[a-z0-9]+)*$/u
const NETWORK_MATRIX_TRACE_COMPONENTS = Object.freeze([
  'browser-network-matrix-runner',
  'browser-network-matrix-execute',
] as const)
const NETWORK_MATRIX_TRACE_SCENARIOS = Object.freeze([
  'network-matrix-run',
  'network-matrix-profile',
  'network-matrix-sample',
  'network-matrix-execution',
] as const)
const NETWORK_MATRIX_TRACE_OUTCOMES = Object.freeze([
  'started',
  'succeeded',
  'failed',
  'skipped',
] as const)
export const NETWORK_MATRIX_TRACE_LIFECYCLE_MILESTONES = Object.freeze({
  'network-matrix-run': Object.freeze({ start: 'run-started', terminal: 'run-terminal' }),
  'network-matrix-profile': Object.freeze({ start: 'profile-started', terminal: 'profile-terminal' }),
  'network-matrix-sample': Object.freeze({ start: 'sample-started', terminal: 'sample-terminal' }),
  'network-matrix-execution': Object.freeze({
    start: 'execution-started',
    terminal: 'execution-terminal',
  }),
} as const)

export type NetworkMatrixTraceComponent = (typeof NETWORK_MATRIX_TRACE_COMPONENTS)[number]
export type NetworkMatrixTraceScenario = (typeof NETWORK_MATRIX_TRACE_SCENARIOS)[number]
export type NetworkMatrixTraceOutcome = (typeof NETWORK_MATRIX_TRACE_OUTCOMES)[number]

export interface NetworkMatrixTraceIdentity {
  readonly component: NetworkMatrixTraceComponent
  readonly scenario: NetworkMatrixTraceScenario
  readonly operationId: string
  readonly runId: string
  readonly profileId?: NetworkMatrixIdentity['profileId']
  readonly browser?: NetworkMatrixIdentity['browser']
  readonly sampleOrdinal?: NetworkMatrixIdentity['sampleOrdinal']
}

export interface NetworkMatrixTraceEvent extends NetworkMatrixTraceIdentity {
  readonly schemaVersion: typeof NETWORK_MATRIX_TRACE_SCHEMA_VERSION
  readonly milestone: string
  readonly outcome: NetworkMatrixTraceOutcome
  readonly context?: Readonly<Record<string, PortableTraceValue>>
}

export interface PortableTraceRecord {
  readonly [key: string]: PortableTraceValue
}

export type PortableTraceArray = readonly PortableTraceValue[]

export type PortableTraceValue =
  | null
  | boolean
  | number
  | string
  | PortableTraceArray
  | PortableTraceRecord

export interface NetworkMatrixTraceFailure {
  readonly name: string
  readonly message: string
}

export interface NetworkMatrixTraceSnapshot extends OwnedEventSnapshot<NetworkMatrixTraceEvent> {
  readonly observedBytes: number
  readonly capturedBytes: number
  readonly failure: NetworkMatrixTraceFailure | null
}

export interface NetworkMatrixTraceChannel extends AsyncIterable<NetworkMatrixTraceEvent> {
  snapshot(): NetworkMatrixTraceSnapshot
}

export interface NetworkMatrixTraceJournal {
  readonly view: NetworkMatrixTraceChannel
  append(event: NetworkMatrixTraceEvent): void
  finish(): void
}

/**
 * Trace construction is a trust boundary: callers cannot smuggle mutable values,
 * accessors, cycles, or runtime-only JSON values into later evidence publication.
 */
export function networkMatrixTrace(
  identity: NetworkMatrixTraceIdentity,
  milestone: string,
  outcome: NetworkMatrixTraceOutcome,
  context?: Readonly<Record<string, unknown>>,
): NetworkMatrixTraceEvent {
  const canonicalIdentity = canonicalNetworkMatrixTraceIdentity(identity)
  const canonicalMilestone = portableMilestone(milestone)
  const canonicalOutcome = requireMember(
    outcome,
    NETWORK_MATRIX_TRACE_OUTCOMES,
    'network matrix trace outcome',
  )
  const event = Object.freeze({
    schemaVersion: NETWORK_MATRIX_TRACE_SCHEMA_VERSION,
    ...canonicalIdentity,
    milestone: canonicalMilestone,
    outcome: canonicalOutcome,
    ...(context === undefined ? {} : {
      context: canonicalTraceContext(context),
    }),
  })
  requireEventByteLimit(event)
  return event
}

export function canonicalNetworkMatrixTraceEvent(
  event: NetworkMatrixTraceEvent,
): NetworkMatrixTraceEvent {
  rejectProxy(event, 'network matrix trace event')
  if (!isPlainRecord(event)) throw new Error('network matrix trace event must be a plain object')
  rejectAccessors(event, 'network matrix trace event')
  rejectUnknownKeys(event, [
    'schemaVersion',
    'component',
    'scenario',
    'operationId',
    'runId',
    'profileId',
    'browser',
    'sampleOrdinal',
    'milestone',
    'outcome',
    'context',
  ], 'network matrix trace event')
  if (event.schemaVersion !== NETWORK_MATRIX_TRACE_SCHEMA_VERSION) {
    throw new Error('network matrix trace schema version is invalid')
  }
  const identity = canonicalNetworkMatrixTraceIdentity(Object.freeze({
    component: event.component,
    scenario: event.scenario,
    operationId: event.operationId,
    runId: event.runId,
    ...(event.profileId === undefined ? {} : { profileId: event.profileId }),
    ...(event.browser === undefined ? {} : { browser: event.browser }),
    ...(event.sampleOrdinal === undefined ? {} : { sampleOrdinal: event.sampleOrdinal }),
  }))
  return networkMatrixTrace(identity, event.milestone, event.outcome, event.context)
}
export function canonicalNetworkMatrixTraceIdentity(
  identity: NetworkMatrixTraceIdentity,
): NetworkMatrixTraceIdentity {
  rejectProxy(identity, 'network matrix trace identity')
  if (!isPlainRecord(identity)) throw new Error('network matrix trace identity must be a plain object')
  rejectAccessors(identity, 'network matrix trace identity')
  rejectUnknownKeys(identity, [
    'component',
    'scenario',
    'operationId',
    'runId',
    'profileId',
    'browser',
    'sampleOrdinal',
  ], 'network matrix trace identity')
  const component = requireMember(
    identity.component,
    NETWORK_MATRIX_TRACE_COMPONENTS,
    'network matrix trace component',
  )
  const scenario = requireMember(
    identity.scenario,
    NETWORK_MATRIX_TRACE_SCENARIOS,
    'network matrix trace scenario',
  )
  if (
    (component === 'browser-network-matrix-execute') !==
      (scenario === 'network-matrix-execution')
  ) {
    throw new Error('network matrix trace component and scenario are inconsistent')
  }
  const operationId = requireOperationId(
    identity.operationId,
    'network matrix trace operation ID',
  )
  const runId = requireRunId(identity.runId, 'network matrix trace run ID')
  if (scenario === 'network-matrix-run' || scenario === 'network-matrix-execution') {
    if (
      identity.profileId !== undefined ||
      identity.browser !== undefined ||
      identity.sampleOrdinal !== undefined
    ) throw new Error('network matrix run trace invents sample identity fields')
    return Object.freeze({ component, scenario, operationId, runId })
  }
  const profileId = requireMember(
    identity.profileId,
    NETWORK_MATRIX_PROFILE_IDS,
    'network matrix trace profile ID',
  )
  if (scenario === 'network-matrix-profile') {
    if (identity.browser !== undefined || identity.sampleOrdinal !== undefined) {
      throw new Error('network matrix profile trace invents sample identity fields')
    }
    return Object.freeze({ component, scenario, operationId, runId, profileId })
  }
  const browser = requireMember(
    identity.browser,
    NETWORK_MATRIX_BROWSERS,
    'network matrix trace browser',
  )
  const sampleOrdinal = identity.sampleOrdinal
  if (
    typeof sampleOrdinal !== 'number' ||
    !Number.isSafeInteger(sampleOrdinal) ||
    sampleOrdinal < 1 ||
    sampleOrdinal > 5
  ) {
    throw new Error('network matrix trace sample ordinal is invalid')
  }
  return Object.freeze({
    component,
    scenario,
    operationId,
    runId,
    profileId,
    browser,
    sampleOrdinal: sampleOrdinal as NetworkMatrixIdentity['sampleOrdinal'],
  })
}

function canonicalTraceContext(
  value: Readonly<Record<string, unknown>>,
): Readonly<Record<string, PortableTraceValue>> {
  rejectProxy(value, 'network matrix trace context')
  if (!isPlainRecord(value)) throw new Error('network matrix trace context must be a plain object')
  const budget = { entries: 0 }
  return canonicalPortableRecord(value, 'network matrix trace context', new Set(), 0, budget)
}

function canonicalPortableValue(
  value: unknown,
  label: string,
  ancestors: Set<object>,
  depth: number,
  budget: { entries: number },
): PortableTraceValue {
  if (depth > NETWORK_MATRIX_MAXIMUM_TRACE_CONTEXT_DEPTH) {
    throw new Error(`${label} exceeds the portable depth limit`)
  }
  if (typeof value === 'object' && value !== null) rejectProxy(value, label)
  if (value === null || typeof value === 'boolean') return value
  if (typeof value === 'string') {
    if (networkMatrixTraceUtf8Bytes(value) > NETWORK_MATRIX_MAXIMUM_TRACE_STRING_BYTES) {
      throw new Error(`${label} string exceeds its byte limit`)
    }
    return value
  }
  if (typeof value === 'number') {
    if (!Number.isSafeInteger(value)) throw new Error(`${label} number is not a safe integer`)
    return value
  }
  if (Array.isArray(value)) {
    if (Object.getPrototypeOf(value) !== Array.prototype) {
      throw new Error(`${label} array has a non-portable prototype`)
    }
    return withAncestor(value, ancestors, label, () => {
      budget.entries += value.length
      requireContextBudget(budget, label)
      const descriptors = Object.getOwnPropertyDescriptors(value)
      const allowedKeys = new Set([
        'length',
        ...Array.from({ length: value.length }, (_, index) => String(index)),
      ])
      if (Reflect.ownKeys(value).some((key) =>
        typeof key !== 'string' || !allowedKeys.has(key))) {
        throw new Error(`${label} array contains an extra or symbolic field`)
      }
      for (let index = 0; index < value.length; index += 1) {
        const descriptor = descriptors[String(index)]
        if (
          descriptor === undefined ||
          !('value' in descriptor) ||
          !descriptor.enumerable
        ) {
          throw new Error(`${label} array contains a hole, accessor, or hidden entry`)
        }
      }
      const canonical: PortableTraceValue[] = []
      for (let index = 0; index < value.length; index += 1) {
        canonical.push(canonicalPortableValue(
          value[index],
          `${label}[${index}]`,
          ancestors,
          depth + 1,
          budget,
        ))
      }
      return Object.freeze(canonical)
    })
  }
  if (!isPlainRecord(value)) throw new Error(`${label} contains a non-portable value`)
  return canonicalPortableRecord(value, label, ancestors, depth, budget)
}

function canonicalPortableRecord(
  value: Readonly<Record<string, unknown>>,
  label: string,
  ancestors: Set<object>,
  depth: number,
  budget: { entries: number },
): Readonly<Record<string, PortableTraceValue>> {
  return withAncestor(value, ancestors, label, () => {
    const ownKeys = Reflect.ownKeys(value)
    if (ownKeys.some((key) => typeof key !== 'string')) {
      throw new Error(`${label} contains a symbolic field`)
    }
    rejectAccessors(value, label)
    const keys = (ownKeys as string[]).sort()
    budget.entries += keys.length
    requireContextBudget(budget, label)
    const canonical = Object.create(null) as Record<string, PortableTraceValue>
    for (const key of keys) {
      if (networkMatrixTraceUtf8Bytes(key) > NETWORK_MATRIX_MAXIMUM_TRACE_KEY_BYTES) {
        throw new Error(`${label} key exceeds its byte limit`)
      }
      Object.defineProperty(canonical, key, {
        value: canonicalPortableValue(
          value[key],
          `${label}.${key}`,
          ancestors,
          depth + 1,
          budget,
        ),
        enumerable: true,
        configurable: false,
        writable: false,
      })
    }
    return Object.freeze(canonical)
  })
}

function withAncestor<T>(
  value: object,
  ancestors: Set<object>,
  label: string,
  operation: () => T,
): T {
  if (ancestors.has(value)) throw new Error(`${label} contains a cycle`)
  ancestors.add(value)
  try {
    return operation()
  } finally {
    ancestors.delete(value)
  }
}

function requireEventByteLimit(event: NetworkMatrixTraceEvent): void {
  if (networkMatrixTraceUtf8Bytes(event) > NETWORK_MATRIX_MAXIMUM_TRACE_EVENT_BYTES) {
    throw new Error('network matrix trace event exceeds its byte limit')
  }
}

export function requireNetworkMatrixTraceTerminalContext(event: NetworkMatrixTraceEvent): void {
  const context = event.context
  if (context === undefined) {
    throw new Error('network matrix trace terminal lacks settlement context')
  }
  if (![
    'not-required',
    'completed',
    'failed',
    'deferred-to-outer-owner',
  ].includes(context.cleanupOutcome as string)) {
    throw new Error('network matrix trace terminal cleanup outcome is invalid')
  }
  const lastMilestone = context.lastMilestone
  if (
    typeof lastMilestone !== 'string' ||
    !PORTABLE_MILESTONE_PATTERN.test(lastMilestone) ||
    /(?:cleanup|close|rollback)/u.test(lastMilestone)
  ) {
    throw new Error('network matrix trace terminal last milestone is not workflow progress')
  }
}

export function networkMatrixTraceLifecycleKey(event: NetworkMatrixTraceIdentity): string {
  return [
    event.component,
    event.scenario,
    event.runId,
    event.operationId,
    event.profileId ?? '',
    event.browser ?? '',
    event.sampleOrdinal ?? '',
  ].join('|')
}

function portableMilestone(value: unknown): string {
  if (
    typeof value !== 'string' ||
    value.length > 128 ||
    !PORTABLE_MILESTONE_PATTERN.test(value)
  ) throw new Error('network matrix trace milestone is invalid')
  return value
}

function requireMember<const Value extends string>(
  value: unknown,
  allowed: readonly Value[],
  label: string,
): Value {
  if (!allowed.includes(value as Value)) throw new Error(`${label} is invalid`)
  return value as Value
}

function rejectUnknownKeys(
  value: Readonly<Record<string, unknown>>,
  allowedKeys: readonly string[],
  label: string,
): void {
  const allowed = new Set(allowedKeys)
  if (Reflect.ownKeys(value).some((key) => typeof key !== 'string' || !allowed.has(key))) {
    throw new Error(`${label} contains unknown keys`)
  }
}

function rejectAccessors(value: object, label: string): void {
  rejectProxy(value, label)
  for (const key of Reflect.ownKeys(value)) {
    const descriptor = Object.getOwnPropertyDescriptor(value, key)
    if (descriptor === undefined || !('value' in descriptor) || !descriptor.enumerable) {
      throw new Error(`${label} contains an accessor or hidden field`)
    }
  }
}

function isPlainRecord(value: unknown): value is Readonly<Record<string, unknown>> {
  if (typeof value !== 'object' || value === null) return false
  if (isProxy(value) || Array.isArray(value)) return false
  const prototype = Object.getPrototypeOf(value)
  return prototype === Object.prototype || prototype === null
}

function rejectProxy(value: unknown, label: string): void {
  if (typeof value === 'object' && value !== null && isProxy(value)) {
    throw new Error(`${label} must not be a Proxy`)
  }
}

function requireContextBudget(budget: { entries: number }, label: string): void {
  if (budget.entries > NETWORK_MATRIX_MAXIMUM_TRACE_CONTEXT_ENTRIES) {
    throw new Error(`${label} exceeds the portable entry limit`)
  }
}


export function networkMatrixTraceUtf8Bytes(value: unknown): number {
  return new TextEncoder().encode(JSON.stringify(value)).byteLength
}