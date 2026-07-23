import type { JobOutcome } from '../../transfer/outcome'
import {
  type BeginOutputFileResult,
  type FileAbortDisposition,
  type OutputCapabilities,
  type OutputDirectory,
  type OutputFile,
  type OutputFileTransaction,
  type OutputSession,
  type OutputSessionIdentity,
  MAXIMUM_OPEN_OUTPUT_FILES,
  VerifiedDurableRanges,
  outputCapabilities,
  outputSessionIdentity,
  snapshotOutputDirectory,
  snapshotOutputFile,
} from '../../transfer/output-session'
import type { ZipArchiveMember, ZipArchiveWriter } from './zip-archive'

export const ZIP_STREAM_BACKEND = 'zip-stream'

export interface ZipCompletionReport {
  readonly outcome: JobOutcome
}

export interface ZipStreamOutputOptions {
  readonly outputSessionId: string
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
  readonly capabilities: OutputCapabilities = outputCapabilities({
    durability: 'None',
    randomWrite: false,
    fileFailureIsolation: false,
    modificationTime: false,
  })

  readonly #archive: ZipArchiveWriter
  readonly #reportCompletion: ZipStreamOutputOptions['reportCompletion']
  readonly #active = new Set<ZipMemberTransaction>()

  #state: ZipSessionState = 'open'
  #memberTail: Promise<void> = Promise.resolve()
  #finishPromise: Promise<void> | undefined
  #abortPromise: Promise<void> | undefined

  constructor(options: ZipStreamOutputOptions) {
    this.identity = outputSessionIdentity({
      backend: ZIP_STREAM_BACKEND,
      outputSessionId: options.outputSessionId,
    })
    this.#archive = options.archive
    this.#reportCompletion = options.reportCompletion
  }

  async ensureDirectory(input: OutputDirectory): Promise<void> {
    this.#requireOpen()
    const directory = snapshotOutputDirectory({ path: input.path })
    const turn = this.#reserveMemberTurn()
    await turn.ready
    try {
      this.#requireOpen()
      await this.#archive.addDirectory(directory)
    } finally {
      turn.release()
    }
  }

  async finalizeDirectory(input: OutputDirectory, signal: AbortSignal): Promise<void> {
    this.#requireOpen()
    signal.throwIfAborted()
    // Catalog authentication owns path uniqueness and traversal order. Retaining
    // every directory here would duplicate that authority solely for validation.
    snapshotOutputDirectory({ path: input.path })
  }

  async beginFile(input: OutputFile): Promise<BeginOutputFileResult> {
    this.#requireOpen()
    if (this.#active.size >= MAXIMUM_OPEN_OUTPUT_FILES) {
      throw new RangeError('ZIP output has reached its open member limit')
    }
    const file = snapshotOutputFile({
      source: input.source,
      path: input.path,
      exactSize: input.exactSize,
    })
    const turn = this.#reserveMemberTurn()
    const transaction = new ZipMemberTransaction(this, this.#archive, file, turn)
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
  }

  async finishJob(outcome: JobOutcome, signal: AbortSignal): Promise<void> {
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
  }

  abortJob(reason: unknown): Promise<void> {
    if (this.#state === 'finished') return Promise.resolve()
    if (this.#abortPromise !== undefined) return this.#abortPromise
    this.#state = 'aborting'
    const operation = this.#abort(reason).then(() => {
      if (this.#state !== 'finished') this.#state = 'aborted'
    })
    this.#abortPromise = operation
    return operation
  }

  memberSettled(transaction: ZipMemberTransaction): void {
    this.#active.delete(transaction)
  }

  requireOpen(): void {
    this.#requireOpen()
  }

  async compromise(reason: unknown): Promise<void> {
    await this.abortJob(reason)
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
  ) {
    this.#session = session
    this.#archive = archive
    this.#file = file
    this.#turn = turn
    this.#ownership = Object.freeze({
      ...session.identity,
      canonicalPath: file.path,
      ownedFileIdentity: `${session.identity.outputSessionId}:${pathKey(file.path)}`,
    })
  }

  writeRange(offset: bigint, data: Uint8Array): Promise<void> {
    const snapshot = data.slice()
    return this.#enqueue(async () => {
      this.#requireActive()
      if (offset !== this.#nextOffset || offset + BigInt(snapshot.byteLength) > this.#file.exactSize) {
        throw new RangeError('ZIP member requires contiguous ascending ranges')
      }
      if (snapshot.byteLength === 0) return
      const member = await this.#startMember()
      await member.write(snapshot)
      this.#nextOffset += BigInt(snapshot.byteLength)
    })
  }

  checkpoint(): Promise<VerifiedDurableRanges> {
    return this.#enqueue(async () => {
      this.#requireActive()
      return new VerifiedDurableRanges(
        this.#ownership,
        this.#file.source,
        this.#file.exactSize,
        [],
      )
    })
  }

  commit(): Promise<void> {
    return this.#enqueue(async () => {
      this.#requireActive()
      if (this.#nextOffset !== this.#file.exactSize) throw new Error('ZIP member is incomplete')
      const member = await this.#startMember()
      await member.close()
      this.#settle()
    })
  }

  abort(reason: unknown): Promise<FileAbortDisposition> {
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
          await this.#session.compromise(reason)
        } finally {
          this.#settle()
        }
      }
      return 'JobOutputCompromised'
    })
  }

  async #startMember(): Promise<ZipArchiveMember> {
    if (this.#member !== undefined) return this.#member
    await this.#turn.ready
    this.#session.requireOpen()
    // Creating a member may emit its local header before rejecting.
    this.#started = true
    this.#member = await this.#archive.beginFile(this.#file)
    return this.#member
  }

  #settle(): void {
    if (this.#settled) return
    this.#settled = true
    this.#turn.release()
    this.#session.memberSettled(this)
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
