import type { AutomaticCheckpointTrigger } from '../../transfer/output-session'
import { AUTOMATIC_CHECKPOINT_PENDING_FLOOR_BYTES } from '../../transfer/checkpoint-schedule'
import type {
  AutomaticCheckpointAdmissionAuthority,
  AutomaticCheckpointAdmissionDecision,
  AutomaticCheckpointAdmissionSnapshot,
  AutomaticCheckpointBudgetHold,
  AutomaticCheckpointFileAdmission,
  CheckpointAttemptIdentity,
  CheckpointAuthorityObserver,
  CheckpointResourceReleaseReason,
  PreservingWriterCost,
} from './contracts'

const MEBIBYTE_BYTES = 1024n * 1024n
const GIBIBYTE_BYTES = 1024n * MEBIBYTE_BYTES

export const MAXIMUM_AUTOMATIC_PREFIX_COPY_BYTES = 128n * MEBIBYTE_BYTES
export const MAXIMUM_AUTOMATIC_WRITE_AMPLIFICATION_BYTES = 2n * GIBIBYTE_BYTES

export interface CreateAutomaticCheckpointAdmissionAuthorityOptions {
  readonly identity: CheckpointAttemptIdentity
  readonly observe?: CheckpointAuthorityObserver
}

interface FileAdmissionRecord {
  readonly owner: symbol
  readonly materializationRelativePath: readonly string[]
  readonly enrollmentOrder: number
  checkpointOrdinal: number
  retired: boolean
  finishedReason: AutomaticFinishedReason | undefined
  pending: PendingAdmissionRecord | undefined
  hold: BudgetHoldRecord | undefined
}

interface PendingAdmissionRecord {
  readonly trigger: AutomaticCheckpointTrigger
  readonly cost: PreservingWriterCost
}

type AutomaticFinishedReason = Extract<
  AutomaticCheckpointAdmissionDecision,
  { readonly kind: 'finished' }
>['reason']

type HoldState = 'tentative' | 'committed' | 'released'

interface BudgetHoldRecord {
  readonly file: FileAdmissionRecord
  readonly trigger: AutomaticCheckpointTrigger
  readonly cost: PreservingWriterCost
  readonly public: AutomaticCheckpointBudgetHold
  state: HoldState
}

export function createAutomaticCheckpointAdmissionAuthority(
  options: CreateAutomaticCheckpointAdmissionAuthorityOptions,
): AutomaticCheckpointAdmissionAuthority {
  return new AttemptAutomaticCheckpointAdmissionAuthority(options)
}

class AttemptAutomaticCheckpointAdmissionAuthority
implements AutomaticCheckpointAdmissionAuthority {
  readonly #identity: CheckpointAttemptIdentity
  readonly #observe: CheckpointAuthorityObserver | undefined
  readonly #owner = Symbol('automatic-checkpoint-admission-authority')
  readonly #files = new Set<FileAdmissionRecord>()
  #nextEnrollmentOrder = 0
  #committedWriteAmplificationBytes = 0n
  #cumulativelyExhausted = false
  #accepting = true

  constructor(options: CreateAutomaticCheckpointAdmissionAuthorityOptions) {
    this.#identity = snapshotAttemptIdentity(options.identity)
    this.#observe = options.observe
  }

  enrollFile(materializationRelativePath: readonly string[]): AutomaticCheckpointFileAdmission {
    this.#requireAccepting()
    const record: FileAdmissionRecord = {
      owner: this.#owner,
      materializationRelativePath: snapshotPath(materializationRelativePath),
      enrollmentOrder: this.#nextEnrollmentOrder,
      checkpointOrdinal: 1,
      retired: false,
      finishedReason: undefined,
      pending: undefined,
      hold: undefined,
    }
    this.#nextEnrollmentOrder += 1
    this.#files.add(record)
    return Object.freeze({
      materializationRelativePath: record.materializationRelativePath,
      request: (trigger: AutomaticCheckpointTrigger, cost: PreservingWriterCost) =>
        this.#request(record, trigger, cost),
      retire: (reason: CheckpointResourceReleaseReason = 'file-retired') =>
        this.#retire(record, reason),
    })
  }

  close(reason: CheckpointResourceReleaseReason = 'terminal-drain'): void {
    if (!this.#accepting) return
    this.#accepting = false
    for (const file of [...this.#files]) this.#retire(file, reason)
    this.#files.clear()
  }

  snapshot(): AutomaticCheckpointAdmissionSnapshot {
    return Object.freeze({
      accepting: this.#accepting,
      enrolledFiles: this.#files.size,
      committedWriteAmplificationBytes: this.#committedWriteAmplificationBytes,
      remainingWriteAmplificationBytes: this.#remainingWriteAmplificationBytes(),
      tentativeHolds: [...this.#files].filter(file => file.hold?.state === 'tentative').length,
      cumulativelyExhausted: this.#cumulativelyExhausted,
    })
  }

  #request(
    file: FileAdmissionRecord,
    trigger: AutomaticCheckpointTrigger,
    inputCost: PreservingWriterCost,
  ): AutomaticCheckpointAdmissionDecision {
    this.#requireOwnedActiveFile(file)
    requireAutomaticTrigger(trigger)
    const cost = snapshotPreservingWriterCost(inputCost)
    const sticky = file.finishedReason ??
      (this.#cumulativelyExhausted ? 'cumulative-write-amplification-budget' : undefined)
    if (sticky !== undefined) {
      file.finishedReason = sticky
      file.pending = undefined
      return this.#finished(file, trigger, cost, sticky)
    }

    if (file.hold !== undefined) return this.#deferred(file, trigger, cost)

    if (cost.prefixCopyBytes > MAXIMUM_AUTOMATIC_PREFIX_COPY_BYTES) {
      file.finishedReason = 'prefix-copy-budget'
      file.pending = undefined
      return this.#finished(file, trigger, cost, file.finishedReason)
    }
    const committedRemaining = this.#committedRemainingWriteAmplificationBytes()
    if (cost.writeAmplificationBytes > committedRemaining) {
      file.finishedReason = 'cumulative-write-amplification-budget'
      file.pending = undefined
      return this.#finished(file, trigger, cost, 'cumulative-write-amplification-budget')
    }
    file.pending = Object.freeze({ trigger, cost })
    const available = this.#remainingWriteAmplificationBytes()
    if (cost.writeAmplificationBytes > available ||
        !this.#hasPriorityUnderBudgetPressure(file, available, committedRemaining)) {
      return this.#deferred(file, trigger, cost)
    }
    file.pending = undefined
    return this.#admit(file, trigger, cost)
  }

  #admit(
    file: FileAdmissionRecord,
    trigger: AutomaticCheckpointTrigger,
    cost: PreservingWriterCost,
  ): AutomaticCheckpointAdmissionDecision {
    const hold = {} as AutomaticCheckpointBudgetHold
    const record: BudgetHoldRecord = {
      file,
      trigger,
      cost,
      public: hold,
      state: 'tentative',
    }
    Object.assign(hold, {
      checkpointOrdinal: file.checkpointOrdinal,
      cost,
      commit: () => this.#commitHold(record),
      release: (reason: CheckpointResourceReleaseReason) => this.#releaseHold(record, reason),
    })
    Object.freeze(hold)
    file.hold = record
    this.#emit(file, trigger, cost, 'admitted')
    return Object.freeze({
      kind: 'admitted',
      hold,
      remainingWriteAmplificationBytes: this.#remainingWriteAmplificationBytes(),
    })
  }

  #deferred(
    file: FileAdmissionRecord,
    trigger: AutomaticCheckpointTrigger,
    cost: PreservingWriterCost,
  ): AutomaticCheckpointAdmissionDecision {
    this.#emit(file, trigger, cost, 'checkpoint-priority')
    return Object.freeze({
      kind: 'deferred',
      reason: 'checkpoint-priority',
      estimate: cost,
      remainingWriteAmplificationBytes: this.#remainingWriteAmplificationBytes(),
    })
  }

  #finished(
    file: FileAdmissionRecord,
    trigger: AutomaticCheckpointTrigger,
    cost: PreservingWriterCost,
    reason: AutomaticFinishedReason,
  ): AutomaticCheckpointAdmissionDecision {
    this.#emit(file, trigger, cost, reason)
    return Object.freeze({
      kind: 'finished',
      reason,
      estimate: cost,
      remainingWriteAmplificationBytes: this.#remainingWriteAmplificationBytes(),
    })
  }

  #commitHold(hold: BudgetHoldRecord): void {
    if (hold.state === 'committed') return
    if (hold.state !== 'tentative') throw settledHoldError('commit')
    this.#requireOwnedActiveFile(hold.file)
    if (hold.file.hold !== hold) throw settledHoldError('commit')
    hold.state = 'committed'
    hold.file.hold = undefined
    this.#committedWriteAmplificationBytes += hold.cost.writeAmplificationBytes
    hold.file.checkpointOrdinal += 1
    if (this.#committedRemainingWriteAmplificationBytes() <
        AUTOMATIC_CHECKPOINT_PENDING_FLOOR_BYTES) {
      this.#cumulativelyExhausted = true
    }
    this.#emit(hold.file, hold.trigger, hold.cost, 'committed')
  }

  #releaseHold(hold: BudgetHoldRecord, reason: CheckpointResourceReleaseReason): void {
    requireReleaseReason(reason)
    if (hold.state === 'released' || hold.state === 'committed') return
    hold.state = 'released'
    if (hold.file.hold === hold) hold.file.hold = undefined
    if (reason === 'capacity-unavailable' && !hold.file.retired) {
      hold.file.pending = Object.freeze({ trigger: hold.trigger, cost: hold.cost })
    }
    this.#emit(hold.file, hold.trigger, hold.cost, 'released', reason)
  }

  #retire(file: FileAdmissionRecord, reason: CheckpointResourceReleaseReason): void {
    requireReleaseReason(reason)
    if (file.retired) return
    this.#requireOwnedFile(file)
    const hold = file.hold
    if (hold !== undefined) this.#releaseHold(hold, reason)
    file.retired = true
    file.pending = undefined
    this.#files.delete(file)
  }

  #hasPriorityUnderBudgetPressure(
    file: FileAdmissionRecord,
    availableBytes: bigint,
    committedRemainingBytes: bigint,
  ): boolean {
    const pending = this.#activeFiles().filter(candidate =>
      candidate.pending !== undefined &&
      candidate.pending.cost.writeAmplificationBytes <= committedRemainingBytes)
    const requestedBytes = pending.reduce(
      (total, candidate) => total + candidate.pending!.cost.writeAmplificationBytes,
      0n,
    )
    if (requestedBytes <= availableBytes) return true
    pending.sort(comparePriority)
    return pending[0] === file
  }

  #activeFiles(): FileAdmissionRecord[] {
    return [...this.#files].filter(file => !file.retired && file.finishedReason === undefined)
  }

  #remainingWriteAmplificationBytes(): bigint {
    const tentative = [...this.#files].reduce(
      (total, file) => total +
        (file.hold?.state === 'tentative' ? file.hold.cost.writeAmplificationBytes : 0n),
      0n,
    )
    const remaining = MAXIMUM_AUTOMATIC_WRITE_AMPLIFICATION_BYTES -
      this.#committedWriteAmplificationBytes - tentative
    return remaining > 0n ? remaining : 0n
  }

  #committedRemainingWriteAmplificationBytes(): bigint {
    const remaining = MAXIMUM_AUTOMATIC_WRITE_AMPLIFICATION_BYTES -
      this.#committedWriteAmplificationBytes
    return remaining > 0n ? remaining : 0n
  }

  #emit(
    file: FileAdmissionRecord,
    trigger: AutomaticCheckpointTrigger,
    cost: PreservingWriterCost,
    decision: Parameters<CheckpointAuthorityObserver>[0]['decision'],
    releaseReason?: CheckpointResourceReleaseReason,
  ): void {
    if (this.#observe === undefined) return
    try {
      this.#observe(Object.freeze({
        authority: 'automatic-admission',
        ...this.#identity,
        materializationRelativePath: file.materializationRelativePath,
        trigger,
        checkpointOrdinal: file.checkpointOrdinal,
        cost,
        remainingAutomaticWriteAmplificationBytes: this.#remainingWriteAmplificationBytes(),
        decision,
        ...(releaseReason === undefined ? {} : { releaseReason }),
      }))
    } catch {
      // Diagnostic consumers must never acquire control of admission or accounting.
    }
  }

  #requireAccepting(): void {
    if (!this.#accepting) throw authorityClosedError('Automatic checkpoint admission')
  }

  #requireOwnedFile(file: FileAdmissionRecord): void {
    if (file.owner !== this.#owner || !this.#files.has(file)) {
      throw new TypeError('Automatic checkpoint file admission belongs to another authority')
    }
  }

  #requireOwnedActiveFile(file: FileAdmissionRecord): void {
    this.#requireAccepting()
    if (file.retired) throw authorityClosedError('Automatic checkpoint file admission')
    this.#requireOwnedFile(file)
  }
}

function comparePriority(left: FileAdmissionRecord, right: FileAdmissionRecord): number {
  return left.checkpointOrdinal - right.checkpointOrdinal ||
    left.enrollmentOrder - right.enrollmentOrder
}

export function snapshotPreservingWriterCost(cost: PreservingWriterCost): PreservingWriterCost {
  return Object.freeze({
    prefixCopyBytes: requireNonNegativeBytes(cost?.prefixCopyBytes, 'prefix copy'),
    writeAmplificationBytes: requireNonNegativeBytes(
      cost?.writeAmplificationBytes,
      'write amplification',
    ),
    temporaryBytes: requireNonNegativeBytes(cost?.temporaryBytes, 'temporary space'),
  })
}

function snapshotAttemptIdentity(identity: CheckpointAttemptIdentity): CheckpointAttemptIdentity {
  return Object.freeze({
    receiveOperationId: requireIdentity(identity?.receiveOperationId, 'receive operation'),
    transferJobId: requireIdentity(identity?.transferJobId, 'transfer job'),
    outputSessionId: requireIdentity(identity?.outputSessionId, 'output session'),
  })
}

function snapshotPath(path: readonly string[]): readonly string[] {
  if (!Array.isArray(path) || path.some(part => typeof part !== 'string' || part.length === 0)) {
    throw new TypeError('Checkpoint materialization path is invalid')
  }
  return Object.freeze([...path])
}

function requireIdentity(value: string, label: string): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw new TypeError(`Checkpoint ${label} identity is required`)
  }
  return value
}

function requireNonNegativeBytes(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n) {
    throw new TypeError(`Preserving writer ${label} bytes must not be negative`)
  }
  return value
}

function requireAutomaticTrigger(trigger: AutomaticCheckpointTrigger): void {
  if (trigger !== 'pending-bytes' && trigger !== 'pending-time') {
    throw new TypeError('Automatic checkpoint trigger is invalid')
  }
}

function requireReleaseReason(reason: CheckpointResourceReleaseReason): void {
  if (typeof reason !== 'string' || reason.length === 0) {
    throw new TypeError('Checkpoint resource release reason is required')
  }
}

function authorityClosedError(label: string): DOMException {
  return new DOMException(`${label} is closed`, 'InvalidStateError')
}

function settledHoldError(action: string): DOMException {
  return new DOMException(`Automatic checkpoint budget hold cannot ${action} after release`, 'InvalidStateError')
}
