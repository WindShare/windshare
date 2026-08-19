import { IndexedDbReceiveOperationRepository } from '../../browser/indexeddb-repository'
import {
  recordOutputException,
  type OutputFailureSinks,
} from '../../diagnostics'
import {
  discardReopenedFileSystemAccessOutput,
  type FreshPageFileSystemAccessDiscardResult,
} from '../../file-system-access/fresh-page-discard'
import {
  openOriginPrivateRetainedArtifactBackend,
  type OriginPrivateRetainedArtifactBackend,
} from '../../origin-private/session'
import type { ReceiveLifecycleState } from '../../workspace/state'
import type {
  ReceiveOperationDiscardResult,
  ReceiveOperationMutationPort,
} from '../authority'
import type { ReceiveOperationResumeDescriptor } from '../descriptor'
import { PersistedReceiveOperationReopenAuthority } from './authority'
import type {
  PersistedReceiveOperationReopenAuthorityOptions,
  ReopenedDirectTreeOperation,
  ReopenedReceiveOperation,
  ReopenedWorkspaceOperation,
} from './model'

export interface ReceiveOperationOwnedCleanupExecutor {
  cleanup(
    operation: ReopenedReceiveOperation,
    failures?: OutputFailureSinks,
  ): Promise<ReceiveOperationDiscardResult>
}

export interface PersistedReceiveOperationCleanupExecutorOptions {
  readonly checkpointDatabaseName?: string
  readonly discardDirectTree?: typeof discardReopenedFileSystemAccessOutput
  readonly openWorkspaceBackend?: typeof openOriginPrivateRetainedArtifactBackend
}

/** Each plan owner derives its own physical inventory before this seam projects the durable result. */
export class PersistedReceiveOperationCleanupExecutor
implements ReceiveOperationOwnedCleanupExecutor {
  readonly #checkpointDatabaseName: string | undefined
  readonly #discardDirectTree: typeof discardReopenedFileSystemAccessOutput
  readonly #openWorkspaceBackend: typeof openOriginPrivateRetainedArtifactBackend

  constructor(options: PersistedReceiveOperationCleanupExecutorOptions = {}) {
    this.#checkpointDatabaseName = options.checkpointDatabaseName
    this.#discardDirectTree = options.discardDirectTree ?? discardReopenedFileSystemAccessOutput
    this.#openWorkspaceBackend = options.openWorkspaceBackend ?? openOriginPrivateRetainedArtifactBackend
  }

  async cleanup(
    operation: ReopenedReceiveOperation,
    failures?: OutputFailureSinks,
  ): Promise<ReceiveOperationDiscardResult> {
    if (operation.kind === 'direct-tree') {
      try {
        return projectDirectTreeDiscard(await this.#discardDirectTree({
          operation,
          ...(this.#checkpointDatabaseName === undefined
            ? {}
            : { databaseName: this.#checkpointDatabaseName }),
        }))
      } catch (error) {
        recordOutputException(failures?.cleanup, error, {
          recoveryDisposition: 'needs_attention',
        })
        throw error
      }
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
      const result = operation.lifecycle.kind === 'expired' ||
          (operation.lifecycle.kind === 'published' && operation.lifecycle.cleanupState === 'cleanup-pending')
        ? await operation.stages.retryTerminalCleanup(request)
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
      if (result.state.kind === 'published' || result.state.kind === 'expired') {
        return Object.freeze({
          kind: 'cleanup-completed',
          terminalState: result.state.kind,
          cleanupReceiptDigest: result.receipt.digest,
        })
      }
      throw new TypeError('workspace cleanup returned a non-terminal lifecycle')
    } finally {
      await backend?.close()
    }
  }
}

export type AuthorityOwnedReceiveOperationMutationResult =
  | Readonly<{
      kind: 'continuation'
      continuation: AuthorityOwnedReceiveOperationContinuation
    }>
  | Readonly<{ kind: 'retention-cleanup'; result: ReceiveOperationDiscardResult }>

export type AuthorityOwnedReceiveOperationContinuation =
  | Readonly<{ kind: 'direct-tree-receive'; operation: ReopenedDirectTreeOperation }>
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
        readonly lifecycle: Extract<ReceiveLifecycleState, { kind: 'resumable-package' }>
        readonly packageContinuation: import('../workspace-continuation').ReopenedWorkspacePackageContinuation
      }
    }>
  | Readonly<{ kind: 'workspace-retained'; operation: ReopenedWorkspaceOperation }>

/**
 * Presentation can consume a descriptor but cannot provide an intent, binding, or
 * cleanup result. The output-owned executor is the only component allowed to turn
 * ownership evidence into a discard receipt.
 */
export class AuthorityOwnedReceiveOperationMutationPort
implements ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult> {
  readonly #reopen: PersistedReceiveOperationReopenAuthority
  readonly #cleanup: ReceiveOperationOwnedCleanupExecutor

  constructor(input: {
    readonly reopen: PersistedReceiveOperationReopenAuthority
    readonly cleanup: ReceiveOperationOwnedCleanupExecutor
  }) {
    this.#reopen = input.reopen
    this.#cleanup = input.cleanup
  }

  async resume(
    descriptor: ReceiveOperationResumeDescriptor,
    failures?: OutputFailureSinks,
  ): Promise<AuthorityOwnedReceiveOperationMutationResult> {
    const operation = await this.#reopen.reopen(descriptor, 'continue', failures)
    return Object.freeze({
      kind: 'continuation',
      continuation: classifyReopenedContinuation(operation),
    })
  }

  async expire(
    descriptor: ReceiveOperationResumeDescriptor,
    failures?: OutputFailureSinks,
  ): Promise<AuthorityOwnedReceiveOperationMutationResult> {
    const operation = await this.#reopen.reopen(descriptor, 'cleanup', failures)
    try {
      return Object.freeze({
        kind: 'retention-cleanup',
        result: await this.#cleanup.cleanup(operation, failures),
      })
    } finally {
      await operation.close()
    }
  }

  async discard(
    descriptor: ReceiveOperationResumeDescriptor,
    failures?: OutputFailureSinks,
  ): Promise<ReceiveOperationDiscardResult> {
    const operation = await this.#reopen.reopen(descriptor, 'cleanup', failures)
    try {
      return await this.#cleanup.cleanup(operation, failures)
    } finally {
      await operation.close()
    }
  }
}

export function createPersistedReceiveOperationMutationPort(
  options: PersistedReceiveOperationReopenAuthorityOptions &
  PersistedReceiveOperationCleanupExecutorOptions,
): AuthorityOwnedReceiveOperationMutationPort {
  return new AuthorityOwnedReceiveOperationMutationPort({
    reopen: new PersistedReceiveOperationReopenAuthority(options),
    cleanup: new PersistedReceiveOperationCleanupExecutor(options),
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
  return Object.freeze({
    kind: 'cleanup-completed',
    terminalState: 'expired',
    cleanupReceiptDigest: result.receiptDigest,
  })
}

function classifyReopenedContinuation(
  operation: ReopenedReceiveOperation,
): AuthorityOwnedReceiveOperationContinuation {
  if (operation.kind === 'direct-tree') {
    return Object.freeze({ kind: 'direct-tree-receive', operation })
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
  if (operation.lifecycle.kind === 'resumable-package' &&
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
