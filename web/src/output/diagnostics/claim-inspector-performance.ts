import {
  PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1,
  type PerformanceClaimInspectorReasonV1,
} from '../../diagnostics/trace/transfer-payload'
import {
  performanceNowMilliseconds,
  type PerformanceSummaryObservations,
} from './performance-summary'

export interface PerformanceClaimInspectorState {
  readonly active: number
  readonly queuedMembers: number
}

export interface PerformanceClaimInspectorContextObservation {
  inspectionStarted(): void
  inspectionFinished(): void
  settlementStarted(): void
  finish(): void
}

export interface PerformanceClaimInspectorSample {
  readonly drainMilliseconds: number
  readonly maximumWidth: number
  readonly atCapacityMilliseconds: bigint
  readonly activeMilliseconds: bigint
  readonly queuedMemberMilliseconds: bigint
  readonly pendingMemberMilliseconds: bigint
  readonly residentContextMilliseconds: bigint
  readonly unfinishedInspectionContextMilliseconds: bigint
  readonly orderedSettlementContextMilliseconds: bigint
  readonly maximumActive: number
  readonly maximumQueuedMembers: number
  readonly maximumPendingMembers: number
  readonly maximumResidentContexts: number
  readonly maximumUnfinishedInspectionContexts: number
  readonly maximumOrderedSettlementContexts: number
  readonly activeAtCompletion: number
  readonly queuedMembersAtCompletion: number
  readonly pendingMembersAtCompletion: number
  readonly residentContextsAtCompletion: number
  readonly unfinishedInspectionContextsAtCompletion: number
  readonly orderedSettlementContextsAtCompletion: number
  readonly underCapacity: Readonly<Record<PerformanceClaimInspectorReasonV1, Readonly<{
    wallMilliseconds: bigint
    idleSlotMilliseconds: bigint
  }>>>
}

export interface PerformanceClaimInspectorObservation {
  observePoolState(state: PerformanceClaimInspectorState): void
  setPendingMembers(pendingMembers: number): void
  beginContext(): PerformanceClaimInspectorContextObservation
  complete(): PerformanceClaimInspectorSample | undefined
}

interface InspectorState {
  active: number
  queuedMembers: number
  pendingMembers: number
  residentContexts: number
  unfinishedInspectionContexts: number
  orderedSettlementContexts: number
}

interface ContextState {
  inspectionStarted: boolean
  inspectionFinished: boolean
  settlementStarted: boolean
  finished: boolean
}

export function createPerformanceClaimInspectorObservation(
  performance: PerformanceSummaryObservations | undefined,
  maximumWidth: number,
  startedAtMilliseconds: number | undefined,
): PerformanceClaimInspectorObservation | undefined {
  if (performance === undefined || startedAtMilliseconds === undefined) return undefined
  return new ClaimInspectorObservation(
    performance,
    requireCount(maximumWidth, 'claim inspector maximum width', 1),
    startedAtMilliseconds,
  )
}

class ClaimInspectorObservation implements PerformanceClaimInspectorObservation {
  readonly #performance: PerformanceSummaryObservations
  readonly #maximumWidth: number
  readonly #startedAtMilliseconds: number
  readonly #state: InspectorState = {
    active: 0,
    queuedMembers: 0,
    pendingMembers: 0,
    residentContexts: 0,
    unfinishedInspectionContexts: 0,
    orderedSettlementContexts: 0,
  }
  readonly #underCapacity = Object.fromEntries(
    PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1.map(reason => [
      reason,
      { wallMilliseconds: 0n, idleSlotMilliseconds: 0n },
    ]),
  ) as Record<PerformanceClaimInspectorReasonV1, {
    wallMilliseconds: bigint
    idleSlotMilliseconds: bigint
  }>
  #lastAtMilliseconds: number
  #atCapacityMilliseconds = 0n
  #activeMilliseconds = 0n
  #queuedMemberMilliseconds = 0n
  #pendingMemberMilliseconds = 0n
  #residentContextMilliseconds = 0n
  #unfinishedInspectionContextMilliseconds = 0n
  #orderedSettlementContextMilliseconds = 0n
  #maximumActive = 0
  #maximumQueuedMembers = 0
  #maximumPendingMembers = 0
  #maximumResidentContexts = 0
  #maximumUnfinishedInspectionContexts = 0
  #maximumOrderedSettlementContexts = 0
  #disabled = false
  #completed = false

  constructor(
    performance: PerformanceSummaryObservations,
    maximumWidth: number,
    startedAtMilliseconds: number,
  ) {
    this.#performance = performance
    this.#maximumWidth = maximumWidth
    this.#startedAtMilliseconds = startedAtMilliseconds
    this.#lastAtMilliseconds = startedAtMilliseconds
  }

  observePoolState(state: PerformanceClaimInspectorState): void {
    if (!this.#advance()) return
    this.#state.active = requireCount(state.active, 'active claim inspections')
    this.#state.queuedMembers = requireCount(state.queuedMembers, 'queued claim inspections')
    if (this.#state.active > this.#maximumWidth) {
      this.#disabled = true
      return
    }
    this.#maximumActive = Math.max(this.#maximumActive, this.#state.active)
    this.#maximumQueuedMembers = Math.max(
      this.#maximumQueuedMembers,
      this.#state.queuedMembers,
    )
  }

  setPendingMembers(pendingMembers: number): void {
    if (!this.#advance()) return
    this.#state.pendingMembers = requireCount(pendingMembers, 'pending claim members')
    this.#maximumPendingMembers = Math.max(
      this.#maximumPendingMembers,
      this.#state.pendingMembers,
    )
  }

  beginContext(): PerformanceClaimInspectorContextObservation {
    if (!this.#advance()) return disabledContextObservation()
    this.#state.residentContexts += 1
    this.#maximumResidentContexts = Math.max(
      this.#maximumResidentContexts,
      this.#state.residentContexts,
    )
    const context: ContextState = {
      inspectionStarted: false,
      inspectionFinished: false,
      settlementStarted: false,
      finished: false,
    }
    return Object.freeze({
      inspectionStarted: () => this.#inspectionStarted(context),
      inspectionFinished: () => this.#inspectionFinished(context),
      settlementStarted: () => this.#settlementStarted(context),
      finish: () => this.#finishContext(context),
    })
  }

  complete(): PerformanceClaimInspectorSample | undefined {
    if (this.#completed || !this.#advance() || this.#disabled) return undefined
    this.#completed = true
    return Object.freeze({
      drainMilliseconds: this.#lastAtMilliseconds - this.#startedAtMilliseconds,
      maximumWidth: this.#maximumWidth,
      atCapacityMilliseconds: this.#atCapacityMilliseconds,
      activeMilliseconds: this.#activeMilliseconds,
      queuedMemberMilliseconds: this.#queuedMemberMilliseconds,
      pendingMemberMilliseconds: this.#pendingMemberMilliseconds,
      residentContextMilliseconds: this.#residentContextMilliseconds,
      unfinishedInspectionContextMilliseconds: this.#unfinishedInspectionContextMilliseconds,
      orderedSettlementContextMilliseconds: this.#orderedSettlementContextMilliseconds,
      maximumActive: this.#maximumActive,
      maximumQueuedMembers: this.#maximumQueuedMembers,
      maximumPendingMembers: this.#maximumPendingMembers,
      maximumResidentContexts: this.#maximumResidentContexts,
      maximumUnfinishedInspectionContexts: this.#maximumUnfinishedInspectionContexts,
      maximumOrderedSettlementContexts: this.#maximumOrderedSettlementContexts,
      activeAtCompletion: this.#state.active,
      queuedMembersAtCompletion: this.#state.queuedMembers,
      pendingMembersAtCompletion: this.#state.pendingMembers,
      residentContextsAtCompletion: this.#state.residentContexts,
      unfinishedInspectionContextsAtCompletion: this.#state.unfinishedInspectionContexts,
      orderedSettlementContextsAtCompletion: this.#state.orderedSettlementContexts,
      underCapacity: Object.freeze(Object.fromEntries(
        PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1.map(reason => [
          reason,
          Object.freeze({ ...this.#underCapacity[reason] }),
        ]),
      ) as PerformanceClaimInspectorSample['underCapacity']),
    })
  }

  #inspectionStarted(context: ContextState): void {
    if (context.finished || context.inspectionStarted || !this.#advance()) return
    context.inspectionStarted = true
    this.#state.unfinishedInspectionContexts += 1
    this.#maximumUnfinishedInspectionContexts = Math.max(
      this.#maximumUnfinishedInspectionContexts,
      this.#state.unfinishedInspectionContexts,
    )
  }

  #inspectionFinished(context: ContextState): void {
    if (context.finished || !context.inspectionStarted || context.inspectionFinished ||
        !this.#advance()) return
    context.inspectionFinished = true
    this.#state.unfinishedInspectionContexts -= 1
    if (!context.settlementStarted) {
      this.#state.orderedSettlementContexts += 1
      this.#maximumOrderedSettlementContexts = Math.max(
        this.#maximumOrderedSettlementContexts,
        this.#state.orderedSettlementContexts,
      )
    }
  }

  #settlementStarted(context: ContextState): void {
    if (context.finished || context.settlementStarted || !this.#advance()) return
    context.settlementStarted = true
    if (context.inspectionFinished) this.#state.orderedSettlementContexts -= 1
  }

  #finishContext(context: ContextState): void {
    if (context.finished || !this.#advance()) return
    context.finished = true
    if (context.inspectionStarted && !context.inspectionFinished) {
      this.#state.unfinishedInspectionContexts -= 1
    }
    if (context.inspectionFinished && !context.settlementStarted) {
      this.#state.orderedSettlementContexts -= 1
    }
    this.#state.residentContexts -= 1
  }

  #advance(): boolean {
    if (this.#completed || this.#disabled) return false
    const observed = performanceNowMilliseconds(this.#performance)
    if (observed === undefined) {
      this.#disabled = true
      return false
    }
    const atMilliseconds = Math.max(this.#lastAtMilliseconds, observed)
    const elapsed = BigInt(atMilliseconds - this.#lastAtMilliseconds)
    const idleSlots = this.#maximumWidth - this.#state.active
    this.#activeMilliseconds += BigInt(this.#state.active) * elapsed
    this.#queuedMemberMilliseconds += BigInt(this.#state.queuedMembers) * elapsed
    this.#pendingMemberMilliseconds += BigInt(this.#state.pendingMembers) * elapsed
    this.#residentContextMilliseconds += BigInt(this.#state.residentContexts) * elapsed
    this.#unfinishedInspectionContextMilliseconds +=
      BigInt(this.#state.unfinishedInspectionContexts) * elapsed
    this.#orderedSettlementContextMilliseconds +=
      BigInt(this.#state.orderedSettlementContexts) * elapsed
    if (idleSlots === 0) {
      this.#atCapacityMilliseconds += elapsed
    } else {
      const reason = this.#underCapacityReason()
      this.#underCapacity[reason].wallMilliseconds += elapsed
      this.#underCapacity[reason].idleSlotMilliseconds += BigInt(idleSlots) * elapsed
    }
    this.#lastAtMilliseconds = atMilliseconds
    return true
  }

  #underCapacityReason(): PerformanceClaimInspectorReasonV1 {
    // Pending work behind the one resident batch is intentionally serialized;
    // this reason must not imply that a configurable preparation lane exists.
    if (this.#state.pendingMembers > 0 && this.#state.residentContexts > 0) {
      return 'batch_serialization'
    }
    if (this.#state.orderedSettlementContexts > 0) return 'ordered_settlement'
    return 'no_pending_arrival'
  }
}

function requireCount(value: number, field: string, minimum = 0): number {
  if (!Number.isSafeInteger(value) || value < minimum) {
    throw new TypeError(`${field} is invalid`)
  }
  return value
}

function disabledContextObservation(): PerformanceClaimInspectorContextObservation {
  return Object.freeze({
    inspectionStarted: () => undefined,
    inspectionFinished: () => undefined,
    settlementStarted: () => undefined,
    finish: () => undefined,
  })
}
