import {
  V2OutputPausedError,
  type PendingFile,
} from './contract'

const PENDING_FILE_STRUCTURAL_METADATA_BYTES = 128n
const PENDING_FILE_EXACT_SIZE_BYTES = 8n
const PENDING_FILE_TIMESTAMP_METADATA_BYTES = 24n
const PENDING_PATH_SEGMENT_METADATA_BYTES = 8n
const UTF8_ENCODER = new TextEncoder()

export function pendingFileMetadataBytes(file: PendingFile): bigint {
  const admission = file.parent.admission
  const strings = [
    file.entry.idText,
    file.entry.name,
    ...file.sourcePath,
    ...file.artifactPath,
    file.parent.directoryId,
    file.parent.generation,
    ...file.parent.sourcePath,
    ...file.parent.artifactPath,
    ...(admission === undefined ? [] : [
      admission.token,
      ...(admission.parentToken === undefined ? [] : [admission.parentToken]),
    ]),
  ]
  let bytes = PENDING_FILE_STRUCTURAL_METADATA_BYTES +
    PENDING_FILE_EXACT_SIZE_BYTES +
    BigInt(file.entry.id.byteLength) +
    BigInt(strings.length) * PENDING_PATH_SEGMENT_METADATA_BYTES
  for (const value of strings) bytes += BigInt(UTF8_ENCODER.encode(value).byteLength)
  if (file.modifiedTime !== undefined) bytes += PENDING_FILE_TIMESTAMP_METADATA_BYTES
  if (file.parent.modifiedTime !== undefined) bytes += PENDING_FILE_TIMESTAMP_METADATA_BYTES
  return bytes
}

interface WeightedQueueItem<T> {
  readonly value: T
  readonly weight: bigint
}

/** Bounded async ownership queue shared by catalog and file schedulers. */
export class AsyncBoundedQueue<T> {
  readonly #maximumItems: number
  readonly #maximumBytes: bigint
  readonly #weight: (item: T) => bigint
  readonly #closeWhenIdle: boolean
  readonly #items: WeightedQueueItem<T>[] = []
  #bytes = 0n
  #closed = false
  #failure: unknown
  #unfinished = 0
  readonly #waiters = new Set<() => void>()

  constructor(
    maximumItems: number,
    maximumBytes: bigint,
    weight: (item: T) => bigint = () => 1n,
    closeWhenIdle = false,
  ) {
    this.#maximumItems = maximumItems
    this.#maximumBytes = maximumBytes
    this.#weight = weight
    this.#closeWhenIdle = closeWhenIdle
  }

  async push(item: T, signal: AbortSignal): Promise<void> {
    const weight = this.#weight(item)
    this.#validateWeight(weight)
    while (!this.#closed && !this.#canAdmit(weight)) await this.#wait(signal)
    signal.throwIfAborted()
    if (this.#closed) throw this.#failure ?? new V2OutputPausedError('Transfer queue is closed')
    this.#admit(item, weight)
  }

  tryPush(item: T, signal: AbortSignal): boolean {
    signal.throwIfAborted()
    if (this.#closed) throw this.#failure ?? new V2OutputPausedError('Transfer queue is closed')
    const weight = this.#weight(item)
    this.#validateWeight(weight)
    if (!this.#canAdmit(weight)) return false
    this.#admit(item, weight)
    return true
  }

  async pop(signal: AbortSignal): Promise<T | undefined> {
    while (this.#items.length === 0 && !this.#closed) await this.#wait(signal)
    signal.throwIfAborted()
    const item = this.#items.shift()
    if (item !== undefined) {
      this.#bytes -= item.weight
      this.#wake()
    }
    return item?.value
  }

  close(): void {
    this.#closed = true
    this.#wake()
  }

  taskDone(): void {
    if (!this.#closeWhenIdle || this.#unfinished === 0) return
    this.#unfinished -= 1
    if (this.#unfinished === 0 && this.#items.length === 0) this.close()
  }

  abort(reason: unknown): void {
    this.#failure = reason
    this.#closed = true
    this.#wake()
  }

  #admit(item: T, weight: bigint): void {
    this.#items.push({ value: item, weight })
    this.#bytes += weight
    if (this.#closeWhenIdle) this.#unfinished += 1
    this.#wake()
  }

  #canAdmit(weight: bigint): boolean {
    return this.#items.length < this.#maximumItems && this.#bytes + weight <= this.#maximumBytes
  }

  #validateWeight(weight: bigint): void {
    if (weight < 0n || weight > this.#maximumBytes) {
      throw new V2OutputPausedError('Transfer queue item exceeds its byte admission')
    }
  }

  async #wait(signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    await new Promise<void>((resolve, reject) => {
      const wake = () => {
        signal.removeEventListener('abort', abort)
        this.#waiters.delete(wake)
        resolve()
      }
      const abort = () => {
        signal.removeEventListener('abort', abort)
        this.#waiters.delete(wake)
        reject(signal.reason ?? new DOMException('Transfer queue aborted', 'AbortError'))
      }
      this.#waiters.add(wake)
      signal.addEventListener('abort', abort, { once: true })
      if (signal.aborted) abort()
    })
  }

  #wake(): void {
    for (const waiter of [...this.#waiters]) waiter()
  }
}
