import type { SelectionMeasure } from '../measure'
import type { V2RevisionCapacityWaitSnapshot } from '../revision-capacity/public'

/** Separates received bytes from whole-file settlement and failure evidence. */
export class V2TransferProgressLedger {
  #writtenBytes = 0n
  #phase: 'receiving' | 'finishing' = 'receiving'
  #activeMaterializedBytes = 0n
  readonly #materializingFiles = new Map<string, bigint>()
  #recoverableBytes = 0n
  #completedFiles = 0
  #completedBytes = 0n
  #failedDirectories = 0
  #fileErrors = 0
  #selectionErrors = 0
  #capacityWaitingFiles = 0
  #capacityAccumulatedWaitMilliseconds = 0
  #capacityWaitAttempts = 0
  #capacityWaitVisible = false

  get failedDirectories(): number { return this.#failedDirectories }
  get completedFiles(): number { return this.#completedFiles }
  get completedBytes(): bigint { return this.#completedBytes }
  get writtenBytes(): bigint { return this.#writtenBytes }
  get recoverableBytes(): bigint { return this.#recoverableBytes }

  acknowledgeWrite(bytes: bigint): void { this.#writtenBytes += bytes }

  acknowledgeRecoverable(bytes: bigint): void { this.#recoverableBytes += bytes }

  beginFinishing(): void { this.#phase = 'finishing' }

  // Absolute per-file coverage replaces a retried transaction's observation;
  // completed files leave this bounded active-worker map and count exactly once.
  observeMaterializedFile(fileId: string, bytes: bigint): void {
    this.#activeMaterializedBytes += bytes - (this.#materializingFiles.get(fileId) ?? 0n)
    if (bytes === 0n) this.#materializingFiles.delete(fileId)
    else this.#materializingFiles.set(fileId, bytes)
  }

  completeFile(exactSize: bigint, fileId?: string): void {
    if (fileId !== undefined) this.observeMaterializedFile(fileId, 0n)
    this.#completedFiles += 1
    this.#completedBytes += exactSize
  }

  failDirectory(): void { this.#failedDirectories += 1 }

  recordFileError(): void { this.#fileErrors += 1 }

  recordSelectionError(): void { this.#selectionErrors += 1 }

  observeCapacityWait(snapshot: V2RevisionCapacityWaitSnapshot): void {
    this.#capacityWaitingFiles = snapshot.activeWaiters
    this.#capacityAccumulatedWaitMilliseconds = snapshot.accumulatedWaitMilliseconds
    this.#capacityWaitAttempts = snapshot.attempts
    this.#capacityWaitVisible = snapshot.visible
  }

  snapshot(measure: SelectionMeasure, outputSessionId?: string): {
    readonly measure: SelectionMeasure
    readonly phase: 'receiving' | 'finishing'
    readonly materializedBytes: bigint
    readonly writtenBytes: bigint
    readonly recoverableBytes: bigint
    readonly completedFiles: number
    readonly completedBytes: bigint
    readonly failedDirectories: number
    readonly fileErrors: number
    readonly selectionErrors: number
    readonly capacityWaitingFiles: number
    readonly capacityAccumulatedWaitMilliseconds: number
    readonly capacityWaitAttempts: number
    readonly capacityWaitVisible: boolean
    readonly outputSessionId?: string
  } {
    return Object.freeze({
      measure,
      phase: this.#phase,
      materializedBytes: this.#completedBytes + this.#activeMaterializedBytes,
      writtenBytes: this.#writtenBytes,
      recoverableBytes: this.#recoverableBytes,
      completedFiles: this.#completedFiles,
      completedBytes: this.#completedBytes,
      failedDirectories: this.#failedDirectories,
      fileErrors: this.#fileErrors,
      selectionErrors: this.#selectionErrors,
      capacityWaitingFiles: this.#capacityWaitingFiles,
      capacityAccumulatedWaitMilliseconds: this.#capacityAccumulatedWaitMilliseconds,
      capacityWaitAttempts: this.#capacityWaitAttempts,
      capacityWaitVisible: this.#capacityWaitVisible,
      ...(outputSessionId === undefined ? {} : { outputSessionId }),
    })
  }
}
