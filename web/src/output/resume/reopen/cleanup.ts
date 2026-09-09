import { forgetReceiveOperationHistory } from '../operation-history'
import type { ReceiveOperationHandleInventoryRepository } from '../../workspace/repository'
import { forgetLegacyCompatibleNameRecord } from '../../browser/indexeddb/compatible-name-legacy-cleanup'
import { IndexedDbReceiveOperationRepository } from '../../browser/indexeddb-repository'
import {
  recordOutputException,
  type OutputFailureSinks,
  type OutputTraceSource,
} from '../../diagnostics'
import {
  discardReopenedFileSystemAccessOutput,
  type FreshPageFileSystemAccessDiscardResult,
} from '../../file-system-access/fresh-page-discard'
import { cleanupReopenedPublishedFileSystemAccessOutput } from '../../file-system-access/published-cleanup'
import {
  openOriginPrivateRetainedArtifactBackend,
  type OriginPrivateRetainedArtifactBackend,
} from '../../origin-private/session'
import type { ReceiveLifecycleState } from '../../workspace/state'
import type { PersistentPausedFileRecovery } from '../../persistent-tree/contracts'
import type {
  ReceiveOperationDiscardResult,
  ReceiveOperationMutationPort,
  ReceiveOperationResumeRequest,
} from '../authority'
import type { ReceiveOperationResumeDescriptor } from '../descriptor'
import { PersistedReceiveOperationReopenAuthority } from './authority'
import type {
  PersistedReceiveOperationReopenAuthorityOptions,
  ReopenedDirectTreeOperation,
  ReopenedDirectZipOperation,
  ReopenedReceiveOperation,
  ReopenedWorkspaceOperation,
} from './model'

export interface ReceiveOperationOwnedCleanupExecutor {
  cleanup(
    operation: ReopenedReceiveOperation,
    failures?: OutputFailureSinks,
  ): Promise<ReceiveOperationDiscardResult>
}

export interface PersistedReceiveOperationReopenPort {
  reopen(
    descriptor: ReceiveOperationResumeDescriptor,
    purpose: 'continue' | 'cleanup' | 'partial-export',
    failures?: OutputFailureSinks,
    retainedFileRecovery?: PersistentPausedFileRecovery,
  ): Promise<ReopenedReceiveOperation>
}

export interface PersistedReceiveOperationCleanupExecutorOptions {
  readonly checkpointDatabaseName?: string
  readonly discardDirectTree?: typeof discardReopenedFileSystemAccessOutput
  readonly cleanupPublishedDirectTree?: typeof cleanupReopenedPublishedFileSystemAccessOutput
  readonly outputTrace?: OutputTraceSource
  readonly openWorkspaceBackend?: typeof openOriginPrivateRetainedArtifactBackend
}

/** Each plan owner derives its own physical inventory before this seam projects the durable result. */
export class PersistedReceiveOperationCleanupExecutor
implements ReceiveOperationOwnedCleanupExecutor {
  readonly #checkpointDatabaseName: string | undefined
  readonly #discardDirectTree: typeof discardReopenedFileSystemAccessOutput
  readonly #cleanupPublishedDirectTree: typeof cleanupReopenedPublishedFileSystemAccessOutput
  readonly #outputTrace: OutputTraceSource | undefined
  readonly #openWorkspaceBackend: typeof openOriginPrivateRetainedArtifactBackend

  constructor(options: PersistedReceiveOperationCleanupExecutorOptions = {}) {
    this.#checkpointDatabaseName = options.checkpointDatabaseName
    this.#discardDirectTree = options.discardDirectTree ?? discardReopenedFileSystemAccessOutput
    this.#cleanupPublishedDirectTree = options.cleanupPublishedDirectTree ?? cleanupReopenedPublishedFileSystemAccessOutput
    this.#outputTrace = options.outputTrace
    this.#openWorkspaceBackend = options.openWorkspaceBackend ?? openOriginPrivateRetainedArtifactBackend
  }

  async cleanup(
    operation: ReopenedReceiveOperation,
    failures?: OutputFailureSinks,
  ): Promise<ReceiveOperationDiscardResult> {
    if (operation.kind === 'direct-tree') return this.#cleanupDirectTree(operation, failures)
    if (operation.kind === 'direct-zip') {
      throw new DOMException(
        'Direct ZIP cleanup requires the owned-file target proof authority',
        'NotSupportedError',
      )
    }
    let backend: OriginPrivateRetainedArtifactBackend | undefined
    try {
      backend = await this.#openWorkspaceBackend({
        receiveIntent: operation.intent,
        operationRepository: operation.repository,
        namespace: operation.namespace,
        ...(this.#checkpointDatabaseName === undefined
          ? {}
          : { checkpointDatabaseName: this.#checkpointDatabaseName }),
        ...(failures === undefined
          ? {}
          : { diagnostics: { backend: 'origin_private', failures } as const }),
      })
      const request = await backend.cleanup.cleanupRequest()
      const result = operation.lifecycle.kind === 'published' && operation.lifecycle.cleanupState === 'cleanup-pending'
        ? await operation.stages.retryPublishedCleanup(request)
        : await operation.stages.discard(request)
      if (result.kind === 'retryable-failure') {
        throw new DOMException('Owned workspace cleanup must be retried', 'OperationError')
      }
      if (result.kind === 'needs-attention') {
        return Object.freeze({ kind: 'needs-attention', reason: 'cleanup-unknown' })
      }
      if (result.state.kind === 'discarded') {
        return Object.freeze({
          kind: 'discarded',
          cleanupReceiptDigest: result.receipt.digest,
        })
      }
      if (result.state.kind === 'published') {
        return Object.freeze({
          kind: 'published-cleanup-completed',
          cleanupReceiptDigest: result.receipt.digest,
        })
      }
      throw new TypeError('workspace cleanup returned a non-terminal lifecycle')
    } finally {
      await backend?.close()
    }
  }

  async #cleanupDirectTree(
    operation: ReopenedDirectTreeOperation,
    failures?: OutputFailureSinks,
  ): Promise<ReceiveOperationDiscardResult> {
    const database = this.#checkpointDatabaseName === undefined
      ? {} : { databaseName: this.#checkpointDatabaseName }
    try {
      if (operation.lifecycle.kind === 'published') {
        const result = await this.#cleanupPublishedDirectTree({
          intent: operation.intent,
          lifecycle: operation.lifecycle,
          repository: operation.repository,
          leaseId: operation.lease.leaseId,
          ...database,
          ...(this.#outputTrace === undefined ? {} : { trace: this.#outputTrace }),
        })
        return Object.freeze({
          kind: 'published-cleanup-completed',
          cleanupReceiptDigest: result.receiptDigest,
        })
      }
      return projectDirectTreeDiscard(await this.#discardDirectTree({ operation, ...database }))
    } catch (error) {
      recordOutputException(failures?.cleanup, error, { recoveryDisposition: 'needs_attention' })
      throw error
    }
  }
}

export type AuthorityOwnedReceiveOperationMutationResult =
  | Readonly<{
      kind: 'continuation'
      continuation: AuthorityOwnedReceiveOperationContinuation
    }>
  | Readonly<{ kind: 'cleanup'; result: ReceiveOperationDiscardResult }>

export type AuthorityOwnedReceiveOperationContinuation =
  | Readonly<{ kind: 'direct-tree-receive'; operation: ReopenedDirectTreeOperation }>
  | Readonly<{ kind: 'direct-tree-catch-up'; operation: ReopenedDirectTreeOperation }>
  | Readonly<{
      kind: 'workspace-receive'
      operation: ReopenedWorkspaceOperation & {
        readonly lifecycle: Extract<ReceiveLifecycleState, { kind: 'receiving' }>
        readonly admittedContent: import('../../workspace/stages').AdmittedWorkspaceContent
        readonly receiveContinuation: import('./model').ReopenedWorkspaceReceiveContinuation
      }
    }>
  | Readonly<{
      kind: 'workspace-package'
      operation: ReopenedWorkspaceOperation & {
        readonly lifecycle: Extract<ReceiveLifecycleState, { kind: 'resumable-package' | 'materialization-sealed' }>
        readonly packageContinuation: import('../workspace-continuation').ReopenedWorkspacePackageContinuation
      }
    }>
  | Readonly<{
      kind: 'workspace-progressive-zip'
      operation: ReopenedWorkspaceOperation & {
        readonly progressiveContinuation: import('./model').ReopenedProgressiveZipContinuation
        readonly admittedContent: import('../../workspace/stages').AdmittedWorkspaceContent
      }
    }>
  | Readonly<{
      kind: 'workspace-progressive-zip-partial'
      operation: ReopenedWorkspaceOperation & {
        readonly partialContinuation: import('./partial-zip-continuation').RetainedZipPartialReader
      }
    }>
  | Readonly<{ kind: 'workspace-retained'; operation: ReopenedWorkspaceOperation }>
  | Readonly<{ kind: 'direct-zip'; operation: ReopenedDirectZipOperation }>
  | Readonly<{
      kind: 'direct-zip-retained-cleanup'
      operation: ReopenedDirectZipOperation
    }>

/**
 * Presentation can consume a descriptor but cannot provide an intent, binding, or
 * cleanup result. The output-owned executor is the only component allowed to turn
 * ownership evidence into a discard receipt.
 */
export class AuthorityOwnedReceiveOperationMutationPort
implements ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult> {
  readonly #reopen: PersistedReceiveOperationReopenPort
  readonly #cleanup: ReceiveOperationOwnedCleanupExecutor
  readonly #forgetHistory: ((descriptor: ReceiveOperationResumeDescriptor) => Promise<void>) | undefined
  readonly #forgetLegacy: ((descriptor: ReceiveOperationResumeDescriptor) => Promise<ReceiveOperationDiscardResult>) | undefined

  constructor(input: {
    readonly reopen: PersistedReceiveOperationReopenPort
    readonly cleanup: ReceiveOperationOwnedCleanupExecutor
    readonly forgetHistory?: (descriptor: ReceiveOperationResumeDescriptor) => Promise<void>
    readonly forgetLegacy?: (descriptor: ReceiveOperationResumeDescriptor) => Promise<ReceiveOperationDiscardResult>
  }) {
    this.#reopen = input.reopen
    this.#cleanup = input.cleanup
    this.#forgetLegacy = input.forgetLegacy
    this.#forgetHistory = input.forgetHistory
  }

  async resume(
    descriptor: ReceiveOperationResumeDescriptor,
    request?: ReceiveOperationResumeRequest,
  ): Promise<AuthorityOwnedReceiveOperationMutationResult> {
    assertPhysicalOutputAuthority(descriptor)
    const operation = await this.#reopen.reopen(
      descriptor,
      request?.purpose ?? 'continue',
      request?.failures,
      request?.retainedFileRecovery,
    )
    return Object.freeze({
      kind: 'continuation',
      continuation: classifyReopenedContinuation(operation),
    })
  }

  async cleanup(
    descriptor: ReceiveOperationResumeDescriptor,
    failures?: OutputFailureSinks,
  ): Promise<AuthorityOwnedReceiveOperationMutationResult> {
    assertPhysicalOutputAuthority(descriptor)
    const operation = await this.#reopen.reopen(descriptor, 'cleanup', failures)
    if (operation.kind === 'direct-zip') return directZipRetainedCleanup(operation)
    try {
      return Object.freeze({
        kind: 'cleanup',
        result: await this.#cleanup.cleanup(operation, failures),
      })
    } finally {
      await operation.close()
    }
  }

  async forget(descriptor: ReceiveOperationResumeDescriptor): Promise<void> {
    if (this.#forgetHistory === undefined) throw new DOMException('Download history removal is unavailable', 'NotSupportedError')
    await this.#forgetHistory(descriptor)
  }

  async discard(
    descriptor: ReceiveOperationResumeDescriptor,
    failures?: OutputFailureSinks,
  ): Promise<ReceiveOperationDiscardResult> {
    if (descriptor.continuation === 'cleanup-incompatible') {
      if (this.#forgetLegacy === undefined) {
        throw new DOMException('Saved-record cleanup is unavailable', 'NotSupportedError')
      }
      return this.#forgetLegacy(descriptor)
    }
    const operation = await this.#reopen.reopen(descriptor, 'cleanup', failures)
    try {
      return await this.#cleanup.cleanup(operation, failures)
    } finally {
      await operation.close()
    }
  }

  async catchUp(
    descriptor: ReceiveOperationResumeDescriptor,
    failures?: OutputFailureSinks,
  ): Promise<AuthorityOwnedReceiveOperationMutationResult> {
    assertPhysicalOutputAuthority(descriptor)
    const operation = await this.#reopen.reopen(descriptor, 'cleanup', failures)
    if (operation.kind === 'direct-zip') return directZipRetainedCleanup(operation)
    if (operation.kind !== 'direct-tree') {
      return withClosedOperation(operation, async () => {
        throw new TypeError('terminal catch-up is exclusive to DirectTree operations')
      })
    }
    return Object.freeze({
      kind: 'continuation',
      continuation: Object.freeze({ kind: 'direct-tree-catch-up', operation }),
    })
  }
}

function assertPhysicalOutputAuthority(descriptor: ReceiveOperationResumeDescriptor): void {
  if (descriptor.continuation === 'cleanup-incompatible') {
    throw new DOMException('Incompatible saved records have no physical output authority', 'InvalidStateError')
  }
}

function directZipRetainedCleanup(
  operation: ReopenedDirectZipOperation,
): AuthorityOwnedReceiveOperationMutationResult {
  // Generic cleanup intentionally has no target-proof port. Ownership transfers
  // to the injected Direct ZIP runtime, which closes this reopen authority.
  return Object.freeze({
    kind: 'continuation',
    continuation: Object.freeze({ kind: 'direct-zip-retained-cleanup', operation }),
  })
}

async function withClosedOperation<Result>(
  operation: ReopenedReceiveOperation,
  action: () => Promise<Result>,
): Promise<Result> {
  try {
    return await action()
  } finally {
    await operation.close()
  }
}

export function createPersistedReceiveOperationMutationPort(
  options: PersistedReceiveOperationReopenAuthorityOptions &
  PersistedReceiveOperationCleanupExecutorOptions,
): AuthorityOwnedReceiveOperationMutationPort {
  return new AuthorityOwnedReceiveOperationMutationPort({
    reopen: new PersistedReceiveOperationReopenAuthority(options),
    cleanup: new PersistedReceiveOperationCleanupExecutor(options),
    forgetHistory: async descriptor => {
      const repository = await options.repositoryFactory()
      try {
        if (!('listHandles' in repository) || typeof repository.listHandles !== 'function') {
          throw new DOMException('Download history inventory is unavailable', 'NotSupportedError')
        }
        await forgetReceiveOperationHistory(descriptor,
          repository as ReceiveOperationHandleInventoryRepository, options.leaseOptions)
      } finally {
        repository.close()
      }
    },
    forgetLegacy: descriptor => forgetLegacyCompatibleNameRecord(descriptor, {
      ...(options.checkpointDatabaseName === undefined ? {} : { databaseName: options.checkpointDatabaseName }),
    }),
  })
}

export type BrowserReceiveOperationMutationPortOptions =
  Omit<PersistedReceiveOperationReopenAuthorityOptions, 'repositoryFactory'> &
  PersistedReceiveOperationCleanupExecutorOptions

/** Production injection seam: inventory/UI supplies only a single-use descriptor. */
export function createBrowserReceiveOperationMutationPort(
  options: BrowserReceiveOperationMutationPortOptions = {},
): AuthorityOwnedReceiveOperationMutationPort {
  return createPersistedReceiveOperationMutationPort({
    ...options,
    repositoryFactory: () => IndexedDbReceiveOperationRepository.open(
      options.checkpointDatabaseName,
    ),
  })
}

function projectDirectTreeDiscard(
  result: FreshPageFileSystemAccessDiscardResult,
): ReceiveOperationDiscardResult {
  if (result.lifecycle.kind === 'needs-attention') {
    if (result.lifecycle.reason === 'publication-unknown') {
      throw new TypeError('DirectTree discard cannot produce publication uncertainty')
    }
    return Object.freeze({ kind: 'needs-attention', reason: result.lifecycle.reason })
  }
  if (!('receiptDigest' in result)) {
    throw new TypeError('DirectTree discard omitted its durable receipt')
  }
  if (result.lifecycle.kind === 'partial-directory') {
    return Object.freeze({ kind: 'partial-directory', receiptDigest: result.receiptDigest })
  }
  if (result.lifecycle.kind === 'discarded') {
    return Object.freeze({ kind: 'discarded', cleanupReceiptDigest: result.receiptDigest })
  }
  throw new TypeError('DirectTree discard returned a non-terminal lifecycle')
}

function classifyReopenedContinuation(
  operation: ReopenedReceiveOperation,
): AuthorityOwnedReceiveOperationContinuation {
  if (operation.kind === 'direct-tree') {
    return Object.freeze({ kind: 'direct-tree-receive', operation })
  }
  if (operation.kind === 'direct-zip') {
    return Object.freeze({ kind: 'direct-zip', operation })
  }
  if (operation.partialContinuation !== undefined) {
    return { kind: 'workspace-progressive-zip-partial', operation: operation as Extract<
      AuthorityOwnedReceiveOperationContinuation, { kind: 'workspace-progressive-zip-partial' }
    >['operation'] }
  }
  if (operation.progressiveContinuation !== undefined && operation.admittedContent !== undefined) {
    return Object.freeze({ kind: 'workspace-progressive-zip', operation: operation as Extract<
      AuthorityOwnedReceiveOperationContinuation, { kind: 'workspace-progressive-zip' }
    >['operation'] })
  }
  if (operation.lifecycle.kind === 'receiving' && operation.admittedContent !== undefined &&
      operation.receiveContinuation !== undefined) {
    return Object.freeze({
      kind: 'workspace-receive',
      operation: operation as Extract<AuthorityOwnedReceiveOperationContinuation, {
        kind: 'workspace-receive'
      }>['operation'],
    })
  }
  if ((operation.lifecycle.kind === 'resumable-package' || operation.lifecycle.kind === 'materialization-sealed') &&
      operation.packageContinuation !== undefined) {
    return Object.freeze({
      kind: 'workspace-package',
      operation: operation as Extract<AuthorityOwnedReceiveOperationContinuation, {
        kind: 'workspace-package'
      }>['operation'],
    })
  }
  return Object.freeze({ kind: 'workspace-retained', operation })
}
