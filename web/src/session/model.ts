import type { ChunkIndex } from '../contracts/selection'
import type { ErrorFrame } from './frame'

export const REQUEST_WINDOW_BLOCKS = 8
export const DEFAULT_REQUEST_TIMEOUT_MS = 10_000
export const DEFAULT_BLOCK_ATTEMPTS = 8
export const ORDERED_REORDER_BLOCKS = REQUEST_WINDOW_BLOCKS

export interface BlockOpener {
  open(
    index: ChunkIndex,
    ciphertext: Uint8Array,
  ): Promise<Uint8Array> | Uint8Array
}

export interface ReceiveSessionOptions {
  readonly maxBlockBytes: number
  readonly requestTimeoutMs?: number
  readonly maxBlockAttempts?: number
  /** A monotonic clock may be injected so retry and scoring tests need no sleeps. */
  readonly now?: () => number
}

export type ReceiveSessionState =
  | 'idle'
  | 'running'
  | 'finalizing'
  | 'completed'
  | 'failed'
  | 'closed'

export interface ReceiveSessionSnapshot {
  readonly state: ReceiveSessionState
  readonly channels: number
  readonly assignedBlocks: number
  readonly retryBlocks: number
  readonly bufferedBlocks: number
  readonly maxBufferedBlocks: number
}

export class SessionClosedError extends Error {
  constructor(message = 'receive session is closed') {
    super(message)
    this.name = 'SessionClosedError'
  }
}

export class BlockAttemptsExhaustedError extends Error {
  readonly index: ChunkIndex
  readonly attempts: number

  constructor(index: ChunkIndex, attempts: number, reason: string) {
    super(`block ${index} failed after ${attempts} attempts (${reason})`)
    this.name = 'BlockAttemptsExhaustedError'
    this.index = index
    this.attempts = attempts
  }
}

export class PeerSessionError extends Error {
  readonly code: number

  constructor(frame: ErrorFrame) {
    super(`peer reported error 0x${frame.code.toString(16).padStart(4, '0')}: ${frame.message}`)
    this.name = 'PeerSessionError'
    this.code = frame.code
  }
}
