import { MAX_FRAME_BYTES } from '../contracts/channel'

export const FRAME_REQUEST = 0x01
export const FRAME_BLOCK = 0x02
export const FRAME_ERROR = 0x03
export const BLOCK_FLAG_LAST = 0x01

export const REQUEST_HEADER_BYTES = 5
export const BLOCK_HEADER_BYTES = 18
export const ERROR_HEADER_BYTES = 5
export const MAX_BLOCK_PAYLOAD_BYTES = MAX_FRAME_BYTES - BLOCK_HEADER_BYTES
export const MAX_REQUEST_INDICES = Math.floor(
  (MAX_FRAME_BYTES - REQUEST_HEADER_BYTES) / 8,
)
export const MAX_ERROR_MESSAGE_BYTES = MAX_FRAME_BYTES - ERROR_HEADER_BYTES

export const ERROR_CODE_BAD_REQUEST = 0x0001
export const ERROR_CODE_BLOCK_READ = 0x0002
export const ERROR_CODE_SEAL = 0x0003

const MAX_U32 = 0xffff_ffff
const MAX_U64 = 0xffff_ffff_ffff_ffffn
const UTF8_ENCODER = new TextEncoder()
const UTF8_DECODER = new TextDecoder('utf-8', { fatal: true })

export type FrameCodecErrorKind =
  | 'unknown-type'
  | 'malformed'
  | 'oversize'
  | 'empty-request'
  | 'empty-payload'
  | 'unknown-flags'
  | 'invalid-utf8'
  | 'out-of-range'

export class FrameCodecError extends Error {
  readonly kind: FrameCodecErrorKind

  constructor(kind: FrameCodecErrorKind, message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'FrameCodecError'
    this.kind = kind
  }
}

export interface RequestFrame {
  readonly type: 'request'
  readonly indices: readonly bigint[]
}

export interface BlockFrame {
  readonly type: 'block'
  readonly index: bigint
  readonly sequence: number
  readonly last: boolean
  readonly payload: Uint8Array
}

export interface ErrorFrame {
  readonly type: 'error'
  readonly code: number
  readonly message: string
}

export type SessionFrame = RequestFrame | BlockFrame | ErrorFrame

export function isFatalErrorCode(code: number): boolean {
  return code === ERROR_CODE_BLOCK_READ || code === ERROR_CODE_SEAL
}

function checkedU64(value: bigint, label: string): bigint {
  if (value < 0n || value > MAX_U64) {
    throw new FrameCodecError(
      'out-of-range',
      `${label} must be an unsigned 64-bit integer`,
    )
  }
  return value
}

function checkedU32(value: number, label: string): number {
  if (!Number.isInteger(value) || value < 0 || value > MAX_U32) {
    throw new FrameCodecError(
      'out-of-range',
      `${label} must be an unsigned 32-bit integer`,
    )
  }
  return value
}

function checkedU16(value: number, label: string): number {
  if (!Number.isInteger(value) || value < 0 || value > 0xffff) {
    throw new FrameCodecError(
      'out-of-range',
      `${label} must be an unsigned 16-bit integer`,
    )
  }
  return value
}

export function encodeRequest(indices: readonly bigint[]): Uint8Array {
  if (indices.length === 0) {
    throw new FrameCodecError('empty-request', 'REQUEST must contain an index')
  }
  if (indices.length > MAX_REQUEST_INDICES) {
    throw new FrameCodecError(
      'oversize',
      `REQUEST contains ${indices.length} indices, limit ${MAX_REQUEST_INDICES}`,
    )
  }

  const frame = new Uint8Array(REQUEST_HEADER_BYTES + indices.length * 8)
  const view = new DataView(frame.buffer)
  frame[0] = FRAME_REQUEST
  view.setUint32(1, indices.length, true)
  indices.forEach((index, offset) => {
    view.setBigUint64(REQUEST_HEADER_BYTES + offset * 8, checkedU64(index, 'index'), true)
  })
  return frame
}

export function encodeBlock(block: Omit<BlockFrame, 'type'>): Uint8Array {
  checkedU64(block.index, 'block index')
  checkedU32(block.sequence, 'block sequence')
  if (block.payload.byteLength === 0) {
    throw new FrameCodecError('empty-payload', 'BLOCK payload must not be empty')
  }
  if (block.payload.byteLength > MAX_BLOCK_PAYLOAD_BYTES) {
    throw new FrameCodecError(
      'oversize',
      `BLOCK payload is ${block.payload.byteLength} bytes, limit ${MAX_BLOCK_PAYLOAD_BYTES}`,
    )
  }

  const frame = new Uint8Array(BLOCK_HEADER_BYTES + block.payload.byteLength)
  const view = new DataView(frame.buffer)
  frame[0] = FRAME_BLOCK
  view.setBigUint64(1, block.index, true)
  view.setUint32(9, block.sequence, true)
  frame[13] = block.last ? BLOCK_FLAG_LAST : 0
  view.setUint32(14, block.payload.byteLength, true)
  frame.set(block.payload, BLOCK_HEADER_BYTES)
  return frame
}

export function encodeError(code: number, message: string): Uint8Array {
  checkedU16(code, 'error code')
  const encoded = UTF8_ENCODER.encode(message)
  if (UTF8_DECODER.decode(encoded) !== message) {
    throw new FrameCodecError(
      'invalid-utf8',
      'ERROR message contains an unpaired UTF-16 surrogate',
    )
  }
  if (encoded.byteLength > MAX_ERROR_MESSAGE_BYTES) {
    throw new FrameCodecError(
      'oversize',
      `ERROR message is ${encoded.byteLength} bytes, limit ${MAX_ERROR_MESSAGE_BYTES}`,
    )
  }

  const frame = new Uint8Array(ERROR_HEADER_BYTES + encoded.byteLength)
  const view = new DataView(frame.buffer)
  frame[0] = FRAME_ERROR
  view.setUint16(1, code, true)
  view.setUint16(3, encoded.byteLength, true)
  frame.set(encoded, ERROR_HEADER_BYTES)
  return frame
}

function requireDeclaredLength(
  frame: Uint8Array,
  expected: number,
  kind: string,
): void {
  if (frame.byteLength !== expected) {
    throw new FrameCodecError(
      'malformed',
      `${kind} declares ${expected} bytes but frame has ${frame.byteLength}`,
    )
  }
}

function decodeRequest(frame: Uint8Array, view: DataView): RequestFrame {
  if (frame.byteLength < REQUEST_HEADER_BYTES) {
    throw new FrameCodecError('malformed', 'REQUEST header is truncated')
  }
  const count = view.getUint32(1, true)
  if (count === 0) {
    throw new FrameCodecError('empty-request', 'REQUEST must contain an index')
  }
  requireDeclaredLength(frame, REQUEST_HEADER_BYTES + count * 8, 'REQUEST')
  const indices = Array.from({ length: count }, (_, offset) =>
    view.getBigUint64(REQUEST_HEADER_BYTES + offset * 8, true),
  )
  return Object.freeze({ type: 'request', indices: Object.freeze(indices) })
}

function decodeBlock(frame: Uint8Array, view: DataView): BlockFrame {
  if (frame.byteLength < BLOCK_HEADER_BYTES) {
    throw new FrameCodecError('malformed', 'BLOCK header is truncated')
  }
  const flags = frame[13]
  if (flags === undefined || (flags & ~BLOCK_FLAG_LAST) !== 0) {
    throw new FrameCodecError(
      'unknown-flags',
      `BLOCK has undefined flags 0x${(flags ?? 0).toString(16).padStart(2, '0')}`,
    )
  }
  const payloadLength = view.getUint32(14, true)
  if (payloadLength === 0) {
    throw new FrameCodecError('empty-payload', 'BLOCK payload must not be empty')
  }
  requireDeclaredLength(frame, BLOCK_HEADER_BYTES + payloadLength, 'BLOCK')
  return {
    type: 'block',
    index: view.getBigUint64(1, true),
    sequence: view.getUint32(9, true),
    last: (flags & BLOCK_FLAG_LAST) !== 0,
    payload: frame.slice(BLOCK_HEADER_BYTES),
  }
}

function decodeError(frame: Uint8Array, view: DataView): ErrorFrame {
  if (frame.byteLength < ERROR_HEADER_BYTES) {
    throw new FrameCodecError('malformed', 'ERROR header is truncated')
  }
  const messageLength = view.getUint16(3, true)
  requireDeclaredLength(frame, ERROR_HEADER_BYTES + messageLength, 'ERROR')
  try {
    return {
      type: 'error',
      code: view.getUint16(1, true),
      message: UTF8_DECODER.decode(frame.subarray(ERROR_HEADER_BYTES)),
    }
  } catch (cause) {
    throw new FrameCodecError('invalid-utf8', 'ERROR message is not valid UTF-8', {
      cause,
    })
  }
}

export function decodeFrame(frame: Uint8Array): SessionFrame {
  if (frame.byteLength > MAX_FRAME_BYTES) {
    throw new FrameCodecError(
      'oversize',
      `frame is ${frame.byteLength} bytes, limit ${MAX_FRAME_BYTES}`,
    )
  }
  if (frame.byteLength === 0) {
    throw new FrameCodecError('malformed', 'frame must not be empty')
  }

  const view = new DataView(frame.buffer, frame.byteOffset, frame.byteLength)
  switch (frame[0]) {
    case FRAME_REQUEST:
      return decodeRequest(frame, view)
    case FRAME_BLOCK:
      return decodeBlock(frame, view)
    case FRAME_ERROR:
      return decodeError(frame, view)
    default:
      throw new FrameCodecError(
        'unknown-type',
        `unknown frame type 0x${frame[0]?.toString(16).padStart(2, '0')}`,
      )
  }
}

export function splitBlockCiphertext(
  index: bigint,
  ciphertext: Uint8Array,
  maxPayloadBytes = MAX_BLOCK_PAYLOAD_BYTES,
): readonly Uint8Array[] {
  checkedU64(index, 'block index')
  if (ciphertext.byteLength === 0) {
    throw new FrameCodecError('empty-payload', 'block ciphertext must not be empty')
  }
  if (
    !Number.isInteger(maxPayloadBytes) ||
    maxPayloadBytes < 1 ||
    maxPayloadBytes > MAX_BLOCK_PAYLOAD_BYTES
  ) {
    throw new FrameCodecError(
      'out-of-range',
      `maximum payload must be in [1, ${MAX_BLOCK_PAYLOAD_BYTES}]`,
    )
  }

  const frameCount = Math.ceil(ciphertext.byteLength / maxPayloadBytes)
  if (frameCount > MAX_U32) {
    throw new FrameCodecError('oversize', 'block needs more than the u32 sequence space')
  }
  return Object.freeze(
    Array.from({ length: frameCount }, (_, sequence) => {
      const first = sequence * maxPayloadBytes
      return encodeBlock({
        index,
        sequence,
        last: sequence === frameCount - 1,
        payload: ciphertext.subarray(first, first + maxPayloadBytes),
      })
    }),
  )
}
