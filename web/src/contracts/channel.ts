export const MIN_FRAME_BYTES = 1
export const MAX_FRAME_BYTES = 65_536

export type Frame = Uint8Array
export type ChannelState = 'connecting' | 'open' | 'closed'

/**
 * A reliable, ordered, transport-neutral frame channel.
 *
 * Implementations validate complete frame length before changing state. They
 * also snapshot outbound, inbound, and inbound-terminal bytes because callers
 * may reuse buffers after an operation accepts them. The receive stream has one
 * consumer so competing readers cannot silently partition a session.
 */
export interface FrameChannel {
  readonly state: ChannelState
  readonly frames: ReadableStream<Frame>

  /** Backpressure resolves on capacity and rejects on abort or remote close. */
  send(frame: Frame, signal?: AbortSignal): Promise<void>

  /**
   * Successful completion guarantees this frame is observed last before the
   * receive stream closes; close cannot overtake an accepted terminal.
   */
  sendTerminal(frame: Frame, signal?: AbortSignal): Promise<void>

  /**
   * Local and remote closure are idempotent and wake blocked sends. Late traffic
   * is discarded rather than reviving a closed receive stream.
   */
  close(): Promise<void>
}
