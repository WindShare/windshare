export interface TransferProgress {
  readonly phase: 'receiving' | 'finishing'
  readonly discovery: 'open' | 'complete' | 'failed'
  readonly discoveredFiles: number
  readonly discoveredBytes: bigint
  readonly receivedObjectBytes: bigint
  readonly materializedBytes: bigint
  readonly writtenBytes: bigint
  readonly completedFiles: number
  readonly completedBytes: bigint
  readonly fileErrors: number
  readonly selectionErrors: number
  readonly failedDirectories: number
  readonly sourceOmittedEntries: bigint
  readonly contentLanes: number
  readonly capacityWaitingFiles: number
  readonly capacityAccumulatedWaitMilliseconds: number
  readonly capacityWaitAttempts: number
  readonly capacityWaitVisible: boolean
  readonly bandwidthWaitVisible: boolean
  readonly transferJobId: string
  readonly outputSessionId?: string
}

export const EMPTY_TRANSFER_PROGRESS: TransferProgress = Object.freeze({
  phase: 'receiving',
  discovery: 'open',
  discoveredFiles: 0,
  discoveredBytes: 0n,
  receivedObjectBytes: 0n,
  materializedBytes: 0n,
  writtenBytes: 0n,
  completedFiles: 0,
  completedBytes: 0n,
  fileErrors: 0,
  selectionErrors: 0,
  failedDirectories: 0,
  sourceOmittedEntries: 0n,
  contentLanes: 0,
  capacityWaitingFiles: 0,
  capacityAccumulatedWaitMilliseconds: 0,
  capacityWaitAttempts: 0,
  capacityWaitVisible: false,
  bandwidthWaitVisible: false,
  transferJobId: '',
})

export class TransferProgressTracker {
  readonly #jobId: string
  readonly #outputSessionId: string | undefined
  readonly #onChange: (progress: TransferProgress) => void
  #phase: 'receiving' | 'finishing' = 'receiving'
  #discovery: 'open' | 'complete' | 'failed' = 'open'
  #discoveredFiles = 0
  #discoveredBytes = 0n
  #receivedObjectBytes = 0n
  #materializedBytes = 0n
  #writtenBytes = 0n
  #completedFiles = 0
  #completedBytes = 0n
  #fileErrors = 0
  #selectionErrors = 0
  #failedDirectories = 0
  #sourceOmittedEntries = 0n
  #contentLanes = 0
  #capacityWaitingFiles = 0
  #capacityAccumulatedWaitMilliseconds = 0
  #capacityWaitAttempts = 0
  #capacityWaitVisible = false
  #bandwidthWaitVisible = false

  constructor(
    jobId: string,
    onChange: (progress: TransferProgress) => void,
    outputSessionId?: string,
  ) {
    this.#jobId = jobId
    this.#outputSessionId = outputSessionId
    this.#onChange = onChange
  }

  observeFileDiscovered(size: bigint): void {
    this.#discoveredFiles += 1
    this.#discoveredBytes += size
    this.#emit()
  }

  observeOmittedEntries(count: bigint): void {
    if (count <= 0n) return
    this.#sourceOmittedEntries += count
    this.#emit()
  }

  observeDiscoverySettled(state: 'complete' | 'failed'): void {
    this.#discovery = state
    this.#emit()
  }

  observeDirectoryFailed(): void {
    this.#failedDirectories += 1
    this.#emit()
  }

  observeInitialCoverage(bytes: bigint): void {
    if (bytes <= 0n) return
    this.#materializedBytes += bytes
    this.#emit()
  }

  observeObjectBytes(bytes: number): void {
    if (bytes <= 0) return
    this.#receivedObjectBytes += BigInt(bytes)
    this.#emit()
  }

  observeWritten(bytes: bigint): void {
    if (bytes <= 0n) return
    this.#writtenBytes += bytes
    this.#materializedBytes += bytes
    this.#emit()
  }

  observeFileCompleted(size: bigint): void {
    this.#completedFiles += 1
    this.#completedBytes += size
    this.#emit()
  }

  observeFileFailed(): void {
    this.#fileErrors += 1
    this.#emit()
  }

  observeSelectionError(): void {
    this.#selectionErrors += 1
    this.#emit()
  }

  observePhase(phase: 'receiving' | 'finishing'): void {
    if (this.#phase === phase) return
    this.#phase = phase
    this.#emit()
  }

  observeContentLanes(count: number): void {
    if (this.#contentLanes === count) return
    this.#contentLanes = count
    this.#emit()
  }

  observeBandwidthWait(waiting: boolean): void {
    if (this.#bandwidthWaitVisible === waiting) return
    this.#bandwidthWaitVisible = waiting
    this.#emit()
  }

  observeCapacityWait(deltaWaiting: number, addedWaitMs: number, addedAttempt: number): void {
    this.#capacityWaitingFiles = Math.max(0, this.#capacityWaitingFiles + deltaWaiting)
    this.#capacityAccumulatedWaitMilliseconds += addedWaitMs
    this.#capacityWaitAttempts += addedAttempt
    this.#capacityWaitVisible = this.#capacityWaitingFiles > 0
    this.#emit()
  }

  snapshot(): TransferProgress {
    return Object.freeze({
      phase: this.#phase,
      discovery: this.#discovery,
      discoveredFiles: this.#discoveredFiles,
      discoveredBytes: this.#discoveredBytes,
      receivedObjectBytes: this.#receivedObjectBytes,
      materializedBytes: this.#materializedBytes,
      writtenBytes: this.#writtenBytes,
      completedFiles: this.#completedFiles,
      completedBytes: this.#completedBytes,
      fileErrors: this.#fileErrors,
      selectionErrors: this.#selectionErrors,
      failedDirectories: this.#failedDirectories,
      sourceOmittedEntries: this.#sourceOmittedEntries,
      contentLanes: this.#contentLanes,
      capacityWaitingFiles: this.#capacityWaitingFiles,
      capacityAccumulatedWaitMilliseconds: this.#capacityAccumulatedWaitMilliseconds,
      capacityWaitAttempts: this.#capacityWaitAttempts,
      capacityWaitVisible: this.#capacityWaitVisible,
      bandwidthWaitVisible: this.#bandwidthWaitVisible,
      transferJobId: this.#jobId,
      ...(this.#outputSessionId === undefined ? {} : { outputSessionId: this.#outputSessionId }),
    })
  }

  #emit(): void {
    this.#onChange(this.snapshot())
  }
}
