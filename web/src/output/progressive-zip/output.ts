import {
  outputCapabilities, outputExecutionProfile, outputSessionIdentity,
  snapshotOpenedOutputRevision, snapshotOutputFileRequest, VerifiedDurableRanges,
  VerifiedFinalOutputFile,
  type OutputSession, type OutputSessionIdentity, type OutputFileOwnership,
  type OpenedOutputRevision, type OutputFileRequest,
} from '../../transfer/output-file-contract'
import {
  snapshotDirectoryMaterializationRequest, type IncrementalDirectoryOutput,
} from '../../transfer/output-session'
import {
  createDirectoryAdmission, createDirectoryAdmissionScope, createDirectoryAdmissionSecret,
  finalizedDirectorySettlement, isImmediateChildPath, verifyDirectoryAdmissionReceipt,
  type DirectoryAdmission, type CanonicalModifiedTime,
} from '../../transfer/directory-admission'
import { FaultDomain } from '../../transfer/fault'
import { normalizeV2FileTransferFailure } from '../../transfer/job/failures'
import type { ReceiveIntent } from '../../transfer/intent'
import {
  ZIP_OBJECT_CHECKPOINT_PENDING_BYTES, ZIP_OBJECT_CHECKPOINT_PENDING_MILLISECONDS,
} from '../../transfer/checkpoint-schedule'
import { ZipCapacityWindow } from './capacity-window'
import { completeZipEntryCrc } from './crc-ranges'
import type { ProgressiveZipArchive } from './archive'

const CONCURRENT_FILE_PIPELINES = 4
const WRITE_BUDGET_BYTES = 8n * 1024n * 1024n

export async function createProgressiveZipOutput(input: {
  readonly archive: ProgressiveZipArchive
  readonly identity: OutputSessionIdentity
  readonly intent: ReceiveIntent
}): Promise<{ output: OutputSession; directories: IncrementalDirectoryOutput }> {
  const { archive, intent } = input
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind !== 'zip-archive' ||
      archive.state.object.operationId !== intent.operationId) {
    throw new TypeError('ZIP output does not belong to the frozen receive operation')
  }
  const identity = outputSessionIdentity(input.identity)
  const scope = await createDirectoryAdmissionScope(intent)
  const secret = createDirectoryAdmissionSecret()
  const capacity = new ZipCapacityWindow(CONCURRENT_FILE_PIPELINES,
    (stage, activeEntries) => archive.observeCapacityWindow(stage, activeEntries))
  const verify = async (receipt: DirectoryAdmission) => {
    if (!await verifyDirectoryAdmissionReceipt(secret, scope, receipt)) {
      throw new TypeError('ZIP directory receipt does not belong to this execution')
    }
  }
  const directories: IncrementalDirectoryOutput = {
    admitDirectory: async (input, signal) => {
      await capacity.beforeDirectory(signal)
      const request = snapshotDirectoryMaterializationRequest(input)
      signal.throwIfAborted()
      if (request.directory.parentAdmission !== undefined) await verify(request.directory.parentAdmission)
      const receipt = await createDirectoryAdmission(secret, scope, request.directory)
      await archive.admitDirectory({
          entryId: `directory:${request.directory.directoryId}`,
          path: request.logicalArtifactPath,
          source: {
            shareInstance: intent.shareInstance, directoryId: request.directory.directoryId,
            generation: request.directory.generation, sourcePath: request.sourceAuthenticationPath,
          },
          ...modifiedTime(request.directory.modifiedTime),
      })
      return receipt
    },
    finalizeDirectory: async (admission, signal) => {
      signal.throwIfAborted()
      await verify(admission)
      return finalizedDirectorySettlement(admission)
    },
  }
  const output: OutputSession = {
    identity,
    capabilities: outputCapabilities({
      durability: 'ProcessRestart', randomWrite: true, fileFailureIsolation: true, modificationTime: true,
    }),
    executionProfile: outputExecutionProfile({
      maximumConcurrentFilePipelines: CONCURRENT_FILE_PIPELINES,
      maximumOutstandingWriteBytes: WRITE_BUDGET_BYTES, maximumBufferedBytes: WRITE_BUDGET_BYTES,
    }),
    beginFile: async (input, signal) => {
      signal.throwIfAborted()
      const request = snapshotOutputFileRequest(input)
      const parent = request.parentAdmission
      if (parent === undefined) throw new TypeError('ZIP file lacks authenticated parent admission')
      await verify(parent)
      if (!isImmediateChildPath(parent.path, request.materializationRelativePath)) {
        throw new TypeError('ZIP file is outside its authenticated parent directory')
      }
      const entryId = `file:${request.source.fileId}`
      const capacityEntry = capacity.enter(entryId)
      try {
        const revision = await openZipRevision(archive, request, entryId, signal)
        const entry = await archive.admitFile({
            entryId, path: request.logicalArtifactPath,
            source: {
              shareInstance: revision.shareInstance, directoryId: parent.directoryId,
              generation: parent.generation, sourcePath: request.sourceAuthenticationPath,
            },
            revision, ...modifiedTime(request.modifiedTime),
        })
        const ownership: OutputFileOwnership = {
          ...identity, canonicalPath: request.logicalArtifactPath,
          ownedFileIdentity: `${archive.state.object.objectId}:${entryId}`,
        }
        let terminal = false
        const finish = () => {
          terminal = true
          capacityEntry.finish()
        }
        const durable = async () => {
          const current = await archive.committedEntry(entryId)
          return new VerifiedDurableRanges(ownership, revision, revision.exactSize, current.ranges)
        }
        return {
          revision,
          checkpoint: {
            objectId: archive.state.object.objectId,
            policy: { kind: 'incremental', pendingBytes: ZIP_OBJECT_CHECKPOINT_PENDING_BYTES,
              pendingMilliseconds: ZIP_OBJECT_CHECKPOINT_PENDING_MILLISECONDS },
          },
          durableRanges: new VerifiedDurableRanges(ownership, revision, revision.exactSize, entry.ranges),
          transaction: {
            writeRange: async (offset, bytes, signal) => {
              signal.throwIfAborted()
              if (terminal) throw new Error('ZIP entry transaction is closed')
              try { await archive.writeRange(entryId, offset, bytes) } catch (error) {
                if (!archive.failed && !signal.aborted && error instanceof DOMException &&
                    error.name === 'QuotaExceededError') throw capacityEntry.blocked(error)
                throw error
              }
            },
            automaticCheckpoint: async (trigger, signal) => {
              signal.throwIfAborted()
              await archive.checkpoint(`zip-${trigger}`)
              return { kind: 'advanced', durable: await durable() }
            },
            commit: async signal => {
              signal.throwIfAborted()
              await archive.checkpoint('zip-file-complete')
              const current = await archive.entry(entryId)
              if (completeZipEntryCrc(current.ranges, revision.exactSize) === undefined) {
                throw new Error('ZIP final checkpoint does not cover the opened revision')
              }
              finish()
              return new VerifiedFinalOutputFile(ownership, revision, revision.exactSize)
            },
            recordSourceFailure: fault => archive.markRevisionFailure(entryId, fault.code),
            retire: async () => {
              finish()
              return archive.failed ? 'JobOutputCompromised' : 'FileIsolated'
            },
            pause: async () => {
              try {
                await archive.checkpoint('zip-file-pause')
                return await durable()
              } finally { finish() }
            },
          },
        }
      } catch (error) {
        capacityEntry.finish()
        throw error
      }
    },
  }
  return { output, directories }
}

function modifiedTime(value: CanonicalModifiedTime | undefined): { modifiedTimeMilliseconds?: bigint } {
  return value === undefined ? {} : {
    modifiedTimeMilliseconds: value.seconds * 1000n + BigInt(Math.floor(value.nanoseconds / 1_000_000)),
  }
}
async function openZipRevision(
  archive: ProgressiveZipArchive, request: OutputFileRequest, entryId: string, signal: AbortSignal,
): Promise<OpenedOutputRevision> {
  try {
    const retained = await archive.findCommittedEntry(entryId)
    const complete = retained?.revision !== undefined &&
      retained.source.shareInstance === request.source.shareInstance &&
      retained.revision.fileId === request.source.fileId &&
      retained.path.join('/') === request.logicalArtifactPath.join('/') &&
      retained.source.sourcePath.join('/') === request.sourceAuthenticationPath.join('/') &&
      completeZipEntryCrc(retained.ranges, retained.revision.exactSize) !== undefined
    // Complete local bytes keep their authenticated revision when the sender replaces its source.
    const revision = complete
      ? snapshotOpenedOutputRevision({ ...retained.revision!, shareInstance: retained.source.shareInstance })
      : snapshotOpenedOutputRevision(await request.openRevision(signal))
    if (revision.shareInstance !== request.source.shareInstance || revision.fileId !== request.source.fileId) {
      throw new TypeError('ZIP opened revision differs from the authenticated catalog identity')
    }
    return revision
  } catch (error) {
    const failure = normalizeV2FileTransferFailure(error)
    if (failure.kind === 'fault' && failure.fault.domain === FaultDomain.Source &&
        await archive.findCommittedEntry(entryId) !== undefined) {
      await archive.markRevisionFailure(entryId, failure.fault.code)
    }
    throw error
  }
}
