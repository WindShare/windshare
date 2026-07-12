import type { FrameChannel } from '../contracts/channel'
import type { ChunkIndex } from '../contracts/selection'
import type { BlockReassembler } from './reassembly'

export interface Flight {
  readonly channel: ChannelEntry
  sentAt: number
  pending: boolean
}

export interface RequestSend {
  readonly indices: readonly ChunkIndex[]
  readonly controller: AbortController
  cleanup(): void
}

export interface ChannelEntry {
  readonly channel: FrameChannel
  readonly inflight: Set<ChunkIndex>
  readonly partial: Map<ChunkIndex, BlockReassembler>
  retired: boolean
  score: number
  sending: RequestSend | undefined
  reader: ReadableStreamDefaultReader<Uint8Array> | undefined
}

export function createChannelEntry(channel: FrameChannel): ChannelEntry {
  return {
    channel,
    inflight: new Set(),
    partial: new Map(),
    retired: false,
    score: 0,
    sending: undefined,
    reader: undefined,
  }
}
