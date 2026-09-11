import { NativeZipRecoveryUnavailableError } from './progressive-checkpoint'
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

export interface ReceiveOperationResumeSource {
  listDirectZipBootstrapCandidates?(): Promise<readonly DirectZipBootstrapResumeDescriptorV1[]>
  listLifecycleStates(): Promise<readonly ReceiveLifecycleState[]>
  readOperationIdentity?(lifecycle: ReceiveLifecycleState): Promise<Readonly<{
    display?: import('../workspace/operation-display').ReceiveOperationDisplay
    shareInstance: string
  }> | undefined>
  readProgressiveRequirement?(lifecycle: ReceiveLifecycleState): Promise<import('./progressive-checkpoint').ProgressiveZipRecoveryRequirement | undefined>
  readDirectZipRequirement?(lifecycle: ReceiveLifecycleState): Promise<import('./direct-zip-checkpoint').DirectZipRecoveryRequirement | undefined>
  readSourceRevisionFailures?(lifecycle: ReceiveLifecycleState): Promise<import('./source-revision-failures').SourceRevisionFailures | undefined>
  isCleanupOnly?(operationId: string): Promise<boolean>
  readRecoverySummary?(
    lifecycle: Extract<ReceiveLifecycleState, {
      kind: 'resumable-receive'
      payloadKind: 'file-set'
    }>,
  ): Promise<RecoverySummary | undefined>
}

export interface ReceiveOperationResumeRequest {
  readonly purpose?: 'partial-export'
  readonly retainedFileRecovery?: PersistentPausedFileRecovery
  readonly failures?: OutputFailureSinks
}

export type ReceiveOperationDiscardResult =
  | Readonly<{ kind: 'discarded'; cleanupReceiptDigest: string }>
  | Readonly<{ kind: 'partial-directory'; receiptDigest: string }>
  | Readonly<{
      kind: 'published-cleanup-completed'
      cleanupReceiptDigest: string
    }>
  | Readonly<{ kind: 'already-absent' }>
  | Readonly<{ kind: 'record-forgotten' }>
  | Readonly<{
      kind: 'needs-attention'
      reason: 'target-ownership-unknown' | 'cleanup-unknown'
    }>

export interface ReceiveOperationMutationPort<TResult = unknown> {
  forget?(descriptor: ReceiveOperationResumeDescriptor): Promise<void>
  resume(
    descriptor: ReceiveOperationResumeDescriptor,
    request?: ReceiveOperationResumeRequest,
  ): Promise<TResult>
  cleanup(
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
  readonly #owners = new WeakMap<ReceiveOperationResumeRef, ResumeReferenceOwner>()

  constructor(input: {
    readonly source: ReceiveOperationResumeSource
    readonly mutations: ReceiveOperationMutationPort<TResult>
  }) {
    this.#source = input.source
    this.#mutations = input.mutations
  }

  async listResumeState(): Promise<ReceiveOperationResumeInventory> {
    // Pre-intent filesystem effects must be surfaced before intent-backed retained work.
    const directZipBootstrapCandidates = this.#source.listDirectZipBootstrapCandidates === undefined
      ? []
      : await this.#source.listDirectZipBootstrapCandidates()
    const lifecycles = await this.#source.listLifecycleStates()
    const owner: ResumeReferenceOwner = { open: true }
    const references: ReceiveOperationResumeRef[] = []
    for (const lifecycle of lifecycles) {
      const projected = receiveOperationResumeDescriptor(lifecycle)
      if (projected === undefined) continue
      const cleanupOnly = await this.#source.isCleanupOnly?.(lifecycle.operationId) ?? false
      const identity = cleanupOnly ? undefined : await this.#source.readOperationIdentity?.(lifecycle)
      const descriptor = await this.#projectDescriptor(Object.freeze({ ...projected, ...identity }), lifecycle, cleanupOnly)
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
      (right.descriptor.display?.createdAtMilliseconds ?? 0) -
        (left.descriptor.display?.createdAtMilliseconds ?? 0) ||
      left.descriptor.operationId.localeCompare(right.descriptor.operationId))
    return new ReceiveOperationResumeInventory(owner, references, directZipBootstrapCandidates)
  }

  async #projectDescriptor(
    projected: ReceiveOperationResumeDescriptor, lifecycle: ReceiveLifecycleState, cleanupOnly: boolean,
  ): Promise<ReceiveOperationResumeDescriptor> {
    let descriptor = projected
    let nativeRequirement: import('./progressive-checkpoint').ProgressiveZipRecoveryRequirement | undefined
    try {
      nativeRequirement = cleanupOnly ? undefined : await this.#source.readProgressiveRequirement?.(lifecycle)
    } catch (error) {
      if (!(error instanceof NativeZipRecoveryUnavailableError)) throw error
      descriptor = Object.freeze({ ...projected, continuation: 'needs-attention',
        recoveryUnavailable: 'native-checkpoint-unavailable' })
    }
    if (cleanupOnly) descriptor = Object.freeze({ ...projected, continuation: 'cleanup-incompatible' as const })
    else if (nativeRequirement === 'local-finalization') {
      descriptor = Object.freeze({ ...projected, continuation: 'resume-local-finalization' as const })
    }
    if (!cleanupOnly) descriptor = await this.#projectDirectZipRequirement(descriptor, lifecycle)
    if (!cleanupOnly && descriptor.recoveryUnavailable === undefined) {
      const sourceRevisionFailures = await this.#source.readSourceRevisionFailures?.(lifecycle)
      if (sourceRevisionFailures !== undefined) descriptor = Object.freeze({ ...descriptor, sourceRevisionFailures })
    }
    return descriptor
  }

  async #projectDirectZipRequirement(
    descriptor: ReceiveOperationResumeDescriptor,
    lifecycle: ReceiveLifecycleState,
  ): Promise<ReceiveOperationResumeDescriptor> {
    const requirement = await this.#source.readDirectZipRequirement?.(lifecycle)
    if (requirement === undefined) return descriptor
    const continuations = {
      'verify-completion': 'verify-direct-zip-completion',
      published: 'history-only',
      receive: 'resume-direct-zip',
    } as const
    return Object.freeze({ ...descriptor, continuation: continuations[requirement] })
  }

  async resume(
    reference: ReceiveOperationResumeRef,
    request?: ReceiveOperationResumeRequest,
  ): Promise<TResult> {
    requireMatchingRetainedFileRecovery(reference, request?.retainedFileRecovery)
    const descriptor = this.#consume(reference)
    if (descriptor.continuation === 'cleanup-incompatible') {
      throw new DOMException('Incompatible saved records can only be forgotten', 'InvalidStateError')
    }
    assertReceiveOperationCanContinue(descriptor)
    return this.#mutations.resume(descriptor, request)
  }

  async forget(reference: ReceiveOperationResumeRef): Promise<void> {
    const forget = this.#mutations.forget
    if (forget === undefined) throw new DOMException('Download history removal is unavailable', 'NotSupportedError')
    await forget.call(this.#mutations, this.#consume(reference))
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
    if (descriptor.continuation !== 'retry-cleanup') {
      throw new DOMException('Receive operation has no retained cleanup authority', 'InvalidStateError')
    }
    // The single-use reference prevents presentation from replaying cleanup authority.
    return this.#mutations.cleanup(descriptor, failures)
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
