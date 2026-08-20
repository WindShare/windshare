import {
  acquireBrowserReceiveOperationLease,
  type BrowserReceiveOperationLease,
} from '../../output/browser/session-lease'
import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../../output/diagnostics'
import {
  inspectOriginPrivateWorkspaceNamespace,
  originPrivateWorkspaceRootHandleId,
  OriginPrivateWorkspaceNamespaceOpenError,
  removeOriginPrivateWorkspaceNamespace,
  removeUncommittedOriginPrivateWorkspaceNamespace,
  type OriginPrivateWorkspaceNamespace,
} from '../../output/origin-private/namespace'
import {
  discardedWorkspaceActivationSettlement,
  observeWorkspaceActivationPersistence,
  recoverWorkspaceActivationCandidate,
  withWorkspaceActivationLock,
  WorkspaceActivationRecovery,
} from '../../output/workspace/activation-recovery'
import type { WorkspaceActivationCandidateV1 } from '../../output/workspace/records'
import type {
  ReceiveOperationRepository,
  WorkspaceActivationJournalRepository,
} from '../../output/workspace/repository'
import { initialReceiveLifecycleState, type ReceiveLifecycleState } from '../../output/workspace/state'
import type { WorkspaceStageTraceListener } from '../../output/workspace/stages'
import type { ReceiveIntent } from '../../transfer/intent'
import type { V2LifecycleMutation, V2OwnedActivationAuthority } from '../v2-receive-runtime'
import type { BrowserReceiveWindow } from './contracts'

export interface WorkspaceActivationOwnerDependencies {
  readonly inspectNamespace: typeof inspectOriginPrivateWorkspaceNamespace
  readonly removeNamespace: typeof removeOriginPrivateWorkspaceNamespace
  readonly removeUncommittedNamespace: typeof removeUncommittedOriginPrivateWorkspaceNamespace
  readonly observePersistence: typeof observeWorkspaceActivationPersistence
  readonly recoverCandidate: typeof recoverWorkspaceActivationCandidate
  readonly withActivationLock: <Result>(
    operationId: string,
    execute: () => Promise<Result>,
  ) => Promise<Result>
  readonly acquireLease: (
    repository: ReceiveOperationRepository,
    operationId: string,
  ) => Promise<BrowserReceiveOperationLease>
  readonly openRecovery: typeof WorkspaceActivationRecovery.open
}

export interface WorkspaceActivationOwnerContext {
  readonly intent: ReceiveIntent
  readonly repository: WorkspaceActivationJournalRepository
  readonly storage: BrowserReceiveWindow['navigator']['storage']
  readonly dependencies: WorkspaceActivationOwnerDependencies
  readonly trace?: WorkspaceStageTraceListener
  readonly diagnostics?: OutputDiagnosticsPorts
}

interface RecoverWorkspaceActivationOwnerInput extends WorkspaceActivationOwnerContext {
  readonly namespace: OriginPrivateWorkspaceNamespace | undefined
  readonly owner: WorkspaceOwnedActivationAuthority | undefined
  readonly error: unknown
}

export class WorkspaceOwnedActivationAuthority implements V2OwnedActivationAuthority {
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState
  readonly #repository: WorkspaceActivationJournalRepository
  readonly #storage: BrowserReceiveWindow['navigator']['storage']
  readonly #dependencies: WorkspaceActivationOwnerDependencies
  readonly #trace: WorkspaceStageTraceListener | undefined
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #candidate: WorkspaceActivationCandidateV1 | undefined
  #namespace: OriginPrivateWorkspaceNamespace | undefined
  #persistenceConfirmed: boolean | undefined
  #lease: BrowserReceiveOperationLease | undefined
  #recovery: WorkspaceActivationRecovery | undefined
  #settlement: Promise<V2LifecycleMutation> | undefined
  #safeToDetach: boolean
  #detached = false
  #leaseReleased = false
  #repositoryClosed = false
  #transferred = false

  private constructor(input: WorkspaceActivationOwnerContext & {
    readonly namespace: OriginPrivateWorkspaceNamespace | undefined
    readonly persistenceConfirmed: boolean | undefined
    readonly candidate: WorkspaceActivationCandidateV1 | undefined
  }) {
    this.intent = input.intent
    this.lifecycle = initialReceiveLifecycleState({
      operationId: input.intent.operationId,
      receiveIntentDigest: input.intent.digest,
    })
    this.#repository = input.repository
    this.#storage = input.storage
    this.#dependencies = input.dependencies
    this.#trace = input.trace
    this.#diagnostics = input.diagnostics
    this.#namespace = input.namespace
    this.#persistenceConfirmed = input.persistenceConfirmed
    this.#candidate = input.candidate
    this.#safeToDetach = input.persistenceConfirmed === true
  }

  static fromCandidate(
    context: WorkspaceActivationOwnerContext,
    candidate: WorkspaceActivationCandidateV1,
  ): WorkspaceOwnedActivationAuthority {
    return new WorkspaceOwnedActivationAuthority({
      ...context,
      namespace: undefined,
      persistenceConfirmed: false,
      candidate,
    })
  }

  static fromPersistedNamespace(
    context: WorkspaceActivationOwnerContext,
    namespace: OriginPrivateWorkspaceNamespace,
  ): WorkspaceOwnedActivationAuthority {
    return new WorkspaceOwnedActivationAuthority({
      ...context,
      namespace,
      persistenceConfirmed: true,
      candidate: undefined,
    })
  }

  static async requirePersistedNamespace(
    context: WorkspaceActivationOwnerContext,
    namespace: OriginPrivateWorkspaceNamespace,
  ): Promise<WorkspaceOwnedActivationAuthority> {
    const observation = await context.dependencies.observePersistence({
      repository: context.repository,
      operationId: context.intent.operationId,
      rootHandleId: namespace.rootHandleId,
    })
    if (observation.kind !== 'owned-effects') {
      throw new TypeError('workspace namespace opened without durable ownership evidence')
    }
    return WorkspaceOwnedActivationAuthority.fromPersistedNamespace(context, namespace)
  }

  static async recover(
    input: RecoverWorkspaceActivationOwnerInput,
  ): Promise<WorkspaceOwnedActivationAuthority | undefined> {
    let namespace = input.namespace
    if (input.error instanceof OriginPrivateWorkspaceNamespaceOpenError &&
        input.error.namespace !== undefined) {
      namespace = input.error.namespace
      input.owner?.adoptNamespace(namespace, false)
    }
    if (input.owner !== undefined) return input.owner
    if (input.error instanceof OriginPrivateWorkspaceNamespaceOpenError) {
      return new WorkspaceOwnedActivationAuthority({
        ...input,
        namespace: input.error.namespace,
        persistenceConfirmed: false,
        candidate: input.error.candidate,
      })
    }

    let observation: Awaited<ReturnType<WorkspaceActivationOwnerDependencies['observePersistence']>>
    const rootHandleId = namespace?.rootHandleId ??
      originPrivateWorkspaceRootHandleId(input.intent.operationId)
    try {
      observation = await input.dependencies.observePersistence({
        repository: input.repository,
        operationId: input.intent.operationId,
        rootHandleId,
      })
    } catch {
      return new WorkspaceOwnedActivationAuthority({
        ...input,
        namespace,
        persistenceConfirmed: undefined,
        candidate: undefined,
      })
    }
    if (observation.kind === 'owned-effects') {
      namespace ??= await input.dependencies.inspectNamespace({
        receiveIntent: input.intent,
        repository: input.repository,
        storage: input.storage,
      }).catch(() => undefined)
      return new WorkspaceOwnedActivationAuthority({
        ...input,
        namespace,
        persistenceConfirmed: true,
        candidate: undefined,
      })
    }
    if (namespace === undefined) return undefined
    if (await input.dependencies.removeUncommittedNamespace(namespace).catch(() => false)) {
      const afterCleanup = await input.dependencies.observePersistence({
        repository: input.repository,
        operationId: input.intent.operationId,
        rootHandleId: namespace.rootHandleId,
      }).catch(() => Object.freeze({ kind: 'owned-effects' as const }))
      if (afterCleanup.kind === 'absent') return undefined
    }
    return new WorkspaceOwnedActivationAuthority({
      ...input,
      namespace,
      persistenceConfirmed: false,
      candidate: undefined,
    })
  }

  acquireLease(): Promise<BrowserReceiveOperationLease> {
    return this.#ensureLease()
  }

  adoptNamespace(namespace: OriginPrivateWorkspaceNamespace, promoted: boolean): void {
    if (namespace.operationId !== this.intent.operationId ||
        (this.#namespace !== undefined && this.#namespace.root !== namespace.root)) {
      throw new TypeError('workspace activation namespace does not belong to its operation')
    }
    this.#namespace = namespace
    if (promoted) {
      this.#persistenceConfirmed = true
      this.#safeToDetach = true
    }
  }

  transferToBoundOperation(): void {
    if (this.#transferred || this.#detached || this.#lease === undefined ||
        !this.#persistenceConfirmed || this.#namespace === undefined) {
      throw new DOMException('Workspace activation ownership cannot be transferred', 'InvalidStateError')
    }
    this.#transferred = true
  }

  settleActivationFailure(reason: unknown): Promise<V2LifecycleMutation> {
    if (this.#settlement !== undefined) return this.#settlement
    if (this.#transferred || this.#detached) {
      return Promise.reject(new DOMException(
        'Workspace activation authority no longer owns settlement',
        'InvalidStateError',
      ))
    }
    const attempt = this.#settle(reason)
    const retained = attempt.catch((error: unknown) => {
      if (this.#settlement === retained) this.#settlement = undefined
      throw error
    })
    this.#settlement = retained
    return retained
  }

  async detach(): Promise<void> {
    if (this.#transferred || this.#detached) return
    if (!this.#safeToDetach) {
      throw new DOMException(
        'Unpersisted workspace effects must become durable or absent before authority detaches',
        'InvalidStateError',
      )
    }
    const failures: unknown[] = []
    if (!this.#leaseReleased) {
      try {
        await this.#lease?.release()
        this.#leaseReleased = true
      } catch (error) {
        failures.push(error)
        recordOutputException(this.#diagnostics?.failures?.cleanup, error)
      }
    }
    if (!this.#repositoryClosed) {
      try {
        this.#repository.close()
        this.#repositoryClosed = true
      } catch (error) {
        failures.push(error)
        recordOutputException(this.#diagnostics?.failures?.cleanup, error)
      }
    }
    if (failures.length !== 0) {
      emitOutputTrace(this.#diagnostics?.trace, () =>
        outputTraceEvent('cleanup', { backend: 'origin_private', transition: 'failed' }))
      throw new AggregateError(failures, 'Workspace activation resources did not detach')
    }
    this.#detached = true
  }

  async #settle(reason: unknown): Promise<V2LifecycleMutation> {
    const absenceSettlement = await this.#dependencies.withActivationLock(
      this.intent.operationId,
      () => this.#ensurePersistenceOrAbsence(reason),
    )
    if (absenceSettlement !== undefined) return absenceSettlement
    const lease = await this.#ensureLease()
    this.#recovery ??= await this.#dependencies.openRecovery({
      repository: this.#repository,
      receiveIntent: this.intent,
      leaseId: lease.leaseId,
      ...(this.#namespace === undefined
        ? {}
        : {
            rootHandleId: this.#namespace.rootHandleId,
            removeNamespace: () => this.#dependencies.removeNamespace(
              this.#namespace!,
              this.#repository,
            ),
          }),
      clock: Date.now,
      ...(this.#trace === undefined ? {} : { onTrace: this.#trace }),
      ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
    })
    return this.#recovery.settle(reason)
  }

  async #ensurePersistenceOrAbsence(reason: unknown): Promise<V2LifecycleMutation | undefined> {
    if (this.#persistenceConfirmed) return undefined
    if (this.#candidate !== undefined) return this.#settleCandidateActivation()
    return this.#settleConservativeActivation(reason)
  }

  async #settleCandidateActivation(): Promise<V2LifecycleMutation | undefined> {
    if (this.#candidate === undefined) throw new TypeError('workspace activation candidate is absent')
    if (this.#namespace !== undefined) {
      await this.#dependencies.removeUncommittedNamespace(this.#namespace).catch(() => false)
    }
    const recovery = await this.#dependencies.recoverCandidate({
      repository: this.#repository,
      candidate: this.#candidate,
      storage: this.#storage as StorageManager & {
        getDirectory(): Promise<FileSystemDirectoryHandle>
      },
    })
    if (recovery.kind === 'absent') {
      this.#safeToDetach = true
      return discardedWorkspaceActivationSettlement(
        this.intent,
        this.#namespace === undefined ? [] : [this.#namespace.rootOwnedObjectId],
      )
    }
    if (recovery.kind === 'needs-attention') {
      this.#safeToDetach = true
      return Object.freeze({ lifecycle: recovery.lifecycle, workspaceUsage: null })
    }
    this.adoptNamespace(recovery.namespace, true)
    return undefined
  }

  async #settleConservativeActivation(reason: unknown): Promise<V2LifecycleMutation | undefined> {
    if (this.#persistenceConfirmed === undefined) {
      const observation = await this.#dependencies.observePersistence({
        repository: this.#repository,
        operationId: this.intent.operationId,
        rootHandleId: this.#namespace?.rootHandleId ??
          originPrivateWorkspaceRootHandleId(this.intent.operationId),
      })
      this.#persistenceConfirmed = observation.kind === 'owned-effects'
      if (this.#persistenceConfirmed) this.#safeToDetach = true
    }
    if (this.#persistenceConfirmed) return undefined
    const namespace = this.#namespace
    if (namespace === undefined) {
      this.#safeToDetach = true
      return discardedWorkspaceActivationSettlement(this.intent, [])
    }
    if (await this.#dependencies.removeUncommittedNamespace(namespace)) {
      const observation = await this.#dependencies.observePersistence({
        repository: this.#repository,
        operationId: this.intent.operationId,
        rootHandleId: namespace.rootHandleId,
      })
      if (observation.kind === 'absent') {
        this.#safeToDetach = true
        return discardedWorkspaceActivationSettlement(
          this.intent,
          [namespace.rootOwnedObjectId],
        )
      }
    }
    throw new AggregateError([reason], 'Workspace activation absence could not be proven')
  }

  async #ensureLease(): Promise<BrowserReceiveOperationLease> {
    if (this.#lease === undefined) {
      const acquired = await this.#dependencies.acquireLease(
        this.#repository,
        this.intent.operationId,
      )
      if (acquired.operationId !== this.intent.operationId) {
        throw new TypeError('workspace activation lease does not belong to its operation')
      }
      this.#lease = acquired
    }
    return this.#lease
  }
}

export function workspaceActivationOwnerDependencies(
  windowPort: BrowserReceiveWindow,
  overrides: Partial<WorkspaceActivationOwnerDependencies> | undefined,
): WorkspaceActivationOwnerDependencies {
  return Object.freeze({
    inspectNamespace: inspectOriginPrivateWorkspaceNamespace,
    removeNamespace: removeOriginPrivateWorkspaceNamespace,
    removeUncommittedNamespace: removeUncommittedOriginPrivateWorkspaceNamespace,
    observePersistence: observeWorkspaceActivationPersistence,
    recoverCandidate: recoverWorkspaceActivationCandidate,
    withActivationLock: <Result>(operationId: string, execute: () => Promise<Result>) =>
      withWorkspaceActivationLock<Result>(operationId, execute, windowPort.navigator.locks),
    acquireLease: (repository: ReceiveOperationRepository, operationId: string) =>
      acquireBrowserReceiveOperationLease(
        repository,
        operationId,
        { manager: windowPort.navigator.locks },
      ),
    openRecovery: WorkspaceActivationRecovery.open,
    ...overrides,
  })
}
