export interface RingBufferCapacity {
  readonly maxCount: number
  readonly maxEventBytes: number
  readonly maxTotalBytes: number
}

interface RingSlot<T> {
  readonly item: T
  readonly bytes: number
}

/**
 * Fixed-capacity circular buffer bounded by both entry count and byte size.
 * Push and eviction run in O(1) without shifting underlying arrays.
 */
export class DiagnosticRingBuffer<T> {
  readonly #slots: (RingSlot<T> | undefined)[]
  readonly #maxCount: number
  readonly #maxEventBytes: number
  readonly #maxTotalBytes: number
  #head = 0
  #size = 0
  #bytes = 0
  #overwritten = 0
  #dropped = 0

  constructor(capacity: RingBufferCapacity) {
    requirePositiveSafeInt(capacity.maxCount, 'ring buffer maxCount')
    requirePositiveSafeInt(capacity.maxEventBytes, 'ring buffer maxEventBytes')
    requirePositiveSafeInt(capacity.maxTotalBytes, 'ring buffer maxTotalBytes')
    if (capacity.maxEventBytes > capacity.maxTotalBytes) {
      throw new RangeError('ring buffer maxEventBytes must not exceed maxTotalBytes')
    }
    this.#maxCount = capacity.maxCount
    this.#maxEventBytes = capacity.maxEventBytes
    this.#maxTotalBytes = capacity.maxTotalBytes
    this.#slots = new Array<RingSlot<T> | undefined>(capacity.maxCount)
  }

  get size(): number { return this.#size }
  get bytes(): number { return this.#bytes }
  get overwritten(): number { return this.#overwritten }
  get dropped(): number { return this.#dropped }

  push(item: T, bytes: number): boolean {
    if (!Number.isSafeInteger(bytes) || bytes <= 0 || bytes > this.#maxEventBytes) {
      this.#dropped += 1
      return false
    }
    while (
      this.#size >= this.#maxCount ||
      (this.#size > 0 && this.#bytes + bytes > this.#maxTotalBytes)
    ) {
      this.#evictOldest()
    }
    const tail = (this.#head + this.#size) % this.#maxCount
    this.#slots[tail] = { item, bytes }
    this.#size += 1
    this.#bytes += bytes
    return true
  }

  last(): T | undefined {
    if (this.#size === 0) return undefined
    const index = (this.#head + this.#size - 1) % this.#maxCount
    return this.#slots[index]?.item
  }

  entries(): readonly T[] {
    const result: T[] = []
    for (let offset = 0; offset < this.#size; offset += 1) {
      const slot = this.#slots[(this.#head + offset) % this.#maxCount]
      if (slot !== undefined) result.push(slot.item)
    }
    return result
  }

  clear(): void {
    for (let offset = 0; offset < this.#size; offset += 1) {
      this.#slots[(this.#head + offset) % this.#maxCount] = undefined
    }
    this.#head = 0
    this.#size = 0
    this.#bytes = 0
  }

  #evictOldest(): void {
    const oldest = this.#slots[this.#head]
    this.#slots[this.#head] = undefined
    this.#head = (this.#head + 1) % this.#maxCount
    this.#size -= 1
    if (oldest !== undefined) this.#bytes -= oldest.bytes
    this.#overwritten += 1
  }
}

function requirePositiveSafeInt(value: number, label: string): void {
  if (!Number.isSafeInteger(value) || value <= 0) {
    throw new RangeError(`${label} must be a positive safe integer`)
  }
}
