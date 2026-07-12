import type { ChunkIndex } from './selection'

export type DeliveryOrder = 'any' | 'ascending'

/** The minimal resume-state projection a scheduler needs from a sink. */
export interface ChunkAvailability {
  has(index: ChunkIndex): boolean
}

/**
 * A sink owns delivery ordering because only the storage strategy knows whether
 * out-of-order blocks are safe. Composition code must not override this value.
 */
export interface BlockSink<O extends DeliveryOrder = DeliveryOrder>
  extends ChunkAvailability {
  readonly deliveryOrder: O

  /** Implementations retaining plaintext must snapshot it before awaiting. */
  writeBlock(index: ChunkIndex, plaintext: Uint8Array): Promise<void>
  finalize(): Promise<void>
  abort(reason: unknown): Promise<void>
}

export type RandomWriteBlockSink = BlockSink<'any'>
export type OrderedBlockSink = BlockSink<'ascending'>
