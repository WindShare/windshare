import { taskEntryContent } from '../origin-private/task-checkpoint/content'
import type { TaskCheckpoint, TaskEntry } from '../origin-private/task-checkpoint/model'
import { assertContentAvailabilityAdvance } from '../../transfer/output/content-availability'
import {
  outputCapabilities, outputExecutionProfile, outputSessionIdentity,
  snapshotOpenedOutputRevision, snapshotOutputFileRequest, VerifiedDurableRanges,
  VerifiedFinalOutputFile,
  type OutputSession, type OutputSessionIdentity, type OutputFileOwnership,
  type BeginOutputFileResult, type OpenedOutputRevision, type OutputFileRequest,
} from '../../transfer/output/file'
import {
  snapshotDirectoryMaterializationRequest, type IncrementalDirectoryOutput,
} from '../../transfer/output/session'
import {
  createDirectoryAdmission, createDirectoryAdmissionScope, createDirectoryAdmissionSecret,
  finalizedDirectorySettlement, isImmediateChildPath, verifyDirectoryAdmissionReceipt,
  type DirectoryAdmission, type CanonicalModifiedTime,
} from '../../transfer/directory-admission'
import { FaultDomain, SourceFaultCode } from '../../transfer/failure/fault'
import { MAX_CONTENT_WARNING_FILES, type MissingContentFile } from '../../transfer/lifecycle/content-warning'
import { normalizeFileTransferFailure } from '../../transfer/job/failures'
import type { ReceiveIntent } from '../../transfer/intent'
import {
  ZIP_OBJECT_CHECKPOINT_PENDING_BYTES, ZIP_OBJECT_CHECKPOINT_PENDING_MILLISECONDS,
} from '../../transfer/checkpoint-schedule'
import { ZipCapacityWindow } from './capacity-window'
import { completeZipEntryCrc } from './crc-ranges'
import type { ProgressiveZipArchive } from './archive'

const CONCURRENT_FILE_PIPELINES = 4
const WRITE_BUDGET_BYTES = 8n * 1024n * 1024n

type ZipFileAdmission =
  | Readonly<{ kind: 'begin' }>
  | Readonly<{ kind: 'readmit'; paused: VerifiedDurableRanges }>

export async function createProgressiveZipOutput(input: {
  readonly archive: ProgressiveZipArchive
  readonly identity: OutputSessionIdentity
  readonly intent: ReceiveIntent
}): Promise<{ output: OutputSession; directories: IncrementalDirectoryOutput; missingFiles: () => readonly MissingContentFile[] }> {
  const { archive, intent } = input
  const missingFiles = new Map<string, MissingContentFile>()
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
  const output = createZipFileOutput({ archive, identity, capacity, verify, missingFiles })
  return { output, directories, missingFiles: () => Object.freeze([...missingFiles.values()]) }
}

function createZipFileOutput(input: {
  readonly archive: ProgressiveZipArchive
  readonly identity: OutputSessionIdentity
  readonly capacity: ZipCapacityWindow
  readonly verify: (admission: DirectoryAdmission) => Promise<void>
  readonly missingFiles: Map<string, MissingContentFile>
}): OutputSession {
  const { archive, identity, capacity, verify, missingFiles } = input
  return {
    identity,
    capabilities: outputCapabilities({
      durability: 'ProcessRestart', randomWrite: true, fileFailureIsolation: true, modificationTime: true,
    }),
    executionProfile: outputExecutionProfile({
      maximumConcurrentFilePipelines: CONCURRENT_FILE_PIPELINES,
      maximumOutstandingWriteBytes: WRITE_BUDGET_BYTES, maximumBufferedBytes: WRITE_BUDGET_BYTES,
    }),
    beginFile: (file, signal) => admitFile(file, { kind: 'begin' }, signal),
    readmitFile: (file, paused, signal) => admitFile(file, { kind: 'readmit', paused }, signal),
  }

  async function admitFile(
    input: OutputFileRequest, admission: ZipFileAdmission, signal: AbortSignal,
  ): Promise<BeginOutputFileResult> {
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
      const ownership: OutputFileOwnership = {
        ...identity, canonicalPath: request.logicalArtifactPath,
        ownedFileIdentity: `${archive.state.object.objectId}:${entryId}`,
      }
      const retained = await archive.findCommittedEntry(entryId)
      if (admission.kind === 'readmit') {
        requirePausedZipEntry(admission.paused, ownership, request, retained, archive.state)
      }
      const revision = await openZipRevision(archive, request, entryId,
        admission.kind === 'begin' ? retained : undefined, signal)
      signal.throwIfAborted()
      const entry = await archive.admitFile({
        entryId, path: request.logicalArtifactPath,
        source: {
          shareInstance: revision.shareInstance, directoryId: parent.directoryId,
          generation: parent.generation, sourcePath: request.sourceAuthenticationPath,
        },
        revision, ...modifiedTime(request.modifiedTime),
      })
      let terminal = false
      const finish = () => {
        terminal = true
        capacityEntry.finish()
      }
      const durable = async () => {
        const current = await archive.committedEntry(entryId)
        const content = taskEntryContent(current, archive.state)
        return new VerifiedDurableRanges(ownership, revision, revision.exactSize, content.ranges, content.replica)
      }
      const content = taskEntryContent(entry, archive.state)
      return {
        revision,
        checkpoint: {
          objectId: archive.state.object.objectId,
          policy: { kind: 'incremental', pendingBytes: ZIP_OBJECT_CHECKPOINT_PENDING_BYTES,
            pendingMilliseconds: ZIP_OBJECT_CHECKPOINT_PENDING_MILLISECONDS },
        },
        durableRanges: new VerifiedDurableRanges(ownership, revision, revision.exactSize, content.ranges, content.replica),
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
          recordSourceFailure: async fault => {
            await archive.markRevisionFailure(entryId, fault.code)
            recordMissingFile(missingFiles, entryId, request.logicalArtifactPath, fault)
          },
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
      if (!signal.aborted) recordMissingFile(missingFiles, entryId, request.logicalArtifactPath, error)
      capacityEntry.finish()
      throw error
    }
  }
}

function requirePausedZipEntry(
  paused: VerifiedDurableRanges, ownership: OutputFileOwnership, request: OutputFileRequest,
  entry: TaskEntry | undefined, checkpoint: TaskCheckpoint,
): void {
  const path = ownership.canonicalPath.join('/')
  if (!(paused instanceof VerifiedDurableRanges) || entry === undefined ||
      paused.ownership.backend !== ownership.backend ||
      paused.ownership.outputSessionId !== ownership.outputSessionId ||
      paused.ownership.ownedFileIdentity !== ownership.ownedFileIdentity ||
      paused.ownership.canonicalPath.join('/') !== path || entry.path.join('/') !== path ||
      paused.source.shareInstance !== request.source.shareInstance ||
      paused.source.fileId !== request.source.fileId || paused.fileSize !== request.expectedSize ||
      paused.availability.replica === null) {
    throw new TypeError('Paused ranges do not belong to this ZIP entry')
  }
  const committed = taskEntryContent(entry, checkpoint)
  assertContentAvailabilityAdvance(paused.availability, committed)
  // Other ZIP entries can advance the shared generation while this entry is paused.
  if (committed.ranges.length !== paused.ranges.length || committed.ranges.some((range, index) =>
    range.start !== paused.ranges[index]?.start || range.end !== paused.ranges[index]?.end)) {
    throw new TypeError('Committed ZIP entry ranges differ from the paused cut')
  }
}

function recordMissingFile(files: Map<string, MissingContentFile>, entryId: string, path: readonly string[], error: unknown): void {
  if (files.size >= MAX_CONTENT_WARNING_FILES) return
  const failure = normalizeFileTransferFailure(error)
  const fault = failure.kind === 'fault' ? failure.fault : undefined
  let reason: MissingContentFile['reason'] = 'file-failed'
  if (fault?.domain === FaultDomain.Source) {
    reason = fault.code === SourceFaultCode.RevisionChanged || fault.code === SourceFaultCode.RevisionInvalidated
      ? 'source-changed' : 'source-unavailable'
  }
  files.set(entryId, { path, reason })
}

function modifiedTime(value: CanonicalModifiedTime | undefined): { modifiedTimeMilliseconds?: bigint } {
  return value === undefined ? {} : {
    modifiedTimeMilliseconds: value.seconds * 1000n + BigInt(Math.floor(value.nanoseconds / 1_000_000)),
  }
}
async function openZipRevision(
  archive: ProgressiveZipArchive, request: OutputFileRequest, entryId: string,
  retained: TaskEntry | undefined, signal: AbortSignal,
): Promise<OpenedOutputRevision> {
  try {
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
    const failure = normalizeFileTransferFailure(error)
    if (failure.kind === 'fault' && failure.fault.domain === FaultDomain.Source &&
        await archive.findCommittedEntry(entryId) !== undefined) {
      await archive.markRevisionFailure(entryId, failure.fault.code)
    }
    throw error
  }
}
