import { projectIncidentConsequencesV2, type IncidentConsequencesV2 } from './consequences'
import type { FailureEvidenceHistoryPort } from '../failure/history'
import type { FailureFact } from './fact'
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
import {
  type IncidentPresentationDecision,
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
import { PendingIncidentScopes } from './pending-scopes'
import {
  captureDiagnosticContextV1,
  type DiagnosticContextSources,
} from '../export/context'
import type { IncidentRecordV2 } from '../export/incident-record-v2'
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
  error(record: IncidentRecordV2 | IncidentConsequencesV2): void
}

export interface IncidentTraceSignalPort {
  signal(
    signal: TraceCaptureSignal<IncidentLink>,
  ): void
}

export interface IncidentLink {
  readonly incidentSequence: bigint
  readonly scope: IncidentScopeIdentity
}

export interface IncidentReporterOptions {
  readonly evidence?: FailureEvidenceHistoryPort
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

interface LateIncidentLink {
  readonly runtimeRunId: string
  readonly link: IncidentLink
  readonly sealedAtElapsedMs: bigint
  consoleReports: number
  accumulator?: FailureFactAccumulator
  firstObserved?: IncidentTimestamp
  readonly omittedBucketCounts: WeakMap<FailureFact, bigint>
}

interface PendingLateLink {
  commit(runtimeRunId: string): void
}

class BrowserIncidentReporter implements IncidentReporter {
  readonly history: IncidentHistoryReadPort
  readonly health: IncidentHealthReadPort
  readonly #evidence: FailureEvidenceHistoryPort | undefined
  readonly #history: IncidentHistoryPort
  readonly #health: IncidentDiagnosticsHealth
  readonly #projector: IncidentRecordProjector
  readonly #timeSource: IncidentTimeSource
  readonly #consoleSink: IncidentConsoleSink
  readonly #contextSources: DiagnosticContextSources
  readonly #traceSignals: IncidentTraceSignalPort | undefined
  readonly #scopeIssuer: IncidentScopeIssuer
  readonly #policy: IncidentPolicy
  readonly #pending: PendingIncidentScopes
  readonly #lateLinks = new Map<string, LateIncidentLink>()
  #nextIncidentSequence = 1n

  constructor(options: IncidentReporterOptions) {
    this.#policy = options.policy === undefined
      ? DEFAULT_INCIDENT_POLICY
      : createIncidentPolicy(options.policy)
    this.#evidence = options.evidence
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
    this.#pending = new PendingIncidentScopes(
      this.#policy, (decision, facts) => this.#emitRootIncident(decision, facts),
    )
  }

  openScope(kind: IncidentScopeKind): IncidentScopeOwner {
    const owner = this.#scopeIssuer.open(kind, {
      factRecorded: (observation) => this.#onFact(observation),
      deadlineReached: (identity) => this.#pending.requestSeal(identity),
      scopeClosed: (identity) => this.#pending.requestSeal(identity),
    })
    this.#pending.register(owner.handle)
    return owner
  }

  submitDecision(
    scope: IncidentScopeHandle,
    decision: PresentationDecision,
  ): void {
    try {
      this.#pending.decide(scope, decision)
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

  #onFact(observation: FactObservation): void {
    this.#evidence?.record(observation.fact, observation.relation, observation.ref)
    if (this.#pending.record(observation.ref)) return
    if (observation.relation === 'consequence') {
      this.#reportLateConsequence(observation)
    }
  }

  #emitRootIncident(
    decision: IncidentPresentationDecision,
    facts: SealedFailureFacts,
  ): void {
    const reports = { consoleReports: 0 }
    const sequence = this.#allocateIncidentSequence()
    const timestamp = this.#readTimestamp()
    const initialOverflow =
      facts.contributorOverflowCount +
      facts.consequenceOverflowCount
    this.#health.recordFactOverflow(initialOverflow)
    this.#recordPendingHistoryEviction()
    const consoleAdmitted = this.#admitConsole(reports)
    const pendingLink = this.#prepareLateLink(
      facts.scope,
      sequence,
      timestamp.elapsedMs,
      reports.consoleReports,
    )
    const projection = this.#projectWithExactHealth({
      sequence,
      timestamp,
      presentation: decision,
      facts,
      initialOverflow,
    })

    this.#appendHistory(projection.record)
    pendingLink.commit(projection.record.runtime_run_id)
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

    root.accumulator ??= createFailureFactAccumulator(root.link.scope, this.#policy)
    root.firstObserved ??= timestamp
    const previous = root.accumulator.snapshotConsequences()
    root.accumulator.record(observation.ref)
    const facts = root.accumulator.snapshotConsequences()
    const { record, omittedBuckets } = projectIncidentConsequencesV2({
      runtimeRunId: root.runtimeRunId,
      incidentSequence: root.link.incidentSequence,
      scope: root.link.scope,
      firstObserved: root.firstObserved,
      lastObserved: timestamp,
      facts,
      policy: this.#policy,
    })
    let overflow = facts.overflowCount - previous.overflowCount
    // Byte pruning can change which causes fit. Account each cause independently
    // so an old omission returning to the export cannot mask a newly omitted fact.
    for (const bucket of omittedBuckets) {
      overflow += bucket.count - (root.omittedBucketCounts.get(bucket.representative) ?? 0n)
      root.omittedBucketCounts.set(bucket.representative, bucket.count)
    }
    this.#health.recordFactOverflow(overflow)
    try {
      if (!this.#history.recordConsequences(record)) {
        this.#lateLinks.delete(key)
        this.#health.recordLateLinkEviction()
        return
      }
    } catch { /* A fallible retention sink cannot suppress Console evidence. */ }
    const newCause = facts.buckets.length > previous.buckets.length ||
      (previous.overflowCount === 0n && facts.overflowCount > 0n)
    if (newCause && this.#admitConsole(root)) this.#reportConsole(record)
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
    scope: { consoleReports: number },
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
    return Object.freeze({
      commit: (runtimeRunId: string) => {
        this.#lateLinks.set(key, {
          runtimeRunId,
          link: Object.freeze({ incidentSequence: sequence, scope }),
          sealedAtElapsedMs: elapsedMs,
          consoleReports,
          omittedBucketCounts: new WeakMap(),
        })
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

  #appendHistory(record: IncidentRecordV2): void {
    try {
      this.#history.append(record)
    } catch {
      // History retention is independent from Console and trace markers.
    }
  }

  #reportConsole(record: IncidentRecordV2 | IncidentConsequencesV2): void {
    try {
      this.#consoleSink.error(record)
    } catch {
      // Developer Console failure cannot suppress the retained incident.
    }
  }

  #signalTrace(
    signal: TraceCaptureSignal<IncidentLink>,
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

function scopeKey(identity: IncidentScopeIdentity): string {
  return `${identity.scopeKind}:${identity.scopeSequence.toString(10)}`
}
