import type { OutputFailureSinks } from '../diagnostics'
import type { RecoverySummary } from '../file-system-access/recovery-summary'
import type { PersistentPausedFileRecovery } from '../persistent-tree/contracts'
import type { ReceiveLifecycleState } from '../workspace/state'
import type { DirectZipBootstrapResumeDescriptorV1 } from '../direct-zip/journal/repository'
import {
  assertReceiveOperationCanContinue,
  receiveOperationResumeDescriptor,
  type ReceiveOperationResumeDescriptor,
} from './descriptor'

export interface ResumeOperationClock {
  now(): number
}

export interface ReceiveOperationResumeSource {
  listDirectZipBootstrapCandidates?(): Promise<readonly DirectZipBootstrapResumeDescriptorV1[]>
  listLifecycleStates(): Promise<readonly ReceiveLifecycleState[]>
  isCleanupOnly?(operationId: string): Promise<boolean>
  readRecoverySummary?(
    lifecycle: Extract<ReceiveLifecycleState, {
      kind: 'resumable-receive'
      payloadKind: 'file-set'
    }>,
  ): Promise<RecoverySummary | undefined>
}

export interface ReceiveOperationResumeRequest {
  readonly retainedFileRecovery?: PersistentPausedFileRecovery
  readonly failures?: OutputFailureSinks
}

export type ReceiveOperationDiscardResult =
  | Readonly<{ kind: 'discarded'; cleanupReceiptDigest: string }>
  | Readonly<{ kind: 'partial-directory'; receiptDigest: string }>
  | Readonly<{
      kind: 'cleanup-completed'
      terminalState: 'published' | 'expired'
      cleanupReceiptDigest: string
    }>
  | Readonly<{ kind: 'already-absent' }>
  | Readonly<{ kind: 'record-forgotten' }>
  | Readonly<{
      kind: 'needs-attention'
      reason: 'target-ownership-unknown' | 'cleanup-unknown'
    }>

export interface ReceiveOperationMutationPort<TResult = unknown> {
  resume(
    descriptor: ReceiveOperationResumeDescriptor,
    request?: ReceiveOperationResumeRequest,
  ): Promise<TResult>
  expire(
    descriptor: ReceiveOperationResumeDescriptor,
    failures?: OutputFailureSinks,
  ): Promise<TResult>
  discard(
    descriptor: ReceiveOperationResumeDescriptor,
    failures?: OutputFailureSinks,
  ): Promise<ReceiveOperationDiscardResult>
  catchUp?(
    descriptor: ReceiveOperationResumeDescriptor,
    failures?: OutputFailureSinks,
  ): Promise<TResult>
}

interface ResumeReferenceOwner {
  open: boolean
}

export class ReceiveOperationResumeRef {
  readonly descriptor: ReceiveOperationResumeDescriptor
  readonly recoverySummary: RecoverySummary | undefined
  readonly #owner: ResumeReferenceOwner
  #consumed = false

  constructor(
    owner: ResumeReferenceOwner,
    descriptor: ReceiveOperationResumeDescriptor,
    recoverySummary?: RecoverySummary,
  ) {
    this.#owner = owner
    this.descriptor = descriptor
    this.recoverySummary = recoverySummary
  }

  consume(owner: ResumeReferenceOwner): ReceiveOperationResumeDescriptor {
    if (owner !== this.#owner || !owner.open) {
      throw new DOMException('Resume reference belongs to a closed inventory', 'InvalidStateError')
    }
    if (this.#consumed) {
      throw new DOMException('Resume reference was already consumed', 'InvalidStateError')
    }
    this.#consumed = true
    return this.descriptor
  }
}

export class ReceiveOperationResumeInventory {
  readonly operations: readonly ReceiveOperationResumeRef[]
  readonly directZipBootstrapCandidates: readonly DirectZipBootstrapResumeDescriptorV1[]
  readonly #owner: ResumeReferenceOwner

  constructor(
    owner: ResumeReferenceOwner,
    operations: readonly ReceiveOperationResumeRef[],
    directZipBootstrapCandidates: readonly DirectZipBootstrapResumeDescriptorV1[] = [],
  ) {
    this.#owner = owner
    this.operations = Object.freeze([...operations])
    this.directZipBootstrapCandidates = Object.freeze([...directZipBootstrapCandidates])
  }

  close(): void {
    this.#owner.open = false
  }
}

export class ReceiveOperationResumeAuthority<TResult = unknown> {
  readonly #source: ReceiveOperationResumeSource
  readonly #mutations: ReceiveOperationMutationPort<TResult>
  readonly #clock: ResumeOperationClock
  readonly #owners = new WeakMap<ReceiveOperationResumeRef, ResumeReferenceOwner>()

  constructor(input: {
    readonly source: ReceiveOperationResumeSource
    readonly mutations: ReceiveOperationMutationPort<TResult>
    readonly clock?: ResumeOperationClock
  }) {
    this.#source = input.source
    this.#mutations = input.mutations
    this.#clock = input.clock ?? SYSTEM_CLOCK
  }

  async listResumeState(): Promise<ReceiveOperationResumeInventory> {
    const now = this.#clock.now()
    // Pre-intent filesystem effects must be surfaced before intent-backed retained work.
    const directZipBootstrapCandidates = this.#source.listDirectZipBootstrapCandidates === undefined
      ? []
      : await this.#source.listDirectZipBootstrapCandidates()
    const lifecycles = await this.#source.listLifecycleStates()
    const owner: ResumeReferenceOwner = { open: true }
    const references: ReceiveOperationResumeRef[] = []
    for (const lifecycle of lifecycles) {
      const projected = receiveOperationResumeDescriptor(lifecycle, now)
      if (projected === undefined) continue
      const cleanupOnly = await this.#source.isCleanupOnly?.(lifecycle.operationId) ?? false
      const descriptor = cleanupOnly
        ? Object.freeze({ ...projected, continuation: 'cleanup-incompatible' as const })
        : projected
      const recoverySummary = !cleanupOnly && lifecycle.kind === 'resumable-receive' &&
          lifecycle.payloadKind === 'file-set' &&
          this.#source.readRecoverySummary !== undefined
        ? await this.#source.readRecoverySummary(lifecycle)
        : undefined
      requireMatchingRecoverySummary(lifecycle, recoverySummary)
      const reference = new ReceiveOperationResumeRef(owner, descriptor, recoverySummary)
      this.#owners.set(reference, owner)
      references.push(reference)
    }
    references.sort((left, right) =>
      left.descriptor.operationId.localeCompare(right.descriptor.operationId))
    return new ReceiveOperationResumeInventory(owner, references, directZipBootstrapCandidates)
  }

  async resume(
    reference: ReceiveOperationResumeRef,
    request?: ReceiveOperationResumeRequest,
  ): Promise<TResult> {
    requireMatchingRetainedFileRecovery(reference, request?.retainedFileRecovery)
    const descriptor = this.#consume(reference)
    const now = this.#clock.now()
    if (descriptor.continuation === 'cleanup-incompatible') {
      throw new DOMException('Incompatible saved records can only be forgotten', 'InvalidStateError')
    }
    if (descriptor.expiresAt !== undefined && now >= descriptor.expiresAt) {
      return this.#mutations.expire(descriptor, request?.failures)
    }
    assertReceiveOperationCanContinue(descriptor, now)
    return this.#mutations.resume(descriptor, request)
  }

  async discard(
    reference: ReceiveOperationResumeRef,
    failures?: OutputFailureSinks,
  ): Promise<ReceiveOperationDiscardResult> {
    const descriptor = this.#consume(reference)
    return this.#mutations.discard(descriptor, failures)
  }

  async cleanup(
    reference: ReceiveOperationResumeRef,
    failures?: OutputFailureSinks,
  ): Promise<TResult> {
    const descriptor = this.#consume(reference)
    if (descriptor.continuation !== 'cleanup-expired' &&
        descriptor.continuation !== 'retry-cleanup') {
      throw new DOMException('Receive operation has no retained cleanup authority', 'InvalidStateError')
    }
    // Expiry owns the cleanup-purpose reopen. Keeping that cut behind this
    // single-use reference prevents presentation from replaying a descriptor.
    return this.#mutations.expire(descriptor, failures)
  }

  async catchUp(
    reference: ReceiveOperationResumeRef,
    failures?: OutputFailureSinks,
  ): Promise<TResult> {
    const descriptor = this.#consume(reference)
    if (descriptor.continuation !== 'pending-catch-up' &&
        descriptor.continuation !== 'restoration-available' &&
        descriptor.continuation !== 'resume-receive' &&
        descriptor.continuation !== 'retry-cleanup') {
      throw new DOMException('Receive operation has no terminal catch-up authority', 'InvalidStateError')
    }
    const catchUp = this.#mutations.catchUp
    if (catchUp === undefined) {
      throw new DOMException('Terminal catch-up authority is unavailable', 'NotSupportedError')
    }
    return catchUp.call(this.#mutations, descriptor, failures)
  }

  #consume(reference: ReceiveOperationResumeRef): ReceiveOperationResumeDescriptor {
    const owner = this.#owners.get(reference)
    if (owner === undefined) {
      throw new DOMException('Resume reference belongs to another authority', 'InvalidStateError')
    }
    this.#owners.delete(reference)
    return reference.consume(owner)
  }
}

function requireMatchingRetainedFileRecovery(
  reference: ReceiveOperationResumeRef,
  retainedFileRecovery: PersistentPausedFileRecovery | undefined,
): void {
  if (reference.descriptor.continuation !== 'resume-receive') {
    if (retainedFileRecovery !== undefined) {
      throw new TypeError('retained file recovery is exclusive to receive continuation')
    }
    return
  }
  if (reference.recoverySummary === undefined && retainedFileRecovery !== undefined) {
    throw new TypeError('retained file recovery requires a validated recovery summary')
  }
  if (reference.recoverySummary !== undefined && retainedFileRecovery === undefined) {
    throw new TypeError('DirectTree continuation requires a retained-file recovery choice')
  }
}

function requireMatchingRecoverySummary(
  lifecycle: ReceiveLifecycleState,
  summary: RecoverySummary | undefined,
): void {
  if (summary === undefined) return
  if (lifecycle.kind !== 'resumable-receive' || lifecycle.payloadKind !== 'file-set' ||
      summary.lifecycleGeneration !== lifecycle.generation ||
      summary.checkpointSetDigest !== lifecycle.checkpointSetDigest ||
      summary.completedFileCount !== lifecycle.completedFileCount ||
      summary.completedBytes !== lifecycle.completedBytes) {
    throw new TypeError('recovery summary does not match its resume inventory lifecycle')
  }
}

const SYSTEM_CLOCK: ResumeOperationClock = Object.freeze({ now: () => Date.now() })
