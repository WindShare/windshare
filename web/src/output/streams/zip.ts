import type { JobOutcome } from '../../transfer/outcome'
import {
  DirectoryAdmissionLedger,
  type DirectoryFileMutationLease,
} from '../../transfer/directory-admission-ledger'
import {
  type BeginOutputFileResult,
  type DirectoryAdmission,
  type DirectoryAdmissionScope,
  type DirectorySettlement,
  type FileRetirementDisposition,
  type JobSettlement,
  type OutputCapabilities,
  type OutputDirectoryAdmission,
  type OutputFile,
  type OutputFileTransaction,
  type OutputSession,
  type OutputSessionIdentity,
  MAXIMUM_OPEN_OUTPUT_FILES,
  VerifiedDurableRanges,
  COMPLETED_JOB_SETTLEMENT,
  needsAttentionJobSettlement,
  outputCapabilities,
  outputSessionIdentity,
  pausedJobSettlement,
  snapshotOutputFile,
} from '../../transfer/output-session'
import { FaultScope, OutputFaultCode, outputFault } from '../../transfer/fault'
import type { ZipArchiveFileEntry, ZipArchiveMember, ZipArchiveWriter } from './zip-archive'

export const ZIP_STREAM_BACKEND = 'zip-stream'

export interface ZipCompletionReport {
  readonly outcome: JobOutcome
}

export interface ZipStreamOutputOptions {
  readonly outputSessionId: string
  readonly directoryAdmissionScope: DirectoryAdmissionScope
  readonly archive: ZipArchiveWriter
  readonly reportCompletion?: (report: ZipCompletionReport) => void
}

type ZipSessionState =
  | 'open'
  | 'finishing'
  | 'finished'
  | 'finish-failed'
  | 'aborting'
  | 'aborted'

export class ZipStreamOutputSession implements OutputSession {
  readonly identity: OutputSessionIdentity
  readonly format = 'zip' as const
  readonly capabilities: OutputCapabilities = outputCapabilities({
    durability: 'None',
    randomWrite: false,
    fileFailureIsolation: false,
    modificationTime: false,
  })

  readonly #archive: ZipArchiveWriter
  readonly #reportCompletion: ZipStreamOutputOptions['reportCompletion']
  readonly #active = new Set<ZipMemberTransaction>()
  readonly #directoryAdmissions: DirectoryAdmissionLedger

  #state: ZipSessionState = 'open'
  #memberTail: Promise<void> = Promise.resolve()
  #finishPromise: Promise<void> | undefined
  #abortPromise: Promise<void> | undefined

  constructor(options: ZipStreamOutputOptions) {
    this.identity = outputSessionIdentity({
      backend: ZIP_STREAM_BACKEND,
      outputSessionId: options.outputSessionId,
    })
    this.#directoryAdmissions = new DirectoryAdmissionLedger(options.directoryAdmissionScope)
    this.#archive = options.archive
    this.#reportCompletion = options.reportCompletion
  }

  admitDirectory(input: OutputDirectoryAdmission, signal: AbortSignal): Promise<DirectoryAdmission> {
    this.#requireOpen()
    return this.#directoryAdmissions.admitDirectory(input, signal, async (directory, operationSignal) => {
      operationSignal.throwIfAborted()
      if (directory.path.length === 0) return
      const materialized = Object.freeze({
        path: directory.path,
        ...(directory.modifiedTime === undefined
          ? {}
          : { modifiedTimeMilliseconds: directory.modifiedTime.milliseconds }),
      })
      const turn = this.#reserveMemberTurn()
      await turn.ready
      operationSignal.throwIfAborted()
      try {
        this.#requireOpen()
        await this.#archive.addDirectory(materialized)
        operationSignal.throwIfAborted()
      } finally {
        turn.release()
      }
    })
  }

  finalizeDirectory(
    admission: DirectoryAdmission,
    signal: AbortSignal,
  ): Promise<DirectorySettlement> {
    this.#requireOpen()
    return this.#directoryAdmissions.finalizeDirectory(admission, signal)
  }

  async beginFile(input: OutputFile, signal: AbortSignal): Promise<BeginOutputFileResult> {
    signal.throwIfAborted()
    this.#requireOpen()
    const mutation = this.#directoryAdmissions.acquireFileMutation(input)
    const admitted = mutation.file
    const file = snapshotOutputFile({
      source: admitted.source,
      path: admitted.path,
      exactSize: admitted.exactSize,
      ...(admitted.parentAdmission === undefined ? {} : { parentAdmission: admitted.parentAdmission }),
      ...(admitted.modifiedTime === undefined ? {} : { modifiedTime: admitted.modifiedTime }),
    })
    try {
      if (this.#active.size >= MAXIMUM_OPEN_OUTPUT_FILES) {
        throw new RangeError('ZIP output has reached its open member limit')
      }
      const turn = this.#reserveMemberTurn()
      const transaction = new ZipMemberTransaction(this, this.#archive, file, turn, mutation)
      this.#active.add(transaction)
      const ownership = Object.freeze({
        ...this.identity,
        canonicalPath: file.path,
        ownedFileIdentity: `${this.identity.outputSessionId}:${pathKey(file.path)}`,
      })
      return Object.freeze({
        transaction,
        durableRanges: new VerifiedDurableRanges(ownership, file.source, file.exactSize, []),
      })
    } catch (error) {
      mutation.release()
      throw error
    }
  }

  async completeJob(outcome: JobOutcome, signal: AbortSignal): Promise<JobSettlement> {
    this.#requireOpen()
    signal.throwIfAborted()
    if (this.#active.size !== 0) {
      throw new Error('Cannot finish ZIP output while members are active')
    }
    this.#state = 'finishing'
    const operation = this.#finish(outcome, signal)
    this.#finishPromise = operation
    try {
      await operation
    } catch (error) {
      if (this.#state === 'finishing') this.#state = 'finish-failed'
      throw error
    }
    return COMPLETED_JOB_SETTLEMENT
  }

  async pauseJob(reason: unknown): Promise<JobSettlement> {
    if (this.#state === 'finished') return COMPLETED_JOB_SETTLEMENT
    try {
      if (this.#abortPromise === undefined) {
        this.#state = 'aborting'
        this.#abortPromise = this.#abort(reason).then(() => {
          if (this.#state !== 'finished') this.#state = 'aborted'
        })
      }
      await this.#abortPromise
      return this.#publicationFinished()
        ? COMPLETED_JOB_SETTLEMENT
        : pausedJobSettlement(this.capabilities.durability)
    } catch {
      return needsAttentionJobSettlement(outputFault(
        FaultScope.OutputPause,
        OutputFaultCode.MutationAmbiguous,
      ))
    }
  }

  memberSettled(transaction: ZipMemberTransaction): void {
    this.#active.delete(transaction)
  }

  requireOpen(): void {
    this.#requireOpen()
  }

  async pauseAfterIrreversibleMember(reason: unknown): Promise<void> {
    await this.pauseJob(reason)
  }

  async #finish(outcome: JobOutcome, signal: AbortSignal): Promise<void> {
    await this.#memberTail
    signal.throwIfAborted()
    await this.#archive.close(signal)
    // Successful stream close is the irreversible publication boundary. A late
    // abort may still converge cleanup but cannot rewrite the canonical outcome.
    this.#state = 'finished'
    try {
      this.#reportCompletion?.(Object.freeze({ outcome }))
    } catch {
      // Completion reporting is an observer, not publication authority. A UI
      // callback cannot revoke an archive that the browser already committed.
    }
  }

  async #abort(reason: unknown): Promise<void> {
    await this.#archive.abort(reason)
    try {
      await this.#finishPromise
    } catch {
      // Archive abort already awaited the shared close/cleanup settlement.
    }
  }

  #reserveMemberTurn(): MemberTurn {
    const ready = this.#memberTail
    let release!: () => void
    this.#memberTail = new Promise<void>((resolve) => {
      release = resolve
    })
    return { ready, release: once(release) }
  }

  #publicationFinished(): boolean {
    return this.#state === 'finished'
  }

  #requireOpen(): void {
    if (this.#state !== 'open') throw new Error('ZIP output session is not open')
  }
}

interface MemberTurn {
  readonly ready: Promise<void>
  readonly release: () => void
}

class ZipMemberTransaction implements OutputFileTransaction {
  readonly #session: ZipStreamOutputSession
  readonly #archive: ZipArchiveWriter
  readonly #file: OutputFile
  readonly #turn: MemberTurn
  readonly #directoryMutation: DirectoryFileMutationLease
  readonly #ownership: ConstructorParameters<typeof VerifiedDurableRanges>[0]

  #operationTail: Promise<unknown> = Promise.resolve()
  #member: ZipArchiveMember | undefined
  #nextOffset = 0n
  #started = false
  #settled = false

  constructor(
    session: ZipStreamOutputSession,
    archive: ZipArchiveWriter,
    file: OutputFile,
    turn: MemberTurn,
    directoryMutation: DirectoryFileMutationLease,
  ) {
    this.#session = session
    this.#archive = archive
    this.#file = file
    this.#turn = turn
    this.#directoryMutation = directoryMutation
    this.#ownership = Object.freeze({
      ...session.identity,
      canonicalPath: file.path,
      ownedFileIdentity: `${session.identity.outputSessionId}:${pathKey(file.path)}`,
    })
  }

  writeRange(offset: bigint, data: Uint8Array, signal: AbortSignal): Promise<void> {
    const snapshot = data.slice()
    return this.#enqueue(async () => {
      signal.throwIfAborted()
      this.#requireActive()
      if (offset !== this.#nextOffset || offset + BigInt(snapshot.byteLength) > this.#file.exactSize) {
        throw new RangeError('ZIP member requires contiguous ascending ranges')
      }
      if (snapshot.byteLength === 0) return
      const member = await this.#startMember()
      signal.throwIfAborted()
      await member.write(snapshot)
      signal.throwIfAborted()
      this.#nextOffset += BigInt(snapshot.byteLength)
    })
  }

  checkpoint(signal: AbortSignal): Promise<VerifiedDurableRanges> {
    return this.#enqueue(async () => {
      signal.throwIfAborted()
      this.#requireActive()
      return new VerifiedDurableRanges(
        this.#ownership,
        this.#file.source,
        this.#file.exactSize,
        [],
      )
    })
  }

  commit(signal: AbortSignal): Promise<void> {
    return this.#enqueue(async () => {
      signal.throwIfAborted()
      this.#requireActive()
      if (this.#nextOffset !== this.#file.exactSize) throw new Error('ZIP member is incomplete')
      const member = await this.#startMember()
      signal.throwIfAborted()
      await member.close()
      signal.throwIfAborted()
      this.#settle()
    })
  }

  retire(reason: unknown): Promise<FileRetirementDisposition> {
    return this.#enqueue(async () => {
      if (this.#settled) return this.#started ? 'JobOutputCompromised' : 'FileIsolated'
      if (!this.#started) {
        this.#settle()
        return 'FileIsolated'
      }
      try {
        await this.#member?.abort(reason)
      } finally {
        try {
          await this.#session.pauseAfterIrreversibleMember(reason)
        } finally {
          this.#settle()
        }
      }
      return 'JobOutputCompromised'
    })
  }

  pause(reason: unknown): Promise<void> {
    return this.#enqueue(async () => {
      if (this.#settled) return
      try {
        await this.#member?.abort(reason)
        if (this.#started) await this.#session.pauseAfterIrreversibleMember(reason)
      } finally {
        this.#settle()
      }
    })
  }

  async #startMember(): Promise<ZipArchiveMember> {
    if (this.#member !== undefined) return this.#member
    await this.#turn.ready
    this.#session.requireOpen()
    // Creating a member may emit its local header before rejecting.
    this.#started = true
    this.#member = await this.#archive.beginFile(zipArchiveFile(this.#file))
    return this.#member
  }

  #settle(): void {
    if (this.#settled) return
    this.#settled = true
    this.#turn.release()
    this.#session.memberSettled(this)
    this.#directoryMutation.release()
  }

  #enqueue<T>(operation: () => Promise<T>): Promise<T> {
    const result = this.#operationTail.then(operation, operation)
    this.#operationTail = result
    return result
  }

  #requireActive(): void {
    if (this.#settled) throw new Error('ZIP member transaction is settled')
  }
}

function zipArchiveFile(file: OutputFile): ZipArchiveFileEntry {
  return Object.freeze({
    path: file.path,
    exactSize: file.exactSize,
    ...(file.modifiedTime === undefined
      ? {}
      : { modifiedTimeMilliseconds: file.modifiedTime.milliseconds }),
  })
}

function once(action: () => void): () => void {
  let called = false
  return () => {
    if (called) return
    called = true
    action()
  }
}

function pathKey(path: readonly string[]): string {
  return path.map((segment) => encodeURIComponent(segment)).join('/')
}
