import type { SelectionMeasure } from '../measure'

/** Separates received bytes from whole-file settlement and failure evidence. */
export class V2TransferProgressLedger {
  #writtenBytes = 0n
  #completedFiles = 0
  #completedBytes = 0n
  #failedDirectories = 0
  #fileErrors = 0
  #selectionErrors = 0

  get failedDirectories(): number { return this.#failedDirectories }

  acknowledgeWrite(bytes: bigint): void { this.#writtenBytes += bytes }

  completeFile(exactSize: bigint): void {
    this.#completedFiles += 1
    this.#completedBytes += exactSize
  }

  failDirectory(): void { this.#failedDirectories += 1 }

  recordFileError(): void { this.#fileErrors += 1 }

  recordSelectionError(): void { this.#selectionErrors += 1 }

  snapshot(measure: SelectionMeasure, outputSessionId?: string): {
    readonly measure: SelectionMeasure
    readonly writtenBytes: bigint
    readonly completedFiles: number
    readonly completedBytes: bigint
    readonly failedDirectories: number
    readonly fileErrors: number
    readonly selectionErrors: number
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
      ...(outputSessionId === undefined ? {} : { outputSessionId }),
    })
  }
}
