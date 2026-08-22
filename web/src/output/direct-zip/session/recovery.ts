import type {
  DirectZipLifecycleDecision,
  DirectZipReopenResult,
  DirectZipTargetPort,
} from '../target'
import type {
  DirectZipRecoveryLifecyclePort,
  DirectZipRecoveryTargetInput,
  DirectZipSessionTrace,
} from './model'
import {
  preflightDirectZipDestinationSpace,
  type DirectZipDestinationSpacePort,
} from './preflight'

export interface DirectZipRecoverySessionInput<ParentHandle, FileHandle, Runtime>
  extends DirectZipRecoveryTargetInput<ParentHandle, FileHandle> {
  readonly target: DirectZipTargetPort<ParentHandle, FileHandle>
  readonly lifecycle: DirectZipRecoveryLifecyclePort<FileHandle, Runtime>
  readonly trustedAction: boolean
  readonly projectedArchiveLength: bigint
  readonly additionalTemporaryBytesUpperBound: bigint
  readonly space: DirectZipDestinationSpacePort<ParentHandle>
  readonly trace?: DirectZipSessionTrace
}

export interface DirectZipRetainedCleanupInput<ParentHandle, FileHandle, Runtime>
  extends DirectZipRecoveryTargetInput<ParentHandle, FileHandle> {
  readonly target: DirectZipTargetPort<ParentHandle, FileHandle>
  readonly lifecycle: DirectZipRecoveryLifecyclePort<FileHandle, Runtime>
  readonly trace?: DirectZipSessionTrace
}

export type DirectZipRecoverySessionResult<ParentHandle, FileHandle, Runtime> =
  | Readonly<{
      readonly kind: 'active'
      readonly session: DirectZipActiveSession<ParentHandle, FileHandle, Runtime>
    }>
  | Readonly<{
      readonly kind: 'gated'
      readonly lifecycle: import('../../workspace/state').ReceiveLifecycleState
      readonly decision: DirectZipLifecycleDecision
      readonly verify?: () => Promise<DirectZipRecoverySessionResult<ParentHandle, FileHandle, Runtime>>
    }>

export class DirectZipActiveSession<ParentHandle, FileHandle, Runtime> {
  readonly #input: DirectZipRecoverySessionInput<ParentHandle, FileHandle, Runtime>
  readonly #runtime: Runtime
  #open = true

  constructor(
    input: DirectZipRecoverySessionInput<ParentHandle, FileHandle, Runtime>,
    runtime: Runtime,
  ) {
    this.#input = input
    this.#runtime = runtime
  }

  get runtime(): Runtime {
    if (!this.#open) throw new DOMException('Direct ZIP session is closed', 'InvalidStateError')
    return this.#runtime
  }

  async pause(signal: AbortSignal): Promise<import('../../workspace/state').ReceiveLifecycleState> {
    if (!this.#open) throw new DOMException('Direct ZIP session is closed', 'InvalidStateError')
    signal.throwIfAborted()
    const lifecycle = await this.#input.lifecycle.pause(this.#runtime, signal)
    this.#open = false
    emit(this.#input, 'pause-committed', 'retained', lifecycle.generation)
    return lifecycle
  }

  async retain(): Promise<void> {
    if (!this.#open) return
    await this.#input.lifecycle.retain(this.#runtime)
    this.#open = false
    emit(this.#input, 'operation-retained', 'retained')
  }

  /** Durable records are removed only after target proof and exact-name deletion succeed. */
  async delete(trustedAction: boolean): Promise<DirectZipRecoverySessionResult<ParentHandle, FileHandle, Runtime> | Readonly<{ kind: 'deleted' }>> {
    if (!this.#open) throw new DOMException('Direct ZIP session is closed', 'InvalidStateError')
    await this.#input.lifecycle.prepareCleanup(this.#runtime)
    this.#open = false
    const deleted = await this.#input.target.deleteProvenTarget({
      binding: this.#input.binding,
      currentParent: this.#input.currentParent,
      predecessor: this.#input.predecessor,
      ...(this.#input.candidate === undefined ? {} : { candidate: this.#input.candidate }),
      trustedAction,
    })
    if (deleted.kind === 'gated') return gate(this.#input, deleted.decision)
    await this.#input.lifecycle.deleteOwnedTarget()
    emit(this.#input, 'cleanup-verified', deleted.value.disposition)
    return Object.freeze({ kind: 'deleted' })
  }
}

/** Expiry cleanup proves and removes the owned target without reopening transfer execution. */
export async function deleteRetainedDirectZipSession<ParentHandle, FileHandle, Runtime>(
  input: DirectZipRetainedCleanupInput<ParentHandle, FileHandle, Runtime>,
  trustedAction: boolean,
  signal: AbortSignal,
): Promise<DirectZipRecoverySessionResult<ParentHandle, FileHandle, Runtime> | Readonly<{ kind: 'deleted' }>> {
  requireOperationBinding(input)
  signal.throwIfAborted()
  await input.lifecycle.prepareRetainedCleanup(signal)
  signal.throwIfAborted()
  const deleted = await input.target.deleteProvenTarget({
    binding: input.binding,
    currentParent: input.currentParent,
    predecessor: input.predecessor,
    ...(input.candidate === undefined ? {} : { candidate: input.candidate }),
    trustedAction,
  })
  if (deleted.kind === 'gated') return cleanupGate(input, deleted.decision)
  await input.lifecycle.deleteOwnedTarget()
  emit(input, 'cleanup-verified', deleted.value.disposition)
  return Object.freeze({ kind: 'deleted' })
}

/** Reopen never recreates or substitutes: it proves the exact persisted binding. */
export async function reopenDirectZipSession<ParentHandle, FileHandle, Runtime>(
  input: DirectZipRecoverySessionInput<ParentHandle, FileHandle, Runtime>,
): Promise<DirectZipRecoverySessionResult<ParentHandle, FileHandle, Runtime>> {
  return observeAndOpen(input, false)
}

async function observeAndOpen<ParentHandle, FileHandle, Runtime>(
  input: DirectZipRecoverySessionInput<ParentHandle, FileHandle, Runtime>,
  verifyChangedEvidence: boolean,
): Promise<DirectZipRecoverySessionResult<ParentHandle, FileHandle, Runtime>> {
  requireRecoveryBinding(input)
  const observed = await input.target.reopen({
    binding: input.binding,
    currentParent: input.currentParent,
    predecessor: input.predecessor,
    ...(input.candidate === undefined ? {} : { candidate: input.candidate }),
    trustedAction: input.trustedAction,
    verifyChangedEvidence,
  })
  if (observed.kind === 'gated') {
    return gate(
      input,
      observed.decision,
      observed.decision.kind === 'target-verification-required' && !verifyChangedEvidence
        ? () => observeAndOpen(input, true)
        : undefined,
    )
  }
  emit(input, 'parent-reauthorized', 'granted')
  emit(
    input,
    verifyChangedEvidence ? 'target-slow-verified' : 'target-observed',
    observed.value.resolution.kind,
  )
  const space = await preflightDirectZipDestinationSpace({
    parent: input.currentParent,
    committedArchiveLength: input.predecessor.committedLength,
    projectedArchiveLength: input.projectedArchiveLength,
    additionalTemporaryBytesUpperBound: input.additionalTemporaryBytesUpperBound,
    space: input.space,
  })
  if (space.kind === 'gated') return gate(input, space.decision)
  emit(input, 'space-preflight', 'admitted')
  const activated = await input.lifecycle.activate(observed.value)
  emit(
    input,
    'execution-opened',
    resolutionOutcome(observed.value),
    activated.lifecycle.generation,
  )
  return Object.freeze({
    kind: 'active',
    session: new DirectZipActiveSession(input, activated.runtime),
  })
}

async function gate<ParentHandle, FileHandle, Runtime>(
  input: DirectZipRecoverySessionInput<ParentHandle, FileHandle, Runtime>,
  decision: DirectZipLifecycleDecision,
  verify?: () => Promise<DirectZipRecoverySessionResult<ParentHandle, FileHandle, Runtime>>,
): Promise<DirectZipRecoverySessionResult<ParentHandle, FileHandle, Runtime>> {
  const lifecycle = await input.lifecycle.gate(decision, decision.kind === 'destination-space-required'
    ? Object.freeze({
        additionalTemporaryBytesUpperBound: input.additionalTemporaryBytesUpperBound,
      })
    : Object.freeze({}))
  emit(input, gateMilestone(decision), 'gated', lifecycle.generation, decision)
  return Object.freeze({
    kind: 'gated',
    lifecycle,
    decision,
    ...(verify === undefined ? {} : { verify }),
  })
}

async function cleanupGate<ParentHandle, FileHandle, Runtime>(
  input: DirectZipRetainedCleanupInput<ParentHandle, FileHandle, Runtime>,
  decision: DirectZipLifecycleDecision,
): Promise<DirectZipRecoverySessionResult<ParentHandle, FileHandle, Runtime>> {
  const lifecycle = await input.lifecycle.gate(decision, Object.freeze({}))
  emit(input, gateMilestone(decision), 'gated', lifecycle.generation, decision)
  return Object.freeze({ kind: 'gated', lifecycle, decision })
}

function gateMilestone(
  decision: DirectZipLifecycleDecision,
): Parameters<NonNullable<DirectZipSessionTrace>>[0]['milestone'] {
  if (decision.kind === 'authorization-required') return 'parent-reauthorized'
  if (decision.kind === 'destination-space-required') return 'space-preflight'
  if (decision.kind === 'needs-attention' && decision.reason === 'cleanup-refused') {
    return 'cleanup-verified'
  }
  return 'target-observed'
}

function requireRecoveryBinding(input: DirectZipRecoverySessionInput<unknown, unknown, unknown>): void {
  requireOperationBinding(input)
  if (input.projectedArchiveLength < input.predecessor.committedLength) {
    throw new TypeError('Direct ZIP recovery input does not bind one frozen operation')
  }
}

function requireOperationBinding(
  input: DirectZipRetainedCleanupInput<unknown, unknown, unknown>,
): void {
  if (input.lifecycle.intent.plan.kind !== 'direct-resumable-zip' ||
      input.lifecycle.intent.operationId !== input.lifecycle.lifecycle.operationId ||
      input.lifecycle.intent.digest !== input.lifecycle.lifecycle.receiveIntentDigest ||
      input.binding.stableName !== input.lifecycle.intent.plan.binding.stableName) {
    throw new TypeError('Direct ZIP recovery input does not bind one frozen operation')
  }
}

function resolutionOutcome(value: DirectZipReopenResult<unknown>): string {
  return value.resolution.kind
}

function emit(
  input: Pick<DirectZipRecoverySessionInput<unknown, unknown, unknown>, 'lifecycle' | 'trace'>,
  milestone: Parameters<NonNullable<DirectZipSessionTrace>>[0]['milestone'],
  outcome: string,
  generation?: bigint,
  decision?: DirectZipLifecycleDecision,
): void {
  input.trace?.(Object.freeze({
    name: 'direct_zip.session.milestone',
    operation_id: input.lifecycle.intent.operationId,
    milestone,
    outcome,
    ...(generation === undefined ? {} : { lifecycle_generation: generation }),
    ...(decision === undefined ? {} : { lifecycle_decision: decision.kind }),
  }))
}
