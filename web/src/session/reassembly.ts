import type { BlockFrame } from './frame'

export type ReassemblyViolationKind =
  | 'duplicate-sequence'
  | 'second-final'
  | 'past-final'
  | 'oversize'
  | 'already-complete'

export class ReassemblyViolation extends Error {
  readonly kind: ReassemblyViolationKind

  constructor(kind: ReassemblyViolationKind, message: string) {
    super(message)
    this.name = 'ReassemblyViolation'
    this.kind = kind
  }
}

/** Collects one request attempt; ciphertext from separate attempts must never mix. */
export class BlockReassembler {
  readonly #maxBlockBytes: number
  readonly #frames = new Map<number, Uint8Array>()
  #finalSequence: number | undefined
  #totalBytes = 0
  #complete = false

  constructor(maxBlockBytes: number) {
    if (!Number.isSafeInteger(maxBlockBytes) || maxBlockBytes <= 0) {
      throw new RangeError('maximum block bytes must be a positive safe integer')
    }
    this.#maxBlockBytes = maxBlockBytes
  }

  get bufferedBytes(): number {
    return this.#totalBytes
  }

  get bufferedFrames(): number {
    return this.#frames.size
  }

  add(frame: BlockFrame): Uint8Array | undefined {
    if (this.#complete) {
      throw new ReassemblyViolation(
        'already-complete',
        `block ${frame.index} received a frame after completion`,
      )
    }
    if (this.#frames.has(frame.sequence)) {
      throw new ReassemblyViolation(
        'duplicate-sequence',
        `block ${frame.index} has duplicate sequence ${frame.sequence}`,
      )
    }
    // Every sequence contributes a non-empty payload. A sequence at or beyond
    // the byte budget can therefore never become a complete ciphertext.
    if (frame.sequence >= this.#maxBlockBytes) {
      throw new ReassemblyViolation(
        'oversize',
        `block ${frame.index} sequence ${frame.sequence} cannot fit the ${this.#maxBlockBytes}-byte reassembly limit`,
      )
    }
    if (
      this.#finalSequence !== undefined &&
      frame.sequence > this.#finalSequence
    ) {
      throw new ReassemblyViolation(
        'past-final',
        `block ${frame.index} sequence ${frame.sequence} exceeds final sequence ${this.#finalSequence}`,
      )
    }
    this.#acceptFinal(frame)

    const nextTotal = this.#totalBytes + frame.payload.byteLength
    if (!Number.isSafeInteger(nextTotal) || nextTotal > this.#maxBlockBytes) {
      throw new ReassemblyViolation(
        'oversize',
        `block ${frame.index} exceeds the ${this.#maxBlockBytes}-byte reassembly limit`,
      )
    }
    this.#totalBytes = nextTotal
    this.#frames.set(frame.sequence, frame.payload.slice())

    if (
      this.#finalSequence === undefined ||
      this.#frames.size !== this.#finalSequence + 1
    ) {
      return undefined
    }

    const ciphertext = new Uint8Array(this.#totalBytes)
    let offset = 0
    for (let sequence = 0; sequence <= this.#finalSequence; sequence += 1) {
      const payload = this.#frames.get(sequence)
      if (payload === undefined) {
        return undefined
      }
      ciphertext.set(payload, offset)
      offset += payload.byteLength
    }
    this.#complete = true
    this.#frames.clear()
    this.#totalBytes = 0
    return ciphertext
  }

  #acceptFinal(frame: BlockFrame): void {
    if (!frame.last) {
      return
    }
    if (this.#finalSequence !== undefined) {
      throw new ReassemblyViolation(
        'second-final',
        `block ${frame.index} has a second final frame`,
      )
    }
    for (const sequence of this.#frames.keys()) {
      if (sequence > frame.sequence) {
        throw new ReassemblyViolation(
          'past-final',
          `block ${frame.index} already has sequence ${sequence} beyond final sequence ${frame.sequence}`,
        )
      }
    }
    this.#finalSequence = frame.sequence
  }
}
