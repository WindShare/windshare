import {
  createFailureIdentity,
  type FailureIdentity,
  type IncidentScopeHandle,
  type IncidentScopeIdentity,
} from '../../diagnostics/incident'
import {
  projectLocalOutputCorrelationV1,
  type CorrelationV1,
  type LocalOutputCorrelationInputV1,
} from '../../diagnostics/export/correlation-v1'
import {
  projectPersistentOutputStageFailure,
  type PersistentOutputStageDiagnostics,
  type PersistentOutputStageFailureMilestone,
  type PersistentOutputStageFailureProjectionV1,
  type PersistentOutputStageMilestone,
} from '../persistent-tree/stage-diagnostics'

export const LOCAL_OUTPUT_OPERATION_FAILURE_SCHEMA_VERSION = 1 as const
export const MAX_RETAINED_LOCAL_OUTPUT_OPERATION_FAILURES = 32

const UINT32_MAX = 0xffff_ffff
const UINT64_MAX = 0xffff_ffff_ffff_ffffn

export interface LocalOutputOperationFailureV1 {
  readonly schemaVersion: typeof LOCAL_OUTPUT_OPERATION_FAILURE_SCHEMA_VERSION
  readonly recordKind: 'local_output_operation_failure'
  readonly stageFailure: PersistentOutputStageFailureProjectionV1
}

export interface CorrelatedLocalOutputOperationFailureV1 {
  readonly owningScope: Readonly<{
    readonly scopeKind: IncidentScopeIdentity['scopeKind']
    readonly scopeSequence: string
  }>
  readonly correlation: CorrelationV1
  readonly failure: LocalOutputOperationFailureV1
}

export interface LocalOutputOperationFailureReadPort {
  snapshot(): readonly CorrelatedLocalOutputOperationFailureV1[]
}

interface LocalOutputFailureProtocolAttempt {
  readonly protocolSessionIdentity: FailureIdentity<'protocol_session'>
  readonly protocolGeneration: number
}

export interface LocalOutputFailureAttemptLease {
  readonly scope: IncidentScopeIdentity
  isActive(): boolean
  protocolAttempt(transferJobId: string): LocalOutputFailureProtocolAttempt | undefined
}

/** A binding may switch attempts; claiming snapshots the current owner for one stage operation. */
export interface LocalOutputFailureAttemptSource {
  claim(): LocalOutputFailureAttemptLease | undefined
}

export interface LocalOutputFailureAttemptAuthority {
  readonly source: LocalOutputFailureAttemptSource
  revoke(): void
}

export interface LocalOutputOperationFailureDiagnosticsPort {
  forAttempt(input: Readonly<{
    attempt: LocalOutputFailureAttemptSource
    transferJobId: string
    outputSessionId: string
  }>): PersistentOutputStageDiagnostics
}

interface LocalOutputFailureAttemptState {
  readonly scope: IncidentScopeIdentity
  readonly protocolAttempts: Map<string, LocalOutputFailureProtocolAttempt>
  active: boolean
}

const attemptStates = new WeakMap<IncidentScopeHandle, LocalOutputFailureAttemptState>()

export class BoundedLocalOutputOperationFailureHistory
implements LocalOutputOperationFailureReadPort, LocalOutputOperationFailureDiagnosticsPort {
  readonly #capacity: number
  // Raw thrown values stay on the synchronous milestone so count-bounded history
  // cannot accidentally extend the lifetime of an arbitrarily large object graph.
  readonly #records: CorrelatedLocalOutputOperationFailureV1[] = []
  #generation = 0

  constructor(capacity = MAX_RETAINED_LOCAL_OUTPUT_OPERATION_FAILURES) {
    if (!Number.isSafeInteger(capacity) || capacity <= 0) {
      throw new RangeError('local output failure history capacity must be a positive integer')
    }
    this.#capacity = capacity
  }

  forAttempt(input: Readonly<{
    attempt: LocalOutputFailureAttemptSource
    transferJobId: string
    outputSessionId: string
  }>): PersistentOutputStageDiagnostics {
    const transferJobId = requireIdentity(input.transferJobId, 'transfer job')
    const outputSessionId = requireIdentity(input.outputSessionId, 'output session')
    const generation = this.#generation
    const pending = new Map<string, Array<LocalOutputFailureAttemptLease | null>>()
    return Object.freeze({
      outputSessionId,
      observe: (milestone: PersistentOutputStageMilestone) => {
        if (generation !== this.#generation ||
            milestone.correlation.outputSessionId !== outputSessionId) return
        try {
          const key = stageInvocationKey(milestone)
          if (milestone.transition === 'started') {
            const queue = pending.get(key) ?? []
            queue.push(input.attempt.claim() ?? null)
            pending.set(key, queue)
            return
          }
          const claimed = consumeClaim(pending, key)
          if (milestone.transition !== 'failed') return
          const attempt = claimed.found ? claimed.attempt : input.attempt.claim()
          if (attempt === undefined || !attempt.isActive()) return
          const protocol = attempt.protocolAttempt(transferJobId)
          const record = projectCorrelatedLocalOutputOperationFailure(milestone, {
            owningScope: attempt.scope,
            correlation: {
              receiveOperationId: milestone.correlation.operationId,
              transferJobId,
              outputSessionId,
              ...(protocol === undefined ? {} : protocol),
            },
          })
          this.#append(record)
        } catch {
          // Local diagnostics cannot replace or delay the native output result.
        }
      },
    })
  }

  snapshot(): readonly CorrelatedLocalOutputOperationFailureV1[] {
    return Object.freeze([...this.#records])
  }

  clear(): void {
    this.#generation += 1
    this.#records.splice(0)
  }

  #append(record: CorrelatedLocalOutputOperationFailureV1): void {
    if (this.#records.length >= this.#capacity) this.#records.shift()
    this.#records.push(record)
  }
}

export function createLocalOutputFailureAttemptAuthority(
  scope: IncidentScopeHandle | undefined,
): LocalOutputFailureAttemptAuthority {
  if (scope === undefined) {
    return Object.freeze({
      source: Object.freeze({ claim: () => undefined }),
      revoke: () => undefined,
    })
  }
  if (attemptStates.has(scope)) {
    throw new TypeError('incident scope already owns a local output failure attempt')
  }
  const state: LocalOutputFailureAttemptState = {
    scope: Object.freeze({ ...scope.identity }),
    protocolAttempts: new Map(),
    active: true,
  }
  const lease = localOutputFailureAttemptLease(state, () => state.active)
  attemptStates.set(scope, state)
  return Object.freeze({
    source: Object.freeze({
      claim: () => state.active ? lease : undefined,
    }),
    revoke: () => {
      state.active = false
    },
  })
}

export function createLateLocalOutputFailureAttemptAuthority(
  scope: IncidentScopeHandle | undefined,
): LocalOutputFailureAttemptAuthority {
  const state = scope === undefined ? undefined : attemptStates.get(scope)
  let active = state !== undefined
  const lease = state === undefined ? undefined : localOutputFailureAttemptLease(state, () => active)
  return Object.freeze({
    source: Object.freeze({
      claim: () => active ? lease : undefined,
    }),
    revoke: () => {
      active = false
    },
  })
}

/**
 * The gateway is the sole protocol-generation source. This weak association only
 * completes an existing attempt and creates no page-global retention or protocol I/O.
 */
export function bindLocalOutputFailureProtocolAttempt(
  scope: IncidentScopeHandle,
  input: Readonly<{
    transferJobId: string
    protocolSessionIdentity: FailureIdentity<'protocol_session'>
    protocolGeneration: number
  }>,
): boolean {
  const state = attemptStates.get(scope)
  if (state === undefined || !state.active) return false
  const transferJobId = requireIdentity(input.transferJobId, 'transfer job')
  if (!Number.isSafeInteger(input.protocolGeneration) ||
      input.protocolGeneration <= 0 || input.protocolGeneration > UINT32_MAX) {
    throw new RangeError('protocol generation must be a positive uint32')
  }
  const protocolSessionIdentity = createFailureIdentity(
    'protocol_session',
    input.protocolSessionIdentity.copyBytes(),
  )
  const existing = state.protocolAttempts.get(transferJobId)
  if (existing !== undefined) {
    if (existing.protocolGeneration !== input.protocolGeneration ||
        !sameIdentity(existing.protocolSessionIdentity, protocolSessionIdentity)) {
      throw new TypeError('transfer job already has a different protocol attempt correlation')
    }
    return true
  }
  state.protocolAttempts.set(transferJobId, Object.freeze({
    protocolSessionIdentity,
    protocolGeneration: input.protocolGeneration,
  }))
  return true
}

export function projectCorrelatedLocalOutputOperationFailure(
  milestone: PersistentOutputStageFailureMilestone,
  association: Readonly<{
    owningScope: IncidentScopeIdentity
    correlation: LocalOutputCorrelationInputV1
  }>,
): CorrelatedLocalOutputOperationFailureV1 {
  return Object.freeze({
    owningScope: projectIncidentScope(association.owningScope),
    correlation: projectLocalOutputCorrelationV1(association.correlation),
    failure: projectLocalOutputOperationFailure(milestone),
  })
}

export function projectLocalOutputOperationFailure(
  milestone: PersistentOutputStageFailureMilestone,
): LocalOutputOperationFailureV1 {
  return Object.freeze({
    schemaVersion: LOCAL_OUTPUT_OPERATION_FAILURE_SCHEMA_VERSION,
    recordKind: 'local_output_operation_failure',
    stageFailure: projectPersistentOutputStageFailure(milestone),
  })
}

function projectIncidentScope(
  scope: IncidentScopeIdentity,
): CorrelatedLocalOutputOperationFailureV1['owningScope'] {
  if (scope.scopeSequence <= 0n || scope.scopeSequence > UINT64_MAX) {
    throw new RangeError('local output failure incident scope sequence must be a positive uint64')
  }
  return Object.freeze({
    scopeKind: scope.scopeKind,
    scopeSequence: scope.scopeSequence.toString(10),
  })
}

function localOutputFailureAttemptLease(
  state: LocalOutputFailureAttemptState,
  active: () => boolean,
): LocalOutputFailureAttemptLease {
  return Object.freeze({
    scope: state.scope,
    isActive: active,
    protocolAttempt: (transferJobId: string) =>
      state.protocolAttempts.get(requireIdentity(transferJobId, 'transfer job')),
  })
}

function consumeClaim(
  pending: Map<string, Array<LocalOutputFailureAttemptLease | null>>,
  key: string,
): Readonly<{ found: boolean; attempt?: LocalOutputFailureAttemptLease }> {
  const queue = pending.get(key)
  if (queue === undefined || queue.length === 0) return Object.freeze({ found: false })
  const attempt = queue.shift()
  if (queue.length === 0) pending.delete(key)
  return Object.freeze({
    found: true,
    ...(attempt === null || attempt === undefined ? {} : { attempt }),
  })
}

function stageInvocationKey(milestone: PersistentOutputStageMilestone): string {
  const correlation = milestone.correlation
  return [
    milestone.stage,
    correlation.operationId,
    correlation.outputSessionId,
    correlation.target,
    correlation.artifactId,
    correlation.artifactPath.map(segment => `${segment.length}:${segment}`).join('/'),
    correlation.ownedObjectId ?? '',
    correlation.checkpointRecordId ?? '',
    correlation.checkpointGeneration?.toString(10) ?? '',
  ].join('\u0000')
}

function sameIdentity(
  left: FailureIdentity<'protocol_session'>,
  right: FailureIdentity<'protocol_session'>,
): boolean {
  const leftBytes = left.copyBytes()
  const rightBytes = right.copyBytes()
  return leftBytes.every((value, index) => value === rightBytes[index])
}

function requireIdentity(value: string, kind: string): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw new TypeError(`local output failure ${kind} identity must be non-empty`)
  }
  return value
}
