import {
  evaluateCheckpointSchedule, snapshotAutomaticCheckpointPolicy,
  type AutomaticCheckpointPolicy, type AutomaticCheckpointTrigger,
} from '../checkpoint-schedule'
import { snapshotOutputCheckpointObject, type OutputCheckpointObject } from '../output-file-contract'

export interface CheckpointClock {
  now(): number
  schedule(callback: () => void, milliseconds: number): () => void
}

export const systemCheckpointClock: CheckpointClock = {
  now: () => Date.now(),
  schedule: (callback, milliseconds) => {
    const timer = setTimeout(callback, milliseconds)
    return () => clearTimeout(timer)
  },
}

// Keep measured flush work below one tenth of receiving time before increasing cadence.
const CHECKPOINT_COST_INTERVAL_MULTIPLIER = 10
const MINIMUM_RETRY_MILLISECONDS = 1_000
const MAXIMUM_TIMER_MILLISECONDS = 2_147_483_647

export interface CheckpointObservation {
  readonly objectId: string
  readonly stage: 'pending' | 'started' | 'advanced' | 'deferred' | 'finished' | 'failed'
  readonly pendingBytes: bigint
  readonly durableBytes: bigint
  readonly atMilliseconds: number
  readonly lastCheckpointMilliseconds?: number
  readonly durationMilliseconds?: number
  readonly checkpointBytes?: bigint
}

export interface CheckpointMemberInput {
  readonly object: OutputCheckpointObject
  readonly durableBytes: bigint
  readonly pendingBytes: bigint
  readonly remainingBytes: bigint
  readonly clock?: CheckpointClock
  readonly signal?: AbortSignal
  readonly observe?: (event: CheckpointObservation) => void
  readonly checkpoint: (trigger: AutomaticCheckpointTrigger) => Promise<
    Readonly<{ kind: 'advanced'; durableBytes: bigint }> |
    Readonly<{ kind: 'deferred' }> | Readonly<{ kind: 'finished' }>>
  readonly onAdvanced: (bytes: bigint) => void
  readonly onFailure: (error: unknown) => void
}

interface Member {
  readonly input: CheckpointMemberInput
  durableBytes: bigint
  pendingBytes: bigint
  remainingBytes: bigint
  closing: boolean
  finished: boolean
  lastCheckpointMilliseconds?: number
}

export interface FileCheckpointController {
  readonly durableBytes: bigint
  /** Serializes accepted writes with cuts on the actual shared storage object. */
  write(bytes: bigint, operation: () => Promise<void>): Promise<void>
  /** Removes due work and waits for already admitted work before terminal settlement. */
  drain(): Promise<void>
}

const sessionGroups = new WeakMap<object, Map<string, ObjectCheckpointGroup>>()

export function registerFileCheckpoint(session: object, input: CheckpointMemberInput): FileCheckpointController {
  input = { ...input, object: snapshotOutputCheckpointObject(input.object) }
  const objectId = input.object.objectId
  let groups = sessionGroups.get(session)
  if (groups === undefined) {
    groups = new Map()
    sessionGroups.set(session, groups)
  }
  let group = groups.get(objectId)
  if (group === undefined) {
    group = new ObjectCheckpointGroup(input.object.policy, input.clock ?? systemCheckpointClock,
      () => groups.delete(objectId))
    groups.set(objectId, group)
  }
  return group.enroll(input)
}

/** One queue makes an aggregate cut observable only after every member's accepted writes. */
class ObjectCheckpointGroup {
  readonly #policy: AutomaticCheckpointPolicy
  readonly #clock: CheckpointClock
  readonly #release: () => void
  readonly #members = new Set<Member>()
  #tail: Promise<unknown> = Promise.resolve()
  #cut: Promise<void> | undefined
  #cancelDue: (() => void) | undefined
  #dueAt: number | undefined
  #pendingSince: number | undefined
  #retryAfter = 0
  #retryAtPendingBytes = 0n
  #costInterval = 0
  #failure: Readonly<{ error: unknown }> | undefined

  constructor(policy: AutomaticCheckpointPolicy, clock: CheckpointClock, release: () => void) {
    this.#policy = snapshotAutomaticCheckpointPolicy(policy)
    this.#clock = clock
    this.#release = release
  }

  enroll(input: CheckpointMemberInput): FileCheckpointController {
    const policy = snapshotAutomaticCheckpointPolicy(input.object.policy)
    if (!samePolicy(this.#policy, policy)) throw new TypeError('Shared checkpoint object has conflicting cost policies')
    for (const bytes of [input.durableBytes, input.pendingBytes, input.remainingBytes]) {
      if (typeof bytes !== 'bigint' || bytes < 0n) throw new RangeError('Checkpoint coverage is invalid')
    }
    const member: Member = { input, durableBytes: input.durableBytes,
      pendingBytes: input.pendingBytes, remainingBytes: input.remainingBytes, closing: false, finished: false }
    this.#members.add(member)
    const abort = () => { member.closing = true; this.#arm() }
    input.signal?.addEventListener('abort', abort, { once: true })
    if (input.signal?.aborted === true) abort()
    if (member.pendingBytes > 0n) {
      this.#pendingSince ??= this.#now()
      this.#observe(member, 'pending')
    }
    this.#arm()
    if (member.pendingBytes > 0n) this.#check().catch(() => undefined)
    let draining: Promise<void> | undefined
    return {
      get durableBytes() { return member.durableBytes },
      write: async (bytes, operation) => {
        if (member.closing) throw new Error('Checkpoint member is draining')
        await this.#enqueue(async () => {
          if (this.#failure !== undefined) throw this.#failure.error
          await operation()
          if (bytes <= 0n || bytes > member.remainingBytes) throw new RangeError('Accepted checkpoint write exceeds remaining bytes')
          member.remainingBytes -= bytes
          const wasClean = member.pendingBytes === 0n
          member.pendingBytes += bytes
          this.#pendingSince ??= this.#now()
          if (wasClean) this.#observe(member, 'pending')
        })
        await this.#check()
      },
      drain: () => {
        draining ??= (async () => {
          member.closing = true
          this.#cancelTimer()
          await this.#tail
          this.#members.delete(member)
          input.signal?.removeEventListener('abort', abort)
          if (this.#members.size === 0) this.#release()
          else this.#arm()
          if (this.#failure !== undefined) throw this.#failure.error
        })()
        return draining
      },
    }
  }

  #enqueue<T>(operation: () => Promise<T>): Promise<T> {
    const result = this.#tail.then(operation)
    this.#tail = result.catch(() => undefined)
    return result
  }

  #active(): Member[] {
    return [...this.#members].filter(member => !member.closing && !member.finished)
  }

  #snapshot() {
    const members = this.#active()
    return {
      durableBytes: members.reduce((sum, member) => sum + member.durableBytes, 0n),
      pendingBytes: members.reduce((sum, member) => sum + member.pendingBytes, 0n),
      remainingBytes: members.reduce((sum, member) => sum + member.remainingBytes, 0n),
      pendingMilliseconds: this.#pendingSince === undefined ? 0 : Math.max(0, this.#now() - this.#pendingSince),
      retryAtPendingBytes: this.#retryAtPendingBytes,
    }
  }

  #check(): Promise<void> {
    if (this.#failure !== undefined) return Promise.reject(this.#failure.error)
    if (this.#cut !== undefined) return this.#cut
    const snapshot = this.#snapshot()
    const decision = evaluateCheckpointSchedule(this.#policy, snapshot)
    if (decision.kind === 'finish-without-further-checkpoint') {
      for (const member of this.#active()) member.finished = true
    }
    if (decision.kind !== 'checkpoint-now' ||
        (this.#policy.kind === 'incremental' && this.#now() < this.#retryAfter)) {
      this.#arm()
      return Promise.resolve()
    }
    this.#cancelTimer()
    const cut = this.#enqueue(async () => {
      const members = this.#active().filter(member => member.pendingBytes > 0n)
      if (members.length === 0) return
      const startedAt = this.#now()
      let deferred = false
      for (const member of members) {
        if (member.closing) continue
        deferred = await this.#checkpointMember(member, decision.trigger) || deferred
      }
      const now = this.#now()
      this.#costInterval = Math.ceil(Math.max(0, now - startedAt) * CHECKPOINT_COST_INTERVAL_MULTIPLIER)
      this.#retryAfter = now + (deferred && this.#policy.kind === 'incremental'
        ? Math.max(MINIMUM_RETRY_MILLISECONDS, this.#policy.pendingMilliseconds, this.#costInterval)
        : this.#costInterval)
      this.#retryAtPendingBytes = deferred && this.#policy.kind === 'prefix-copy'
        ? decision.retryAtPendingBytes : 0n
      this.#pendingSince = this.#active().some(member => member.pendingBytes > 0n) ? now : undefined
    })
    this.#cut = cut.catch(error => {
      this.#failure = { error }
      this.#cancelTimer()
      for (const member of this.#members) {
        this.#observe(member, 'failed')
        member.input.onFailure(error)
      }
      throw error
    }).finally(() => {
      this.#cut = undefined
      this.#arm()
    })
    return this.#cut
  }

  async #checkpointMember(member: Member, trigger: AutomaticCheckpointTrigger): Promise<boolean> {
    this.#observe(member, 'started')
    const memberStartedAt = this.#now()
    const result = await member.input.checkpoint(trigger)
    const durationMilliseconds = Math.max(0, this.#now() - memberStartedAt)
    if (result.kind === 'finished') {
      member.finished = true
      this.#observe(member, 'finished', { durationMilliseconds })
    } else if (result.kind === 'deferred') {
      this.#observe(member, 'deferred', { durationMilliseconds })
      return true
    } else {
      const advanced = result.durableBytes - member.durableBytes
      if (advanced !== member.pendingBytes || advanced <= 0n) {
        throw new Error('Checkpoint did not acknowledge exactly the accepted pending bytes')
      }
      member.durableBytes = result.durableBytes
      member.pendingBytes = 0n
      member.lastCheckpointMilliseconds = this.#now()
      this.#observe(member, 'advanced', { durationMilliseconds, checkpointBytes: advanced })
      member.input.onAdvanced(advanced)
    }
    return false
  }

  #arm(): void {
    if (this.#policy.kind !== 'incremental' || this.#cut !== undefined || this.#failure !== undefined) {
      this.#cancelTimer()
      return
    }
    const snapshot = this.#snapshot()
    if (snapshot.pendingBytes === 0n) this.#pendingSince = undefined
    if (snapshot.pendingBytes === 0n || snapshot.remainingBytes === 0n || this.#pendingSince === undefined) {
      this.#cancelTimer()
      return
    }
    const now = this.#now()
    const due = Math.max(snapshot.pendingBytes >= this.#policy.pendingBytes
      ? now : this.#pendingSince + this.#policy.pendingMilliseconds, this.#retryAfter)
    const delay = Math.min(MAXIMUM_TIMER_MILLISECONDS, Math.max(0, due - now))
    if (this.#cancelDue !== undefined && this.#dueAt === now + delay) return
    this.#cancelTimer()
    this.#dueAt = now + delay
    this.#cancelDue = this.#clock.schedule(() => {
      this.#cancelDue = undefined
      this.#dueAt = undefined
      // Failure also aborts blocked readers through the member's failure observer.
      this.#check().catch(() => undefined)
    }, delay)
  }

  #cancelTimer(): void {
    this.#cancelDue?.()
    this.#cancelDue = undefined
    this.#dueAt = undefined
  }

  #observe(member: Member, stage: CheckpointObservation['stage'],
    detail: { durationMilliseconds?: number; checkpointBytes?: bigint } = {}): void {
    try {
      member.input.observe?.({
        objectId: member.input.object.objectId, stage,
        pendingBytes: member.pendingBytes, durableBytes: member.durableBytes,
        atMilliseconds: this.#now(),
        ...(member.lastCheckpointMilliseconds === undefined ? {} : {
          lastCheckpointMilliseconds: member.lastCheckpointMilliseconds,
        }),
        ...detail,
      })
    } catch { /* Diagnostics cannot change checkpoint authority. */ }
  }

  #now(): number {
    const now = this.#clock.now()
    if (!Number.isFinite(now)) throw new RangeError('Checkpoint clock returned a non-finite time')
    return now
  }
}

function samePolicy(left: AutomaticCheckpointPolicy, right: AutomaticCheckpointPolicy): boolean {
  if (left.kind === 'disabled' || right.kind === 'disabled') return left.kind === right.kind
  return left.kind === right.kind && left.pendingBytes === right.pendingBytes &&
    (left.kind !== 'incremental' || (right.kind === 'incremental' &&
      left.pendingMilliseconds === right.pendingMilliseconds))
}
