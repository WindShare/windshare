import { validateReceiveIntent, type ReceiveIntent } from '../../transfer/intent'
import { recordOutputException, type OutputDiagnosticsPorts } from '../diagnostics'
import { BrowserFileSystemTree } from '../browser/filesystem-tree'
import {
  verifyFSAOperationBinding,
  type FSAOperationBindingRepository,
  type PersistedFSAOperationBinding,
} from '../browser/indexeddb-root-binding'
import {
  acquireFSARootMutationLease,
  type BrowserLockManagerRuntime,
  type FSAHandleIdentityResolver,
  type FSARootMutationLease,
} from '../browser/namespace-mutation'
import { PersistentTreeOutputSession } from '../persistent-tree/session'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import {
  createPersistentOutputStageAuthority,
  type PersistentOutputStageDiagnostics,
} from '../persistent-tree/stage-diagnostics'
import {
  openFSAFileCheckpointRepository,
  type FSAFileCheckpointRepository,
  type FSAFileCheckpointRepositoryFactory,
  type FSASemanticOutputRepository,
} from './checkpoint-repository'
import { SafeNameRegistry } from './safe-name'
import {
  emitFSAOutputTrace,
  needsAttention,
  outputTrace,
} from './session-diagnostics'
import {
  FileSystemAccessOutputSession,
  type FSAOutputTrace,
} from './session'

export interface ReopenFileSystemAccessOutputOptions {
  readonly intent: ReceiveIntent
  readonly operationRepository: FSAOperationBindingRepository
  readonly maximumConcurrentInitialClaimInspections?: number
  readonly lockManager?: BrowserLockManagerRuntime
  readonly mutationIdentities?: FSAHandleIdentityResolver
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
  readonly databaseName?: string
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly stageDiagnostics?: PersistentOutputStageDiagnostics
  readonly trace?: FSAOutputTrace
}

export async function reopenFileSystemAccessOutput(
  options: ReopenFileSystemAccessOutputOptions,
): Promise<FileSystemAccessOutputSession> {
  const intent = await validateReceiveIntent(options.intent)
  const stageAuthority = createPersistentOutputStageAuthority(
    options.stageDiagnostics,
    {
      operationId: intent.operationId,
      artifactId: intent.artifact.digest,
    },
  )
  let firstBinding: PersistedFSAOperationBinding
  try {
    firstBinding = await verifyFSAOperationBinding({
      repository: options.operationRepository,
      intent,
      ...(stageAuthority === undefined
        ? {}
        : { stageScope: stageAuthority.bindingScope() }),
    })
  } catch (error) {
    if (error instanceof TargetOwnershipUnknownError && error.stage !== 'checkpoint') {
      emitFSAOutputTrace(options.trace, needsAttention(intent.operationId))
    }
    recordOutputException(options.diagnostics?.failures?.reopen, error)
    outputTrace(options.diagnostics, { eventName: 'reopen', transition: 'failed' })
    throw error
  }
  const rootLease = await acquireReopenRootLease(firstBinding.parent, options)
  let checkpoints: FSASemanticOutputRepository | undefined
  let materializationOpening = false
  try {
    const binding = await verifyFSAOperationBinding({
      repository: options.operationRepository,
      intent,
      expectedParent: firstBinding.parent,
      ...(stageAuthority === undefined
        ? {}
        : { stageScope: stageAuthority.bindingScope() }),
    })
    const safeNames = new SafeNameRegistry(binding.reservation)
    checkpoints = await openFSAFileCheckpointRepository(options, intent, binding.reservation)
    const tree = new BrowserFileSystemTree({
      binding,
      operationRepository: options.operationRepository,
      fileHandles: checkpoints,
      mutations: rootLease.authority,
      safeNames,
      ...(stageAuthority === undefined ? {} : { stageAuthority }),
    })
    materializationOpening = true
    const materialization = await PersistentTreeOutputSession.open({
      tree,
      checkpoints,
      semantic: checkpoints,
      ...initialClaimInspectionSessionOption(options.maximumConcurrentInitialClaimInspections),
      ...(options.diagnostics === undefined
        ? {}
        : { diagnostics: options.diagnostics }),
      ...(stageAuthority === undefined ? {} : { stageAuthority }),
      ...(options.trace === undefined ? {} : { trace: options.trace }),
    })
    emitFSAOutputTrace(options.trace, Object.freeze({
      name: 'receive.reservation.reopened',
      operation_id: intent.operationId,
      receive_intent_digest: intent.digest,
      reservation_kind: 'named-container-entry',
    }))
    outputTrace(options.diagnostics, { eventName: 'reopen', transition: 'authorized' })
    return new FileSystemAccessOutputSession({
      intent,
      reservation: binding.reservation,
      materialization,
      tree,
      binding,
      operationRepository: options.operationRepository,
      checkpoints,
      rootLease,
      safeNames,
      ...(stageAuthority === undefined ? {} : { stageAuthority }),
      ...(options.diagnostics === undefined
        ? {}
        : { diagnostics: options.diagnostics }),
    })
  } catch (error) {
    reportFSAReopenFailure(options, intent.operationId, error, materializationOpening)
    const cleanupFailures = await releaseFailedFSAOpen(
      checkpoints,
      rootLease,
      options.diagnostics,
    )
    if (cleanupFailures.length !== 0) {
      throw new AggregateError(
        [error, ...cleanupFailures],
        'FSA output reopen failed and could not release all authorities',
        { cause: error },
      )
    }
    throw error
  }
}

function initialClaimInspectionSessionOption(
  maximumConcurrentInitialClaimInspections: number | undefined,
): Readonly<{ maximumConcurrentInitialClaimInspections?: number }> {
  return maximumConcurrentInitialClaimInspections === undefined
    ? {}
    : { maximumConcurrentInitialClaimInspections }
}

async function acquireReopenRootLease(
  parent: FileSystemDirectoryHandle,
  options: ReopenFileSystemAccessOutputOptions,
): Promise<FSARootMutationLease> {
  try {
    return await acquireFSARootMutationLease(
      parent,
      options.lockManager,
      undefined,
      options.diagnostics?.performance,
      options.mutationIdentities,
    )
  } catch (error) {
    recordOutputException(options.diagnostics?.failures?.reopen, error)
    outputTrace(options.diagnostics, { eventName: 'reopen', transition: 'failed' })
    throw error
  }
}

function reportFSAReopenFailure(
  options: ReopenFileSystemAccessOutputOptions,
  operationId: string,
  error: unknown,
  materializationOpening: boolean,
): void {
  if (error instanceof TargetOwnershipUnknownError && error.stage !== 'checkpoint') {
    emitFSAOutputTrace(options.trace, needsAttention(operationId))
  }
  if (!materializationOpening) {
    recordOutputException(options.diagnostics?.failures?.reopen, error)
  }
  outputTrace(options.diagnostics, { eventName: 'reopen', transition: 'failed' })
}

async function releaseFailedFSAOpen(
  checkpoints: FSAFileCheckpointRepository | undefined,
  rootLease: FSARootMutationLease,
  diagnostics: OutputDiagnosticsPorts | undefined,
): Promise<readonly unknown[]> {
  const failures: unknown[] = []
  try {
    checkpoints?.close()
  } catch (error) {
    failures.push(error)
    recordOutputException(diagnostics?.failures?.cleanup, error)
  }
  try {
    await rootLease.release()
  } catch (error) {
    failures.push(error)
    recordOutputException(diagnostics?.failures?.cleanup, error)
  }
  if (failures.length !== 0) {
    outputTrace(diagnostics, { eventName: 'cleanup', transition: 'failed' })
  }
  return Object.freeze(failures)
}
