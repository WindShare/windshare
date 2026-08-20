import { decodeBase64Url, encodeBase64Url } from '../../src/crypto/bytes'
import { browserBuildSnapshot } from '../../src/diagnostics/build-identity'
import { createBrowserDiagnosticsComposition } from '../../src/diagnostics/browser-composition'
import type { IncidentRecordV1 } from '../../src/diagnostics/export/incident-record-v1'
import {
  createFailureIdentity,
  type ProtocolMessageKindV1,
} from '../../src/diagnostics/incident'
import { prepareFSAOperationBindingTransition } from '../../src/output/browser/indexeddb-root-binding'
import { IndexedDbReceiveOperationRepository } from '../../src/output/browser/indexeddb-repository'
import { fsaRootMutationLockName } from '../../src/output/browser/namespace-mutation'
import {
  type OutputDiagnosticsPorts,
  type OutputFailureSinks,
} from '../../src/output/diagnostics'
import { PersistedReceiveOperationReopenAuthority } from '../../src/output/resume/reopen-authority'
import { receiveOperationResumeDescriptor } from '../../src/output/resume/descriptor'
import {
  decodeStoredReceiveLifecycleState,
  storedReceiveLifecycleState,
} from '../../src/output/workspace/state-codec'
import {
  STABLE_RETENTION_MILLISECONDS,
  type ReceiveLifecycleState,
} from '../../src/output/workspace'
import {
  createDirectTreePlan,
  createFSANamedEntryReservation,
  createReceiveIntent,
  createSelectionSpec,
  createSingleFileDirectoryTreeArtifact,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import { FSAReceiveOperation } from '../../src/ui/browser-receive/fsa'
import { RetainedInventoryCoordinator } from '../../src/ui/controller/retained-inventory'
import type { V2JoinedBrowserShare } from '../../src/ui/v2-gateway'
import type { V2ReceiverDiagnosticSnapshot } from '../../src/ui/v2-model'
import type {
  V2ReceiveCompositionPort,
  V2RetainedReceiveInventory,
  V2RetainedReceiveOperation,
} from '../../src/ui/v2-receive-runtime'
import {
  createOutputTraceSource,
  createProtocolTraceSource,
  createV2ReceiverTraceSource,
} from '../../src/ui/v2-production-trace'
const EVIDENCE_CLOCK_MILLISECONDS = Date.parse('2026-08-19T09:00:00.000Z')
const ACTION_TIMEOUT_MILLISECONDS = 5_000
const PRIVATE_REOPEN_DETAIL = 'private FSA continuation detail C:/receiver/secret.txt'
const PRIVATE_CLEANUP_DETAIL = 'private lease cleanup detail /receiver/secret.txt'
const PRIVATE_FIXTURE_NAME = 'private-fixture.bin'

export interface FSAReconstructionCorrelation {
  readonly protocolSessionId: string
  readonly protocolOperationId: string
  readonly requestKind: ProtocolMessageKindV1
}

export interface FSAReconstructionResult {
  readonly bundle: string
  readonly incidentConsoleCount: number
  readonly actionErrorName: string
  readonly restoredLifecycle: string
  readonly forbiddenSentinels: readonly string[]
}

/**
 * Reproduces the failure at the real FSA reopen/settlement seam while keeping the
 * network operation external. The caller supplies a tuple observed from the live
 * browser/sender exchange so this bundle and the sender artifact share authority.
 */
export async function reconstructFSAContinuationFailure(
  input: FSAReconstructionCorrelation,
): Promise<FSAReconstructionResult> {
  const correlation = protocolCorrelation(input)
  const intent = await directTreeIntent()
  const databaseName = `windshare-w6-reconstruction-${crypto.randomUUID()}`
  const parent = await reconstructionDirectory()
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  const stableLifecycle = resumableReceive(intent, 4n)
  const binding = await prepareFSAOperationBindingTransition({
    repository,
    intent,
    parent,
  })
  await repository.commitTransition({
    operationId: intent.operationId,
    records: [
      ...(binding.transition.records ?? []),
      await storedReceiveLifecycleState(stableLifecycle),
    ],
    ...(binding.transition.handles === undefined
      ? {}
      : { handles: binding.transition.handles }),
  })
  repository.close()

  let clockMilliseconds = EVIDENCE_CLOCK_MILLISECONDS
  let lifecycle: ReceiveLifecycleState = stableLifecycle
  const incidentConsole: unknown[] = []
  const snapshot = (): V2ReceiverDiagnosticSnapshot => Object.freeze({
    controller: Object.freeze({ generation: 7n, phase: 'browsing' }),
    lifecycle: Object.freeze({
      generation: lifecycle.generation,
      state: lifecycle.kind,
    }),
    progress: Object.freeze({
      generation: 7n,
      discovery: 'complete',
      discoveredFiles: 1n,
      discoveredBytes: 16n,
      writtenBytes: 16n,
      completedFiles: 1n,
      completedBytes: 16n,
      fileErrors: 1n,
      selectionErrors: 0n,
      failedDirectories: 0n,
      contentLanes: 1,
    }),
    output: Object.freeze({ generation: 7n, planKind: 'direct-tree' }),
  })
  const diagnostics = createBrowserDiagnosticsComposition({
    build: browserBuildSnapshot(),
    secureContext: globalThis.isSecureContext,
    consoleSink: Object.freeze({
      error: (record: IncidentRecordV1) => incidentConsole.push(record),
    }),
    controllerSnapshot: snapshot,
    randomBytes: length => identityBytes(211, length),
    clock: Object.freeze({
      nowMilliseconds: () => clockMilliseconds,
      captureTime: () => new Date(clockMilliseconds).toISOString(),
    }),
  })
  diagnostics.runtime.enable()

  const protocolTrace = createProtocolTraceSource(diagnostics.trace).current
  if (protocolTrace === undefined) throw new Error('diagnostic trace did not enable')
  protocolTrace(Object.freeze({
    eventName: 'protocol_operation',
    transition: 'request_sent',
    requestKind: input.requestKind,
    correlation,
  }))
  clockMilliseconds += 1

  const outputTrace = createOutputTraceSource(diagnostics.trace)
  const operation = retainedOperation(intent, stableLifecycle)
  let listCount = 0
  const inventory: V2RetainedReceiveInventory = Object.freeze({
    operations: Object.freeze([operation]),
    presentationFailures: Object.freeze([]),
    act: async (
      _operation: V2RetainedReceiveOperation,
      action: Parameters<V2RetainedReceiveInventory['act']>[1],
      signal: AbortSignal,
      failures?: OutputFailureSinks,
    ) => {
      if (action !== 'continue') throw new TypeError('reconstruction requires continuation')
      signal.throwIfAborted()
      const outputDiagnostics = diagnosticsFor(outputTrace, failures)
      const reopen = new PersistedReceiveOperationReopenAuthority({
        repositoryFactory: () => IndexedDbReceiveOperationRepository.open(databaseName),
        clock: { now: () => clockMilliseconds },
        diagnostics: outputDiagnostics,
      })
      const reopened = await reopen.reopen(
        requiredDescriptor(stableLifecycle, clockMilliseconds),
        'continue',
        failures,
      )
      if (reopened.kind !== 'direct-tree') {
        throw new TypeError('FSA reconstruction reopened a non-FSA operation')
      }
      const releaseRootLock = await holdFSARootLock(parent)
      try {
        await FSAReceiveOperation.reopen(reopened, outputDiagnostics)
        throw new Error('FSA continuation unexpectedly reopened')
      } catch (error) {
        await releaseRootLock()
        lifecycle = await readLifecycle(databaseName, intent.operationId)
        // Losing the lease at this cut models the owned close failure that follows
        // the primary reopen/continuation result; it must remain a consequence.
        await removeLease(databaseName, intent.operationId, reopened.lease.leaseId)
        let cleanupError: unknown
        try {
          await reopened.close()
        } catch (errorClosingAuthority) {
          // The reopen authority owns both the native cleanup classification and
          // its trace event; this harness must not duplicate that evidence.
          cleanupError = errorClosingAuthority
        }
        if (cleanupError !== undefined) {
          throw new AggregateError(
            [error, cleanupError],
            `${PRIVATE_REOPEN_DETAIL}; ${PRIVATE_CLEANUP_DETAIL}`,
            { cause: error },
          )
        }
        throw error
      }
    },
    close: () => undefined,
  })
  const emptyInventory: V2RetainedReceiveInventory = Object.freeze({
    operations: Object.freeze([]),
    presentationFailures: Object.freeze([]),
    act: () => Promise.reject(new TypeError('empty inventory has no actions')),
    close: () => undefined,
  })
  const receive = Object.freeze({
    retained: Object.freeze({
      list: async () => {
        listCount += 1
        return listCount === 1 ? inventory : emptyInventory
      },
    }),
  }) as unknown as V2ReceiveCompositionPort
  const joined = Object.freeze({
    descriptor: Object.freeze({
      shareInstanceId: intent.selection.shareInstance,
      syntheticRootId: intent.selection.syntheticRoot,
    }),
    protocolSessionId: input.protocolSessionId,
  }) as unknown as V2JoinedBrowserShare
  let actionError: unknown
  const coordinator = new RetainedInventoryCoordinator({
    receive,
    isDisposed: () => false,
    currentJoinedShare: () => joined,
    continuationBlocked: () => false,
    adoptContinuation: () => Promise.reject(new Error('failed continuation cannot be adopted')),
    ownsRuntime: () => false,
    publish: () => undefined,
    trace: createV2ReceiverTraceSource(diagnostics.trace),
    onActionError: error => { actionError = error },
    incidents: diagnostics.incidents,
  })

  await coordinator.load()
  coordinator.perform(operation, 'continue')
  await waitFor(() => actionError !== undefined && diagnostics.runtime.inspectLastFailure() !== null)
  coordinator.close(new DOMException('evidence completed', 'AbortError'))
  const bundle = diagnostics.runtime.export()

  return Object.freeze({
    bundle,
    incidentConsoleCount: incidentConsole.length,
    actionErrorName: actionError instanceof Error ? actionError.name : 'unknown',
    restoredLifecycle: lifecycle.kind,
    forbiddenSentinels: Object.freeze([
      PRIVATE_REOPEN_DETAIL,
      PRIVATE_CLEANUP_DETAIL,
      PRIVATE_FIXTURE_NAME,
    ]),
  })
}

function protocolCorrelation(input: FSAReconstructionCorrelation) {
  const session = decodeBase64Url(input.protocolSessionId)
  const operation = decodeBase64Url(input.protocolOperationId)
  if (session?.byteLength !== 16 || operation?.byteLength !== 16) {
    throw new TypeError('shared protocol correlation is not a canonical 16-byte tuple')
  }
  return Object.freeze({
    protocolSessionId: createFailureIdentity('protocol_session', session),
    protocolOperationId: createFailureIdentity('protocol_operation', operation),
  })
}

function retainedOperation(
  intent: ReceiveIntent,
  lifecycle: ReturnType<typeof resumableReceive>,
): V2RetainedReceiveOperation {
  return Object.freeze({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    lifecycleGeneration: lifecycle.generation,
    lifecycle,
    continuation: 'resume-receive',
    actions: Object.freeze(['continue'] as const),
  })
}

function diagnosticsFor(
  trace: ReturnType<typeof createOutputTraceSource>,
  failures: OutputFailureSinks | undefined,
): OutputDiagnosticsPorts {
  return Object.freeze({
    backend: 'file_system_access',
    trace,
    ...(failures === undefined ? {} : { failures }),
  })
}

async function waitFor(predicate: () => boolean): Promise<void> {
  const deadline = performance.now() + ACTION_TIMEOUT_MILLISECONDS
  while (!predicate()) {
    if (performance.now() >= deadline) {
      throw new Error('FSA reconstruction did not produce an incident')
    }
    await new Promise(resolve => setTimeout(resolve, 0))
  }
}

async function directTreeIntent(): Promise<ReceiveIntent> {
  const selection = await createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const artifact = await createSingleFileDirectoryTreeArtifact({
    fileId: identity(3),
    sourcePath: PRIVATE_FIXTURE_NAME,
    outputName: PRIVATE_FIXTURE_NAME,
  })
  const reservation = await createFSANamedEntryReservation({
    operationId: identity(4),
    reservationId: identity(5),
    artifact,
    authorityRef: identity(6, 32),
    reservedName: PRIVATE_FIXTURE_NAME,
    collisionIndex: 0,
  })
  return createReceiveIntent({
    selection,
    artifact,
    plan: await createDirectTreePlan(artifact, reservation),
  })
}

function resumableReceive(
  intent: ReceiveIntent,
  generation: bigint,
): Extract<ReceiveLifecycleState, { readonly kind: 'resumable-receive' }> {
  return Object.freeze({
    kind: 'resumable-receive',
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    generation,
    checkpointSetDigest: identity(60, 32),
    completedFileCount: 1n,
    completedBytes: 16n,
    expiresAt: EVIDENCE_CLOCK_MILLISECONDS + STABLE_RETENTION_MILLISECONDS,
  })
}

function requiredDescriptor(
  lifecycle: ReceiveLifecycleState,
  now: number,
) {
  const descriptor = receiveOperationResumeDescriptor(lifecycle, now)
  if (descriptor === undefined) throw new Error('reconstruction lifecycle has no continuation')
  return descriptor
}

async function holdFSARootLock(
  parent: FileSystemDirectoryHandle,
): Promise<() => Promise<void>> {
  let acquired!: () => void
  const acquiredSignal = new Promise<void>(resolve => { acquired = resolve })
  let release!: () => void
  const releaseSignal = new Promise<void>(resolve => { release = resolve })
  const completion = navigator.locks.request(
    await fsaRootMutationLockName(parent),
    { mode: 'exclusive' },
    async () => {
      acquired()
      await releaseSignal
    },
  )
  await acquiredSignal
  return async () => {
    release()
    await completion
  }
}

async function readLifecycle(
  databaseName: string,
  operationId: string,
): Promise<ReceiveLifecycleState> {
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  try {
    const record = await repository.readLifecycle(operationId)
    if (record === undefined) throw new Error('restored FSA lifecycle is missing')
    return decodeStoredReceiveLifecycleState(record)
  } finally {
    repository.close()
  }
}

async function removeLease(
  databaseName: string,
  operationId: string,
  leaseId: string,
): Promise<void> {
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  try {
    await repository.commitTransition({
      operationId,
      expectedLeaseId: leaseId,
      lease: { kind: 'delete', leaseId },
    })
  } finally {
    repository.close()
  }
}

function identity(seed: number, length = 16): string {
  return encodeBase64Url(identityBytes(seed, length))
}

function identityBytes(seed: number, length: number): Uint8Array {
  return new Uint8Array(length).fill(seed & 0xff)
}

function reconstructionDirectory(): Promise<FileSystemDirectoryHandle> {
  const target = globalThis as typeof globalThis & Readonly<{
    __windshareReconstructionDirectory?: () => Promise<FileSystemDirectoryHandle>
  }>
  const getDirectory = target.__windshareReconstructionDirectory
  if (getDirectory === undefined) {
    throw new DOMException('Reconstruction storage is unavailable', 'NotSupportedError')
  }
  return getDirectory()
}
