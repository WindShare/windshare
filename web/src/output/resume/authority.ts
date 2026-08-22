import type { OutputFailureSinks } from '../diagnostics'
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
  | Readonly<{
      kind: 'needs-attention'
      reason: 'target-ownership-unknown' | 'cleanup-unknown'
    }>

export interface ReceiveOperationMutationPort<TResult = unknown> {
  resume(
    descriptor: ReceiveOperationResumeDescriptor,
    failures?: OutputFailureSinks,
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
  readonly #owner: ResumeReferenceOwner
  #consumed = false

  constructor(owner: ResumeReferenceOwner, descriptor: ReceiveOperationResumeDescriptor) {
    this.#owner = owner
    this.descriptor = descriptor
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
      const descriptor = receiveOperationResumeDescriptor(lifecycle, now)
      if (descriptor === undefined) continue
      const reference = new ReceiveOperationResumeRef(owner, descriptor)
      this.#owners.set(reference, owner)
      references.push(reference)
    }
    references.sort((left, right) =>
      left.descriptor.operationId.localeCompare(right.descriptor.operationId))
    return new ReceiveOperationResumeInventory(owner, references, directZipBootstrapCandidates)
  }

  async resume(
    reference: ReceiveOperationResumeRef,
    failures?: OutputFailureSinks,
  ): Promise<TResult> {
    const descriptor = this.#consume(reference)
    const now = this.#clock.now()
    if (descriptor.expiresAt !== undefined && now >= descriptor.expiresAt) {
      return this.#mutations.expire(descriptor, failures)
    }
    assertReceiveOperationCanContinue(descriptor, now)
    return this.#mutations.resume(descriptor, failures)
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
        descriptor.continuation !== 'retry-cleanup') {
      throw new DOMException('Receive operation has no terminal catch-up authority', 'InvalidStateError')
    }
    const catchUp = this.#mutations.catchUp
    if (catchUp === undefined) {
      throw new DOMException('Terminal catch-up authority is unavailable', 'NotSupportedError')
    }
    return catchUp(descriptor, failures)
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

const SYSTEM_CLOCK: ResumeOperationClock = Object.freeze({ now: () => Date.now() })
