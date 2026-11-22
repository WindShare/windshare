import { decodeBase64Url, encodeBase64Url } from '../../src/crypto/bytes'
import { browserBuildSnapshot } from '../../src/diagnostics/build-identity'
import { createBrowserDiagnosticsComposition } from '../../src/application/diagnostics/composition'
import type { IncidentRecordV2 } from '../../src/diagnostics/export/incident-record-v2'
import {
  createFailureIdentity,
  incidentPresentationDecision,
  nativeOutputFailureFact,
  type ProtocolMessageKindV1,
} from '../../src/diagnostics/incident'
import {
  createOutputTraceSource,
  createProtocolTraceSource,
} from '../../src/application/diagnostics/receive-trace'
import type { ReceiverDiagnosticSnapshot } from '../../src/diagnostics/export/context'
import { reopenFSASession } from '../../src/output/backends/fsa'
import { ReceiveStore } from '../../src/output/store/database'
import { acquireTaskLock } from '../../src/output/store/tab-lock'
import type { ReceiveTaskRecord } from '../../src/output/store/types'
import { classifyNativeError } from '../../src/transfer/fault'
import { initialReceiveState, transitionReceiveState, type ReceiveLifecycleState } from '../../src/transfer/state'

const EVIDENCE_CLOCK_MILLISECONDS = Date.parse('2026-08-19T09:00:00.000Z')
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

export type FSAReconstructionAttempt =
  | Readonly<{ kind: 'unavailable'; reason: 'origin-private-storage-unavailable' }>
  | Readonly<{ kind: 'reconstructed'; result: FSAReconstructionResult }>

/**
 * Reproduces a real cross-tab lock conflict when reopening a paused FSA task against
 * `ReceiveStore` + `navigator.locks`, verifying that diagnostic bundles preserve typed
 * failure context and protocol correlation without leaking private file names or paths.
 */
export async function reconstructFSAContinuationFailure(
  input: FSAReconstructionCorrelation,
): Promise<FSAReconstructionAttempt> {
  const getDirectory = reconstructionDirectory()
  if (getDirectory === undefined) {
    return Object.freeze({ kind: 'unavailable', reason: 'origin-private-storage-unavailable' })
  }
  const correlation = protocolCorrelation(input)
  const taskId = identity(4)
  const databaseName = `windshare-reconstruction-${crypto.randomUUID()}`
  const parent = await getDirectory()
  const store = await ReceiveStore.open(databaseName)
  const paused = transitionReceiveState(initialReceiveState(taskId, EVIDENCE_CLOCK_MILLISECONDS), {
    kind: 'paused',
    reason: 'user',
    completedFiles: 1n,
    completedBytes: 16n,
    retainedBytes: 16n,
    discoveredFiles: 1n,
    discoveredBytes: 16n,
    discoveryComplete: true,
  }, EVIDENCE_CLOCK_MILLISECONDS + 10)
  const taskRecord: ReceiveTaskRecord = Object.freeze({
    taskId,
    shareInstance: identity(1),
    selectionKey: 'reconstruction-selection',
    selection: { shareInstance: identity(1), syntheticRoot: identity(2), defaultSelected: true, rules: [] },
    artifact: { kind: 'directory-tree' as const, name: PRIVATE_FIXTURE_NAME, singleFileId: identity(3), singleFilePath: [PRIVATE_FIXTURE_NAME], singleFileSize: 16n },
    backend: 'fsa' as const,
    lifecycle: paused,
    target: { kind: 'fsa' as const, parentHandle: parent, reservedName: PRIVATE_FIXTURE_NAME, collisionIndex: 0, singleFile: true },
    discoveryComplete: true,
    updatedAt: EVIDENCE_CLOCK_MILLISECONDS + 10,
  })
  await store.putTask(taskRecord)

  let clockMilliseconds = EVIDENCE_CLOCK_MILLISECONDS
  let lifecycle: ReceiveLifecycleState = paused
  const incidentConsole: unknown[] = []
  const snapshot = (): ReceiverDiagnosticSnapshot => Object.freeze({
    controller: Object.freeze({ generation: 7n, phase: 'browsing' }),
    lifecycle: Object.freeze({ generation: lifecycle.generation, state: lifecycle.kind }),
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
      capacityWaitingFiles: 0n,
      capacityAccumulatedWaitMilliseconds: 0n,
      capacityWaitAttempts: 0n,
      capacityWaitVisible: false,
    }),
    output: Object.freeze({ generation: 7n, planKind: 'fsa' }),
  })
  const diagnostics = createBrowserDiagnosticsComposition({
    build: browserBuildSnapshot(),
    secureContext: globalThis.isSecureContext,
    consoleSink: Object.freeze({ error: (record: IncidentRecordV2) => incidentConsole.push(record) }),
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
  const competingLock = await acquireTaskLock(taskId)
  const scope = diagnostics.incidents.openScope('retained_action')
  let actionError: unknown
  try {
    await reopenFSASession({
      task: taskRecord,
      store,
      allowPermissionPrompt: false,
      diagnostics: { backend: 'file_system_access', trace: outputTrace },
    })
    throw new Error('FSA reopen unexpectedly succeeded while another tab held the Web Lock')
  } catch (error) {
    actionError = error
    const trigger = scope.facts.record(nativeOutputFailureFact({
      stage: 'reopen',
      recoveryDisposition: 'resumable_receive',
      nativeClass: classifyNativeError(error),
      operation: 'reopen',
      error,
    }), 'contributor')
    diagnostics.incidents.submitDecision(scope.handle, incidentPresentationDecision('retained_action', 'failed', trigger))
  } finally {
    scope.close()
    await competingLock.release()
    lifecycle = (await store.getTask(taskId))?.lifecycle ?? paused
    store.close()
    await deleteDatabase(databaseName)
  }

  const bundle = diagnostics.runtime.export()
  return Object.freeze({
    kind: 'reconstructed',
    result: Object.freeze({
      bundle,
      incidentConsoleCount: incidentConsole.length,
      actionErrorName: actionError instanceof Error ? actionError.name : 'unknown',
      restoredLifecycle: lifecycle.kind,
      forbiddenSentinels: Object.freeze([PRIVATE_FIXTURE_NAME]),
    }),
  })
}

function protocolCorrelation(input: FSAReconstructionCorrelation) {
  const session = decodeBase64Url(input.protocolSessionId)
  if (session?.byteLength !== 16 || !/^[1-9]\d*$/.test(input.protocolOperationId)) {
    throw new TypeError('shared protocol correlation is not canonical')
  }
  return Object.freeze({
    protocolSessionId: createFailureIdentity('protocol_session', session),
    protocolOperationId: createFailureIdentity('protocol_operation', BigInt(input.protocolOperationId)),
  })
}

function identity(seed: number, length = 16): string {
  return encodeBase64Url(identityBytes(seed, length))
}

function identityBytes(seed: number, length: number): Uint8Array {
  return new Uint8Array(length).fill(seed & 0xff)
}

function reconstructionDirectory(): (() => Promise<FileSystemDirectoryHandle>) | undefined {
  const target = globalThis as typeof globalThis & Readonly<{
    __windshareReconstructionDirectory?: () => Promise<FileSystemDirectoryHandle>
  }>
  return target.__windshareReconstructionDirectory
}

function deleteDatabase(name: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const req = indexedDB.deleteDatabase(name)
    req.onsuccess = () => resolve()
    req.onerror = () => reject(req.error)
    req.onblocked = () => resolve()
  })
}
