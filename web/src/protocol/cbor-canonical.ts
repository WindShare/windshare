export const MAXIMUM_CBOR_DEPTH = 16
export const MAXIMUM_CBOR_ARRAY_ITEMS = 2_048
export const MAXIMUM_CBOR_MAP_FIELDS = 256

const fatalUtf8 = new TextDecoder('utf-8', { fatal: true })
const MAXIMUM_INLINE_ARGUMENT = 23
const ARGUMENT_WIDTHS = [1, 2, 4, 8] as const
const MINIMUM_ARGUMENTS = [24n, 256n, 65_536n, 4_294_967_296n] as const

function readArgument(view: DataView, offset: number, width: number): number | bigint {
  if (width === 8) return view.getBigUint64(offset)
  if (width === 4) return view.getUint32(offset)
  if (width === 2) return view.getUint16(offset)
  return view.getUint8(offset)
}

/**
 * Validate the wire representation without copying or comparing opaque payloads.
 * Schema maps have unsigned integer keys, whose canonical byte order is numeric
 * order. Integers, definite lengths and the three simple values exhaust the
 * protocol's supported CBOR representations.
 */
export function validateCanonicalEncoding(encoded: Uint8Array): void {
  const view = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength)
  let offset = 0

  const byte = (): number => {
    if (offset >= encoded.byteLength) throw new Error('Truncated CBOR')
    return encoded[offset++]!
  }
  const argument = (additional: number): number | bigint => {
    if (additional <= MAXIMUM_INLINE_ARGUMENT) return additional
    const index = additional - MAXIMUM_INLINE_ARGUMENT - 1
    const width = ARGUMENT_WIDTHS[index]
    if (width === undefined || offset + width > encoded.byteLength) {
      throw new Error('Invalid CBOR argument')
    }
    const value = readArgument(view, offset, width)
    offset += width
    if (BigInt(value) < MINIMUM_ARGUMENTS[index]!) throw new Error('Nonminimal CBOR argument')
    return value
  }
  const length = (value: number | bigint, maximum: number): number => {
    if (value > maximum) throw new Error('CBOR length exceeds its boundary')
    return Number(value)
  }
  const map = (depth: number, value: number | bigint): void => {
    const count = length(value, MAXIMUM_CBOR_MAP_FIELDS)
    let previous = -1
    for (let i = 0; i < count; i++) {
      const keyHeader = byte()
      if (keyHeader >>> 5 !== 0) throw new Error('CBOR schema keys must be unsigned integers')
      const key = length(argument(keyHeader & 31), Number.MAX_SAFE_INTEGER)
      if (key <= previous) throw new Error('CBOR map keys are duplicated or unordered')
      previous = key
      item(depth + 1)
    }
  }
  const item = (depth: number): void => {
    if (depth > MAXIMUM_CBOR_DEPTH) throw new Error('CBOR nesting exceeds the protocol limit')
    const initial = byte()
    const major = initial >>> 5
    const additional = initial & 31
    if (major === 7) {
      if (initial !== 0xf4 && initial !== 0xf5 && initial !== 0xf6) {
        throw new Error('Unsupported CBOR simple value')
      }
      return
    }
    if (major === 6) throw new Error('CBOR tags are forbidden')
    const value = argument(additional)
    if (major <= 1) return
    if (major === 2 || major === 3) {
      const size = length(value, encoded.byteLength - offset)
      if (major === 3) fatalUtf8.decode(encoded.subarray(offset, offset + size))
      offset += size
      return
    }
    if (major === 4) {
      const count = length(value, MAXIMUM_CBOR_ARRAY_ITEMS)
      for (let i = 0; i < count; i++) item(depth + 1)
      return
    }
    map(depth, value)
  }
  item(0)
  if (offset !== encoded.byteLength) throw new Error('Trailing CBOR bytes')
}
