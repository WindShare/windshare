import { isFailureFact, type FailureFact } from './fact'
import {
  DEFAULT_INCIDENT_POLICY,
  createIncidentPolicy,
  type IncidentPolicy,
} from './policy'

export const INCIDENT_SCOPE_KINDS = Object.freeze([
  'join',
  'browse',
  'preview_open',
  'preview_seek',
  'preview_media',
  'projection',
  'authority_activation',
  'receive',
  'lifecycle_action',
  'retained_inventory',
  'retained_action',
] as const)

export type IncidentScopeKind = (typeof INCIDENT_SCOPE_KINDS)[number]

export interface IncidentScopeIdentity {
  readonly scopeKind: IncidentScopeKind
  readonly scopeSequence: bigint
}

export const FAILURE_FACT_RELATIONS = Object.freeze([
  'contributor',
  'consequence',
] as const)

export type FailureFactRelation = (typeof FAILURE_FACT_RELATIONS)[number]

const failureFactRefBrand: unique symbol = Symbol('FailureFactRef')

export interface FailureFactRef {
  readonly scope: IncidentScopeIdentity
  readonly factSequence: bigint
  readonly [failureFactRefBrand]: true
}

export interface IncidentClock {
  elapsedMilliseconds(): number
}

export interface IncidentScheduleCancellation {
  cancel(): void
}

export interface IncidentScheduler {
  schedule(
    delayMilliseconds: number,
    callback: () => void,
  ): IncidentScheduleCancellation
}

export interface IncidentScopeObserver {
  factRecorded?(observation: Readonly<{
    ref: FailureFactRef
    fact: FailureFact
    relation: FailureFactRelation
    elapsedMilliseconds: number
  }>): void
  deadlineReached?(identity: IncidentScopeIdentity): void
  scopeClosed?(identity: IncidentScopeIdentity): void
}

export interface FailureFactSink {
  record(fact: FailureFact, relation: FailureFactRelation): FailureFactRef
}

export interface IncidentScopeHandle {
  readonly identity: IncidentScopeIdentity
  readonly facts: FailureFactSink
}

export interface IncidentScopeOwner extends IncidentScopeHandle {
  readonly handle: IncidentScopeHandle
  close(): void
  isClosed(): boolean
}

export interface IncidentScopeIssuer {
  open(
    kind: IncidentScopeKind,
    observer?: IncidentScopeObserver,
  ): IncidentScopeOwner
}

const UINT64_MAX = 0xffff_ffff_ffff_ffffn

const refFacts = new WeakMap<FailureFactRef, FailureFact>()
const refRelations = new WeakMap<FailureFactRef, FailureFactRelation>()

interface OpenScopeState {
  nextFactSequence: bigint
  deadlineToken?: object
  deadlineCancellation?: IncidentScheduleCancellation
}

class ScopeFactRef implements FailureFactRef {
  readonly scope: IncidentScopeIdentity
  readonly factSequence: bigint
  readonly [failureFactRefBrand] = true

  constructor(scope: IncidentScopeIdentity, factSequence: bigint) {
    this.scope = scope
    this.factSequence = factSequence
    Object.freeze(this)
  }
}

class DefaultIncidentScopeIssuer implements IncidentScopeIssuer {
  readonly #clock: IncidentClock
  readonly #scheduler: IncidentScheduler
  readonly #policy: IncidentPolicy
  #nextScopeSequence = 1n

  constructor(
    clock: IncidentClock,
    scheduler: IncidentScheduler,
    policy: IncidentPolicy,
  ) {
    this.#clock = clock
    this.#scheduler = scheduler
    this.#policy = policy
  }

  open(
    kind: IncidentScopeKind,
    observer: IncidentScopeObserver = {},
  ): IncidentScopeOwner {
    if (!isMember(INCIDENT_SCOPE_KINDS, kind)) {
      throw new RangeError('Unknown incident scope kind')
    }
    if (this.#nextScopeSequence > UINT64_MAX) {
      throw new RangeError('Incident scope sequence exhausted')
    }
    const identity = Object.freeze({
      scopeKind: kind,
      scopeSequence: this.#nextScopeSequence,
    })
    this.#nextScopeSequence += 1n
    return new OpenIncidentScope(
      identity,
      this.#clock,
      this.#scheduler,
      this.#policy,
      observer,
    )
  }
}

class OpenIncidentScope implements IncidentScopeOwner {
  readonly identity: IncidentScopeIdentity
  readonly facts: FailureFactSink
  readonly handle: IncidentScopeHandle
  readonly #clock: IncidentClock
  readonly #scheduler: IncidentScheduler
  readonly #policy: IncidentPolicy
  readonly #observer: IncidentScopeObserver
  #state?: OpenScopeState
  #closed = false

  constructor(
    identity: IncidentScopeIdentity,
    clock: IncidentClock,
    scheduler: IncidentScheduler,
    policy: IncidentPolicy,
    observer: IncidentScopeObserver,
  ) {
    this.identity = identity
    this.#clock = clock
    this.#scheduler = scheduler
    this.#policy = policy
    this.#observer = observer
    this.facts = Object.freeze({
      record: (fact: FailureFact, relation: FailureFactRelation) =>
        this.#record(fact, relation),
    })
    this.handle = Object.freeze({
      identity,
      facts: this.facts,
    })
  }

  #record(
    fact: FailureFact,
    relation: FailureFactRelation,
  ): FailureFactRef {
    if (!isFailureFact(fact)) throw new TypeError('Incident scope accepts only failure facts')
    if (!isMember(FAILURE_FACT_RELATIONS, relation)) {
      throw new RangeError('Unknown failure fact relation')
    }
    if (this.#closed && relation === 'contributor') {
      throw new Error('Closed incident scopes reject new contributors')
    }

    const elapsedMilliseconds = readClock(this.#clock)
    const state = this.#state ?? this.#createState()
    if (state.nextFactSequence > UINT64_MAX) {
      throw new RangeError('Failure fact sequence exhausted')
    }
    const ref = new ScopeFactRef(this.identity, state.nextFactSequence)
    state.nextFactSequence += 1n
    refFacts.set(ref, fact)
    refRelations.set(ref, relation)
    this.#notify(() => this.#observer.factRecorded?.(Object.freeze({
      ref,
      fact,
      relation,
      elapsedMilliseconds,
    })))
    return ref
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#cancelDeadline()
    this.#notify(() => this.#observer.scopeClosed?.(this.identity))
  }

  isClosed(): boolean {
    return this.#closed
  }

  #createState(): OpenScopeState {
    const state: OpenScopeState = {
      nextFactSequence: 1n,
    }
    this.#state = state
    if (!this.#closed) {
      const token = {}
      state.deadlineToken = token
      try {
        state.deadlineCancellation = this.#scheduler.schedule(
          this.#policy.incidentSealDeadlineMilliseconds,
          () => {
            if (
              this.#closed ||
              this.#state?.deadlineToken !== token
            ) {
              return
            }
            delete state.deadlineToken
            delete state.deadlineCancellation
            this.#notify(() => this.#observer.deadlineReached?.(this.identity))
          },
        )
      } catch {
        // Losing a diagnostic deadline cannot acquire authority over product work.
        delete state.deadlineToken
      }
    }
    return state
  }

  #cancelDeadline(): void {
    const state = this.#state
    if (state === undefined) return
    delete state.deadlineToken
    try {
      state.deadlineCancellation?.cancel()
    } catch {
      // Diagnostic scheduling must not make the product workflow fail.
    }
    delete state.deadlineCancellation
  }

  #notify(callback: () => void): void {
    try {
      callback()
    } catch {
      // Observers are diagnostic side effects and cannot acquire workflow authority.
    }
  }
}

export function createIncidentScopeIssuer(input?: {
  readonly clock?: IncidentClock
  readonly scheduler?: IncidentScheduler
  readonly policy?: IncidentPolicy
}): IncidentScopeIssuer {
  return new DefaultIncidentScopeIssuer(
    input?.clock ?? browserIncidentClock,
    input?.scheduler ?? browserIncidentScheduler,
    input?.policy === undefined
      ? DEFAULT_INCIDENT_POLICY
      : createIncidentPolicy(input.policy),
  )
}

// Aggregate and reporter layers consume these capabilities without exposing
// payload retrieval on the opaque reference given to presentation code.
export function isKnownFailureFactRef(value: unknown): value is FailureFactRef {
  return (
    typeof value === 'object' &&
    value !== null &&
    refFacts.has(value as FailureFactRef)
  )
}

export function failureFactForRef(ref: FailureFactRef): FailureFact {
  const fact = refFacts.get(ref)
  if (fact === undefined) throw new TypeError('Unknown failure fact reference')
  return fact
}

export function failureFactRelationForRef(
  ref: FailureFactRef,
): FailureFactRelation {
  const relation = refRelations.get(ref)
  if (relation === undefined) throw new TypeError('Unknown failure fact reference')
  return relation
}

export function sameIncidentScope(
  left: FailureFactRef,
  right: FailureFactRef,
): boolean {
  return (
    left.scope.scopeKind === right.scope.scopeKind &&
    left.scope.scopeSequence === right.scope.scopeSequence
  )
}

function readClock(clock: IncidentClock): number {
  const value = clock.elapsedMilliseconds()
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new RangeError('Incident clock must return a safe non-negative integer')
  }
  return value
}

const browserIncidentClock: IncidentClock = Object.freeze({
  elapsedMilliseconds: () => Math.floor(performance.now()),
})

const browserIncidentScheduler: IncidentScheduler = Object.freeze({
  schedule: (delayMilliseconds: number, callback: () => void) => {
    const timeout = globalThis.setTimeout(callback, delayMilliseconds)
    return Object.freeze({
      cancel: () => globalThis.clearTimeout(timeout),
    })
  },
})

function isMember<const Value extends string>(
  values: readonly Value[],
  value: unknown,
): value is Value {
  return typeof value === 'string' && values.includes(value as Value)
}
