import type { SelectionMeasure } from '../measure'
import type { V2RevisionCapacityWaitSnapshot } from '../revision-capacity/public'

/** Separates received bytes from whole-file settlement and failure evidence. */
export class V2TransferProgressLedger {
  #writtenBytes = 0n
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

  acknowledgeWrite(bytes: bigint): void { this.#writtenBytes += bytes }

  completeFile(exactSize: bigint): void {
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
    readonly writtenBytes: bigint
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
      writtenBytes: this.#writtenBytes,
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
