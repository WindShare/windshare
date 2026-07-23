export const SMALL_TRANSFER_FILE_LIMIT = 30
export const SMALL_TRANSFER_BYTE_LIMIT = 8n * 1024n * 1024n

export type SelectionSizeClass = 'small' | 'large' | 'unknown'
export type DiscoveryTerminal = 'open' | 'complete' | 'failed'

export interface SelectionMeasure {
  readonly discoveredFiles: number
  readonly discoveredBytes: bigint
  readonly discovery: DiscoveryTerminal
  readonly sizeClass: SelectionSizeClass
}

export class SelectionMeasureError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'SelectionMeasureError'
  }
}

/**
 * Monotonic streaming measurement over unique selected files. Global catalog
 * NodeID authority is the deduplication boundary, so this layer retains no IDs.
 */
export class SelectionMeasureTracker {
  #files = 0
  #bytes = 0n
  #reachedLargeBoundary = false
  #terminal: DiscoveryTerminal = 'open'

  observeUniqueFile(exactSize: bigint): SelectionMeasure {
    if (this.#terminal !== 'open') {
      throw new SelectionMeasureError('cannot discover a file after discovery became terminal')
    }
    if (exactSize < 0n) {
      throw new SelectionMeasureError('discovered file size must not be negative')
    }
    if (this.#files === Number.MAX_SAFE_INTEGER) {
      throw new SelectionMeasureError('discovered file count exceeds exact integer representation')
    }
    this.#files += 1
    this.#bytes += exactSize
    this.#reachedLargeBoundary ||= this.#files >= SMALL_TRANSFER_FILE_LIMIT ||
      this.#bytes >= SMALL_TRANSFER_BYTE_LIMIT
    return this.snapshot()
  }

  complete(): SelectionMeasure {
    return this.#finish('complete')
  }

  fail(): SelectionMeasure {
    return this.#finish('failed')
  }

  snapshot(): SelectionMeasure {
    let sizeClass: SelectionSizeClass = 'unknown'
    if (this.#reachedLargeBoundary) {
      sizeClass = 'large'
    } else if (this.#terminal === 'complete') {
      sizeClass = 'small'
    }
    return Object.freeze({
      discoveredFiles: this.#files,
      discoveredBytes: this.#bytes,
      discovery: this.#terminal,
      sizeClass,
    })
  }

  #finish(terminal: Exclude<DiscoveryTerminal, 'open'>): SelectionMeasure {
    if (this.#terminal !== 'open' && this.#terminal !== terminal) {
      throw new SelectionMeasureError('discovery terminal state cannot change')
    }
    this.#terminal = terminal
    return this.snapshot()
  }
}
