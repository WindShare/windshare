import {
  observePausedTask,
  reconstructPausedTask,
  type PausedTaskTraceListener,
  type ReconstructedPausedTask,
  type ResumeStateOperationRequest,
  type TransferRunFactory,
} from '../../resume/authority'
import {
  assertPausedTaskCurrentShare,
  type PausedTaskDescriptorV1,
} from '../../resume/descriptor'
import {
  FILE_SYSTEM_ACCESS_BACKEND,
  ORIGIN_PRIVATE_BACKEND,
} from '../../capability/contract'
import { acquireFileSystemAccessOutputSession } from '../../file-system-access/session'
import {
  openOriginPrivateOutputSession,
  type OriginPrivateStorage,
} from '../../origin-private/session'
import { OriginPrivateZipExporter } from '../../origin-private/zip-exporter'
import {
  createTransferRun,
} from '../../../transfer/intent'
import { directoryAdmissionScope } from '../../../transfer/output-session'
import {
  assertPreparedCapabilityCurrent,
  PausedTaskCapabilityError,
  sameDirectoryEntry,
  verifyPreparedRootIdentity,
  type StoredRootCapability,
} from './records'

export {
  PausedTaskCapabilityError,
  PausedTaskDescriptorConflictError,
  type PausedTaskCapabilityFailure,
} from './records'

export interface FileSystemAccessPermissionPort {
  requestWritePermission(root: FileSystemDirectoryHandle): Promise<PermissionState>
}

export interface BrowserPausedTaskStateOptions {
  readonly databaseName?: string
  readonly permission?: FileSystemAccessPermissionPort
  readonly originPrivateStorage?: OriginPrivateStorage
  readonly createRun?: TransferRunFactory
  readonly onTrace?: PausedTaskTraceListener
}

export type BrowserPausedTaskResumeRequest = ResumeStateOperationRequest

export interface BrowserPausedTaskDependencies {
  readonly permission: FileSystemAccessPermissionPort
  readonly originPrivateStorage: OriginPrivateStorage
  readonly createRun: TransferRunFactory
  readonly onTrace?: PausedTaskTraceListener
}

export function browserPausedTaskDependencies(
  options: BrowserPausedTaskStateOptions,
): BrowserPausedTaskDependencies {
  return Object.freeze({
    permission: options.permission ?? BROWSER_FILE_SYSTEM_ACCESS_PERMISSION,
    originPrivateStorage: options.originPrivateStorage ?? defaultOriginPrivateStorage(),
    createRun: options.createRun ?? createTransferRun,
    ...(options.onTrace === undefined ? {} : { onTrace: options.onTrace }),
  })
}

export async function resumePreparedPausedTask(
  databaseName: string,
  descriptor: PausedTaskDescriptorV1,
  preparedCapability: StoredRootCapability,
  request: BrowserPausedTaskResumeRequest,
  dependencies: BrowserPausedTaskDependencies,
): Promise<ReconstructedPausedTask> {
  assertPausedTaskCurrentShare(descriptor, request.currentShare)
  try {
    if (descriptor.intent.output.backend === FILE_SYSTEM_ACCESS_BACKEND) {
      return await resumeFileSystemAccessPausedTask(
        databaseName,
        descriptor,
        preparedCapability,
        request,
        dependencies,
      )
    }
    return await resumeOriginPrivatePausedTask(
      databaseName,
      descriptor,
      preparedCapability,
      request,
      dependencies,
    )
  } catch (error) {
    observePausedTask(
      dependencies.onTrace,
      'paused-task-capability-rejected',
      descriptor,
      { decision: capabilityFailureDecision(error) },
    )
    throw error
  }
}

async function resumeFileSystemAccessPausedTask(
  databaseName: string,
  descriptor: PausedTaskDescriptorV1,
  preparedCapability: StoredRootCapability,
  request: BrowserPausedTaskResumeRequest,
  dependencies: BrowserPausedTaskDependencies,
): Promise<ReconstructedPausedTask> {
  const permission = dependencies.permission.requestWritePermission(
    preparedCapability.handle,
  )
  let state: PermissionState
  try {
    state = await permission
  } catch (error) {
    throw new PausedTaskCapabilityError(
      'permission-denied',
      'File System Access permission renewal failed',
      { cause: error },
    )
  }
  if (state !== 'granted') {
    throw new PausedTaskCapabilityError(
      'permission-denied',
      'File System Access permission was not granted',
    )
  }
  await assertPreparedCapabilityCurrent(
    databaseName,
    descriptor,
    preparedCapability,
  )
  await verifyPreparedRootIdentity(
    databaseName,
    FILE_SYSTEM_ACCESS_BACKEND,
    descriptor.intent.output.target,
    preparedCapability.handle,
  )
  observePausedTask(
    dependencies.onTrace,
    'paused-task-capability-accepted',
    descriptor,
    { decision: 'fsa-permission-and-same-entry' },
  )
  return reconstructPausedTask({
    descriptor,
    currentShare: request.currentShare,
    createRun: dependencies.createRun,
    ...(dependencies.onTrace === undefined ? {} : { onTrace: dependencies.onTrace }),
    openSession: (run) => acquireFileSystemAccessOutputSession(
      preparedCapability.handle,
      {
        outputSessionId: run.outputSessionId,
        directoryAdmissionScope: directoryAdmissionScope(descriptor.intent),
        transferIntentDigest: descriptor.intent.digest,
        rootIdentity: descriptor.intent.output.target,
        databaseName,
      },
    ),
  })
}

async function resumeOriginPrivatePausedTask(
  databaseName: string,
  descriptor: PausedTaskDescriptorV1,
  preparedCapability: StoredRootCapability,
  request: BrowserPausedTaskResumeRequest,
  dependencies: BrowserPausedTaskDependencies,
): Promise<ReconstructedPausedTask> {
  if (descriptor.intent.output.backend !== ORIGIN_PRIVATE_BACKEND) {
    throw new PausedTaskCapabilityError(
      'unsupported',
      'Paused task backend cannot be reconstructed by this browser',
    )
  }
  const acquireOutput = request.acquireOriginPrivateOutput
  if (acquireOutput === undefined) {
    throw new PausedTaskCapabilityError(
      'unsupported',
      'OPFS resume requires a fresh final output capability',
    )
  }
  // Start the picker/stream operation before any await so transient user
  // activation remains available to the application-owned acquisition port.
  const outputOperation = acquireOutput()
  let rootOperation: Promise<FileSystemDirectoryHandle>
  try {
    rootOperation = dependencies.originPrivateStorage.getDirectory()
  } catch (error) {
    await abortAcquiredOutput(outputOperation, error)
    throw error
  }
  const [outputResult, rootResult] = await Promise.allSettled([
    outputOperation,
    rootOperation,
  ])
  if (outputResult.status === 'rejected') throw outputResult.reason
  if (rootResult.status === 'rejected') {
    await outputResult.value.abort(rootResult.reason).catch(() => undefined)
    throw rootResult.reason
  }
  const output = outputResult.value
  const currentRoot = rootResult.value
  const exporter = new OriginPrivateZipExporter(output)
  try {
    if (!await sameDirectoryEntry(currentRoot, preparedCapability.handle)) {
      throw new PausedTaskCapabilityError(
        'stale',
        'The origin-private root capability no longer matches the paused task',
      )
    }
    await assertPreparedCapabilityCurrent(
      databaseName,
      descriptor,
      preparedCapability,
    )
    await verifyPreparedRootIdentity(
      databaseName,
      ORIGIN_PRIVATE_BACKEND,
      descriptor.intent.output.target,
      currentRoot,
    )
    observePausedTask(
      dependencies.onTrace,
      'paused-task-capability-accepted',
      descriptor,
      { decision: 'opfs-root-and-fresh-export' },
    )
    return await reconstructPausedTask({
      descriptor,
      currentShare: request.currentShare,
      createRun: dependencies.createRun,
      ...(dependencies.onTrace === undefined ? {} : { onTrace: dependencies.onTrace }),
      openSession: (run) => openOriginPrivateOutputSession({
        outputSessionId: run.outputSessionId,
        directoryAdmissionScope: directoryAdmissionScope(descriptor.intent),
        transferIntentDigest: descriptor.intent.digest,
        rootIdentity: descriptor.intent.output.target,
        storage: {
          getDirectory: async () => currentRoot,
          ...(dependencies.originPrivateStorage.estimate === undefined
            ? {}
            : { estimate: () => dependencies.originPrivateStorage.estimate!() }),
        },
        exporter,
        databaseName,
      }),
    })
  } catch (error) {
    await exporter.abort(error).catch(() => undefined)
    throw error
  }
}

export async function abortAcquiredOutput(
  output: Promise<WritableStream<Uint8Array>>,
  reason: unknown,
): Promise<void> {
  const settled = await Promise.allSettled([output])
  const acquired = settled[0]
  if (acquired?.status === 'fulfilled') {
    await acquired.value.abort(reason).catch(() => undefined)
  }
}

function defaultOriginPrivateStorage(): OriginPrivateStorage {
  return {
    getDirectory: () => {
      const storage = navigator.storage as StorageManager & {
        getDirectory?: () => Promise<FileSystemDirectoryHandle>
      }
      if (storage.getDirectory === undefined) {
        return Promise.reject(new DOMException(
          'Origin-private storage is unavailable',
          'NotSupportedError',
        ))
      }
      return storage.getDirectory()
    },
    estimate: () => navigator.storage.estimate(),
  }
}

const BROWSER_FILE_SYSTEM_ACCESS_PERMISSION: FileSystemAccessPermissionPort = Object.freeze({
  requestWritePermission(root: FileSystemDirectoryHandle): Promise<PermissionState> {
    const permissionRoot = root as FileSystemDirectoryHandle & {
      requestPermission?: (
        descriptor?: { readonly mode?: 'read' | 'readwrite' },
      ) => Promise<PermissionState>
    }
    if (permissionRoot.requestPermission === undefined) {
      return Promise.reject(new PausedTaskCapabilityError(
        'unsupported',
        'File System Access permission renewal is unavailable',
      ))
    }
    return permissionRoot.requestPermission({ mode: 'readwrite' })
  },
})

function capabilityFailureDecision(error: unknown): string {
  if (error instanceof PausedTaskCapabilityError) return error.failure
  if (error instanceof DOMException) return error.name
  return 'dependency-failure'
}
