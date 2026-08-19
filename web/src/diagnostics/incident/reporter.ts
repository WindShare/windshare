import {
  createFailureFactAccumulator,
  type FailureFactAccumulator,
  type SealedFailureFacts,
} from './aggregate'
import {
  createIncidentPolicy,
  DEFAULT_INCIDENT_POLICY,
  type IncidentPolicy,
} from './policy'
import {
  createIncidentScopeIssuer,
  type IncidentScopeHandle,
  type IncidentScopeIdentity,
  type IncidentScopeIssuer,
  type IncidentScopeKind,
  type IncidentScopeObserver,
  type IncidentScopeOwner,
} from './scope'
import type { FailureFact } from './fact'
import {
  presentationBoundaryForScope,
  type PresentationDecision,
  type PresentationOutcome,
  type PresentationBoundary,
} from './presentation'
import {
  BoundedIncidentHistory,
  type IncidentHistoryPort,
  type IncidentHistoryReadPort,
} from './history'
import {
  IncidentDiagnosticsHealth,
  type IncidentHealthReadPort,
} from './health'
import type { TraceCaptureSignal } from '../trace/ports'
import {
  captureDiagnosticContextV1,
  type DiagnosticContextSources,
} from '../export/context'
import type { IncidentRecordV1 } from '../export/incident-record-v1'
import type {
  IncidentRecordProjection,
  IncidentRecordProjector,
} from '../export/projector'

export interface IncidentTimestamp {
  readonly time: string
  readonly elapsedMs: bigint
}

export interface IncidentTimeSource {
  capture(): IncidentTimestamp
}

export interface IncidentConsoleSink {
  error(record: IncidentRecordV1): void
}

export interface IncidentTraceSignalPort {
  signal(
    signal: TraceCaptureSignal<IncidentLink, IncidentScopeIdentity>,
  ): void
}

export interface IncidentLink {
  readonly incidentSequence: bigint
  readonly scope: IncidentScopeIdentity
  readonly rootIncidentSequence?: bigint
}

export interface IncidentReporterOptions {
  readonly projector: IncidentRecordProjector
  readonly timeSource: IncidentTimeSource
  readonly consoleSink: IncidentConsoleSink
  readonly contextSources?: DiagnosticContextSources
  readonly traceSignals?: IncidentTraceSignalPort
  readonly scopeIssuer?: IncidentScopeIssuer
  readonly history?: IncidentHistoryPort
  readonly health?: IncidentDiagnosticsHealth
  readonly policy?: IncidentPolicy
}

export interface IncidentReporter {
  readonly history: IncidentHistoryReadPort
  readonly health: IncidentHealthReadPort
  openScope(kind: IncidentScopeKind): IncidentScopeOwner
  submitDecision(
    scope: IncidentScopeHandle,
    decision: PresentationDecision,
  ): void
  clearRetainedIncidents(): void
}

type FactObservation = Parameters<
  NonNullable<IncidentScopeObserver['factRecorded']>
>[0]

interface ActiveIncidentScope {
  readonly owner: IncidentScopeOwner
  accumulator?: FailureFactAccumulator
  decision?: PresentationDecision
  sealRequested: boolean
  sealing: boolean
  scopeClosed: boolean
  terminalSignaled: boolean
  consoleReports: number
}

interface LateIncidentLink {
  readonly link: IncidentLink
  readonly presentation: Readonly<{
    boundary: PresentationBoundary
    outcome: PresentationOutcome
  }>
  readonly sealedAtElapsedMs: bigint
  consoleReports: number
}

interface PendingLateLink {
  commit(): void
}

class BrowserIncidentReporter implements IncidentReporter {
  readonly history: IncidentHistoryReadPort
  readonly health: IncidentHealthReadPort
  readonly #history: IncidentHistoryPort
  readonly #health: IncidentDiagnosticsHealth
  readonly #projector: IncidentRecordProjector
  readonly #timeSource: IncidentTimeSource
  readonly #consoleSink: IncidentConsoleSink
  readonly #contextSources: DiagnosticContextSources
  readonly #traceSignals: IncidentTraceSignalPort | undefined
  readonly #scopeIssuer: IncidentScopeIssuer
  readonly #policy: IncidentPolicy
  readonly #active = new Map<string, ActiveIncidentScope>()
  readonly #lateLinks = new Map<string, LateIncidentLink>()
  #nextIncidentSequence = 1n

  constructor(options: IncidentReporterOptions) {
    this.#policy = options.policy === undefined
      ? DEFAULT_INCIDENT_POLICY
      : createIncidentPolicy(options.policy)
    this.#projector = options.projector
    this.#timeSource = options.timeSource
    this.#consoleSink = options.consoleSink
    this.#contextSources = options.contextSources ?? {}
    this.#traceSignals = options.traceSignals
    this.#scopeIssuer = options.scopeIssuer ??
      createIncidentScopeIssuer({ policy: this.#policy })
    this.#history = options.history ?? new BoundedIncidentHistory(this.#policy)
    this.history = this.#history
    this.#health = options.health ?? new IncidentDiagnosticsHealth()
    this.health = this.#health
  }

  openScope(kind: IncidentScopeKind): IncidentScopeOwner {
    const owner = this.#scopeIssuer.open(kind, {
      factRecorded: (observation) => this.#onFact(observation),
      deadlineReached: (identity) => this.#requestSeal(identity),
      scopeClosed: (identity) => this.#onScopeClosed(identity),
    })
    this.#active.set(scopeKey(owner.identity), {
      owner,
      sealRequested: false,
      sealing: false,
      scopeClosed: false,
      terminalSignaled: false,
      consoleReports: 0,
    })
    return owner
  }

  submitDecision(
    scope: IncidentScopeHandle,
    decision: PresentationDecision,
  ): void {
    try {
      this.#submitDecision(scope, decision)
    } catch {
      // Reporting is observational; a malformed diagnostic call has no authority.
    }
  }

  clearRetainedIncidents(): void {
    this.#lateLinks.clear()
    try {
      this.#history.clear()
    } catch {
      // Explicit clear still revokes late links if a custom history sink fails.
    }
  }

  #submitDecision(
    scope: IncidentScopeHandle,
    decision: PresentationDecision,
  ): void {
    const key = scopeKey(scope.identity)
    const state = this.#active.get(key)
    if (state === undefined || state.owner.handle !== scope) return
    if (state.decision !== undefined) return
    if (
      decision.boundary !== presentationBoundaryForScope(
        state.owner.identity.scopeKind,
      )
    ) {
      return
    }
    if (
      decision.kind === 'incident' &&
      (
        decision.trigger.scope.scopeKind !== state.owner.identity.scopeKind ||
        decision.trigger.scope.scopeSequence !==
          state.owner.identity.scopeSequence
      )
    ) {
      return
    }
    state.decision = decision
    if (decision.kind === 'excluded') {
      if (state.scopeClosed) this.#signalScopeTerminal(state)
      this.#active.delete(key)
      return
    }
    this.#trySeal(state)
    if (state.scopeClosed && this.#active.has(key)) {
      this.#signalScopeTerminal(state)
      this.#active.delete(key)
    }
  }

  #onFact(observation: FactObservation): void {
    const key = scopeKey(observation.ref.scope)
    const active = this.#active.get(key)
    if (active !== undefined) {
      active.accumulator ??= createFailureFactAccumulator(
        active.owner.identity,
        this.#policy,
      )
      active.accumulator.record(observation.ref)
      return
    }
    if (observation.relation === 'consequence') {
      this.#reportLateConsequence(observation)
    }
  }

  #requestSeal(identity: IncidentScopeIdentity): void {
    const state = this.#active.get(scopeKey(identity))
    if (state === undefined) return
    state.sealRequested = true
    this.#trySeal(state)
  }

  #onScopeClosed(identity: IncidentScopeIdentity): void {
    const key = scopeKey(identity)
    const state = this.#active.get(key)
    if (state === undefined) {
      this.#signalTrace({
        kind: 'scope_terminal',
        scope: identity,
        elapsedMs: this.#safeElapsedMilliseconds(),
      })
      return
    }

    state.scopeClosed = true
    // Sealing first lets an enabled trace establish its root before the same
    // close notification terminates that root's post-failure tail.
    this.#requestSeal(identity)
    if (state.decision !== undefined && this.#active.has(key)) {
      this.#signalScopeTerminal(state)
      this.#active.delete(key)
    }
  }

  #trySeal(state: ActiveIncidentScope): void {
    if (
      !state.sealRequested ||
      state.sealing ||
      state.decision?.kind !== 'incident' ||
      state.accumulator === undefined
    ) {
      return
    }
    state.sealing = true
    const key = scopeKey(state.owner.identity)
    try {
      const facts = state.accumulator.seal(state.decision.trigger)
      this.#emitRootIncident(state, facts)
    } catch {
      // Projection/reporting failure cannot delay or change the owned outcome.
    } finally {
      if (state.scopeClosed) this.#signalScopeTerminal(state)
      this.#active.delete(key)
    }
  }

  #signalScopeTerminal(state: ActiveIncidentScope): void {
    if (state.terminalSignaled) return
    state.terminalSignaled = true
    this.#signalTrace({
      kind: 'scope_terminal',
      scope: state.owner.identity,
      elapsedMs: this.#safeElapsedMilliseconds(),
    })
  }

  #emitRootIncident(
    state: ActiveIncidentScope,
    facts: SealedFailureFacts,
  ): void {
    if (state.decision?.kind !== 'incident') return
    const sequence = this.#allocateIncidentSequence()
    const timestamp = this.#readTimestamp()
    const initialOverflow =
      facts.contributorOverflowCount +
      facts.consequenceOverflowCount
    this.#health.recordFactOverflow(initialOverflow)
    this.#recordPendingHistoryEviction()
    const consoleAdmitted = this.#admitConsole(state)
    const pendingLink = this.#prepareLateLink(
      facts.scope,
      sequence,
      state.decision.boundary,
      state.decision.outcome,
      timestamp.elapsedMs,
      state.consoleReports,
    )
    const projection = this.#projectWithExactHealth({
      sequence,
      timestamp,
      presentation: state.decision,
      facts,
      initialOverflow,
    })

    this.#appendHistory(projection.record)
    pendingLink.commit()
    const link = Object.freeze({
      incidentSequence: sequence,
      scope: facts.scope,
    })
    this.#signalTrace({
      kind: 'incident_sealed',
      incident: link,
      elapsedMs: timestamp.elapsedMs,
    })
    if (consoleAdmitted) this.#reportConsole(projection.record)
  }

  #reportLateConsequence(observation: FactObservation): void {
    const timestamp = this.#readTimestamp()
    const key = scopeKey(observation.ref.scope)
    this.#evictExpiredLateLinks(timestamp.elapsedMs)
    const root = this.#lateLinks.get(key)
    if (root === undefined) return

    const sequence = this.#allocateIncidentSequence()
    this.#recordPendingHistoryEviction()
    const consoleAdmitted = this.#admitConsole(root)
    const facts = lateConsequenceFacts(observation)
    const projection = this.#projectWithExactHealth({
      sequence,
      timestamp,
      presentation: root.presentation,
      facts,
      initialOverflow: 0n,
      rootIncidentSequence: root.link.incidentSequence,
    })

    this.#appendHistory(projection.record)
    const link = Object.freeze({
      incidentSequence: sequence,
      rootIncidentSequence: root.link.incidentSequence,
      scope: root.link.scope,
    })
    this.#signalTrace({
      kind: 'incident_sealed',
      incident: link,
      elapsedMs: timestamp.elapsedMs,
    })
    if (consoleAdmitted) this.#reportConsole(projection.record)
  }

  #projectWithExactHealth(input: {
    readonly sequence: bigint
    readonly timestamp: IncidentTimestamp
    readonly presentation: Readonly<{
      boundary: PresentationBoundary
      outcome: PresentationOutcome
    }>
    readonly facts: SealedFailureFacts
    readonly initialOverflow: bigint
    readonly rootIncidentSequence?: bigint
  }): IncidentRecordProjection {
    let accountedOverflow = input.initialOverflow
    const context = captureDiagnosticContextV1(this.#contextSources)
    const maximumPasses =
      input.facts.contributorBuckets.length +
      input.facts.consequenceBuckets.length +
      2
    for (let pass = 0; pass < maximumPasses; pass += 1) {
      const result = this.#projector.project({
        sequence: input.sequence,
        time: input.timestamp.time,
        elapsedMs: input.timestamp.elapsedMs,
        presentation: input.presentation,
        facts: input.facts,
        context,
        health: this.#health.incidentHealthSnapshot(),
        ...(input.rootIncidentSequence === undefined
          ? {}
          : { rootIncidentSequence: input.rootIncidentSequence }),
      })
      if (result.overflowFactCount < accountedOverflow) {
        throw new Error('Incident projector lost overflow accounting')
      }
      if (result.overflowFactCount === accountedOverflow) return result
      this.#health.recordFactOverflow(
        result.overflowFactCount - accountedOverflow,
      )
      accountedOverflow = result.overflowFactCount
    }
    throw new Error('Incident projection did not reach a stable health cut')
  }

  #recordPendingHistoryEviction(): void {
    try {
      const count = this.#history.nextAppendEvictionCount()
      if (typeof count !== 'bigint' || count < 0n) return
      this.#health.recordHistoryEviction(count)
    } catch {
      // A fallible history admission read cannot suppress other incident sinks.
    }
  }

  #admitConsole(
    scope: ActiveIncidentScope | LateIncidentLink,
  ): boolean {
    if (scope.consoleReports >= this.#policy.maxConsoleReportsPerScope) {
      this.#health.recordConsoleSuppression()
      return false
    }
    scope.consoleReports += 1
    return true
  }

  #prepareLateLink(
    scope: IncidentScopeIdentity,
    sequence: bigint,
    boundary: PresentationBoundary,
    outcome: PresentationOutcome,
    elapsedMs: bigint,
    consoleReports: number,
  ): PendingLateLink {
    this.#evictExpiredLateLinks(elapsedMs)
    while (this.#lateLinks.size >= this.#policy.maxLateIncidentLinks) {
      const oldestKey = this.#lateLinks.keys().next().value as string | undefined
      if (oldestKey === undefined) break
      this.#lateLinks.delete(oldestKey)
      this.#health.recordLateLinkEviction()
    }
    const key = scopeKey(scope)
    const entry: LateIncidentLink = {
      link: Object.freeze({ incidentSequence: sequence, scope }),
      presentation: Object.freeze({ boundary, outcome }),
      sealedAtElapsedMs: elapsedMs,
      consoleReports,
    }
    return Object.freeze({
      commit: () => {
        this.#lateLinks.set(key, entry)
      },
    })
  }

  #evictExpiredLateLinks(nowElapsedMs: bigint): void {
    const maximumAge = BigInt(this.#policy.maxLateIncidentLinkAgeMilliseconds)
    for (const [key, entry] of this.#lateLinks) {
      if (
        nowElapsedMs >= entry.sealedAtElapsedMs &&
        nowElapsedMs - entry.sealedAtElapsedMs >= maximumAge
      ) {
        this.#lateLinks.delete(key)
        this.#health.recordLateLinkEviction()
      }
    }
  }

  #appendHistory(record: IncidentRecordV1): void {
    try {
      this.#history.append(record)
    } catch {
      // History retention is independent from Console and trace markers.
    }
  }

  #reportConsole(record: IncidentRecordV1): void {
    try {
      this.#consoleSink.error(record)
    } catch {
      // Developer Console failure cannot suppress the retained incident.
    }
  }

  #signalTrace(
    signal: TraceCaptureSignal<IncidentLink, IncidentScopeIdentity>,
  ): void {
    try {
      this.#traceSignals?.signal(signal)
    } catch {
      // Detailed trace is optional and never owns incident admission.
    }
  }

  #readTimestamp(): IncidentTimestamp {
    const timestamp = this.#timeSource.capture()
    if (
      typeof timestamp.time !== 'string' ||
      typeof timestamp.elapsedMs !== 'bigint' ||
      timestamp.elapsedMs < 0n
    ) {
      throw new TypeError('Incident time source returned an invalid timestamp')
    }
    return Object.freeze({
      time: timestamp.time,
      elapsedMs: timestamp.elapsedMs,
    })
  }

  #safeElapsedMilliseconds(): bigint {
    try {
      return this.#readTimestamp().elapsedMs
    } catch {
      return 0n
    }
  }

  #allocateIncidentSequence(): bigint {
    const sequence = this.#nextIncidentSequence
    this.#nextIncidentSequence += 1n
    return sequence
  }
}

export function createIncidentReporter(
  options: IncidentReporterOptions,
): IncidentReporter {
  return new BrowserIncidentReporter(options)
}

function lateConsequenceFacts(
  observation: FactObservation,
): SealedFailureFacts {
  return Object.freeze({
    scope: observation.ref.scope,
    trigger: Object.freeze({
      ref: observation.ref,
      fact: observation.fact as FailureFact,
    }),
    factCount: 1n,
    contributorBuckets: Object.freeze([]),
    contributorOverflowCount: 0n,
    consequenceBuckets: Object.freeze([]),
    consequenceOverflowCount: 0n,
  })
}

function scopeKey(identity: IncidentScopeIdentity): string {
  return `${identity.scopeKind}:${identity.scopeSequence.toString(10)}`
}
