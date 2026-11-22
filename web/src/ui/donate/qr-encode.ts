// Zero-dependency ISO/IEC 18004 Byte-mode QR encoder (Versions 1-6, EC-L and EC-M).
// Encoding donation URLs and wallet addresses directly from config eliminates
// address-to-QR drift without adding third-party runtime packages to /donate.
export type QrErrorCorrectionLevel = 'L' | 'M'

export interface QrMatrix {
  readonly version: number
  readonly size: number
  readonly modules: Uint8Array
}

interface QrVersionSpec {
  readonly version: number
  readonly size: number
  readonly blockCount: number
  readonly dataPerBlock: number
  readonly ecPerBlock: number
}

const GF_PRIMITIVE_POLY = 0x11d
const FORMAT_BCH_POLY = 0x537
const FORMAT_XOR_MASK = 0x5412
const BYTE_MODE_INDICATOR = 0b0100
const BYTE_HEADER_BYTES = 2
const PAD_BYTE_EVEN = 0xec
const PAD_BYTE_ODD = 0x11
const FINDER_SIZE = 7
const TIMING_AXIS = 6
const MASK_COUNT = 8
const PENALTY_N1 = 3
const PENALTY_N2 = 3
const PENALTY_N3 = 40
const PENALTY_N4 = 10
const FINDER_WINDOW_MASK = 0x7ff
const FINDER_PATTERN_A = 0b10111010000
const FINDER_PATTERN_B = 0b00001011101

export const DEFAULT_QR_QUIET_ZONE = 4

// ISO/IEC 18004 Table 9 for Versions 1-6 (all have uniform blocks and <= 1 alignment pattern).
const QR_SPECS: Readonly<Record<QrErrorCorrectionLevel, readonly QrVersionSpec[]>> = {
  L: [
    { version: 1, size: 21, blockCount: 1, dataPerBlock: 19, ecPerBlock: 7 },
    { version: 2, size: 25, blockCount: 1, dataPerBlock: 34, ecPerBlock: 10 },
    { version: 3, size: 29, blockCount: 1, dataPerBlock: 55, ecPerBlock: 15 },
    { version: 4, size: 33, blockCount: 1, dataPerBlock: 80, ecPerBlock: 20 },
    { version: 5, size: 37, blockCount: 1, dataPerBlock: 108, ecPerBlock: 26 },
    { version: 6, size: 41, blockCount: 2, dataPerBlock: 68, ecPerBlock: 18 },
  ],
  M: [
    { version: 1, size: 21, blockCount: 1, dataPerBlock: 16, ecPerBlock: 10 },
    { version: 2, size: 25, blockCount: 1, dataPerBlock: 28, ecPerBlock: 16 },
    { version: 3, size: 29, blockCount: 1, dataPerBlock: 44, ecPerBlock: 26 },
    { version: 4, size: 33, blockCount: 2, dataPerBlock: 32, ecPerBlock: 18 },
    { version: 5, size: 37, blockCount: 2, dataPerBlock: 43, ecPerBlock: 24 },
    { version: 6, size: 41, blockCount: 4, dataPerBlock: 27, ecPerBlock: 16 },
  ],
}

const EC_FORMAT_BITS: Readonly<Record<QrErrorCorrectionLevel, number>> = {
  L: 0b01,
  M: 0b00,
}

const textEncoder = new TextEncoder()

function selectQrSpec(byteLength: number, level: QrErrorCorrectionLevel): QrVersionSpec {
  for (const spec of QR_SPECS[level]) {
    const capacity = spec.blockCount * spec.dataPerBlock - BYTE_HEADER_BYTES
    if (byteLength <= capacity) return spec
  }
  throw new RangeError(`QR payload (${byteLength} bytes) exceeds Version 6 EC-${level} capacity`)
}

function gfMul(a: number, b: number): number {
  let result = 0
  let x = a
  let y = b
  for (let bit = 0; bit < 8; bit += 1) {
    if ((y & 1) !== 0) result ^= x
    const carry = x & 0x80
    x = (x << 1) & 0xff
    if (carry !== 0) x ^= GF_PRIMITIVE_POLY & 0xff
    y >>= 1
  }
  return result
}

function buildRsGenerator(ecCount: number): Uint8Array {
  const poly = new Uint8Array(ecCount)
  poly[ecCount - 1] = 1
  let root = 1
  for (let i = 0; i < ecCount; i += 1) {
    for (let j = 0; j < ecCount; j += 1) {
      const next = j + 1 < ecCount ? (poly[j + 1] ?? 0) : 0
      poly[j] = gfMul(poly[j] ?? 0, root) ^ next
    }
    root = gfMul(root, 2)
  }
  return poly
}

function computeRsRemainder(data: Uint8Array, generator: Uint8Array): Uint8Array {
  const ecCount = generator.length
  const rem = new Uint8Array(ecCount)
  for (const byte of data) {
    const factor = byte ^ (rem[0] ?? 0)
    rem.copyWithin(0, 1)
    rem[ecCount - 1] = 0
    for (let i = 0; i < ecCount; i += 1) {
      rem[i] = (rem[i] ?? 0) ^ gfMul(generator[i] ?? 0, factor)
    }
  }
  return rem
}

function encodeByteCodewords(bytes: Uint8Array, totalDataCodewords: number): Uint8Array {
  const out = new Uint8Array(totalDataCodewords)
  const len = bytes.length
  // Versions 1-9 byte mode: 4-bit mode (0100) + 8-bit length + 8-bit payload bytes + 4-bit terminator.
  out[0] = (BYTE_MODE_INDICATOR << 4) | ((len >> 4) & 0x0f)
  let prevLowNibble = len & 0x0f
  for (let i = 0; i < len; i += 1) {
    const byte = bytes[i] ?? 0
    out[i + 1] = (prevLowNibble << 4) | ((byte >> 4) & 0x0f)
    prevLowNibble = byte & 0x0f
  }
  out[len + 1] = prevLowNibble << 4

  let padToggle = false
  for (let i = len + 2; i < totalDataCodewords; i += 1) {
    out[i] = padToggle ? PAD_BYTE_ODD : PAD_BYTE_EVEN
    padToggle = !padToggle
  }
  return out
}

function buildInterleavedCodewords(data: Uint8Array, spec: QrVersionSpec): Uint8Array {
  const generator = buildRsGenerator(spec.ecPerBlock)
  const dataBlocks: Uint8Array[] = []
  const ecBlocks: Uint8Array[] = []
  for (let b = 0; b < spec.blockCount; b += 1) {
    const start = b * spec.dataPerBlock
    const block = data.subarray(start, start + spec.dataPerBlock)
    dataBlocks.push(block)
    ecBlocks.push(computeRsRemainder(block, generator))
  }

  const out = new Uint8Array(spec.blockCount * (spec.dataPerBlock + spec.ecPerBlock))
  let cursor = 0
  for (let i = 0; i < spec.dataPerBlock; i += 1) {
    for (const block of dataBlocks) {
      out[cursor] = block[i] ?? 0
      cursor += 1
    }
  }
  for (let i = 0; i < spec.ecPerBlock; i += 1) {
    for (const block of ecBlocks) {
      out[cursor] = block[i] ?? 0
      cursor += 1
    }
  }
  return out
}

function placeFinder(
  grid: Uint8Array,
  isFunc: Uint8Array,
  size: number,
  originX: number,
  originY: number,
): void {
  for (let dy = -1; dy <= FINDER_SIZE; dy += 1) {
    const y = originY + dy
    if (y < 0 || y >= size) continue
    for (let dx = -1; dx <= FINDER_SIZE; dx += 1) {
      const x = originX + dx
      if (x < 0 || x >= size) continue
      const idx = y * size + x
      isFunc[idx] = 1
      const dist = Math.max(Math.abs(dx - 3), Math.abs(dy - 3))
      grid[idx] = dist !== 2 && dist !== 4 ? 1 : 0
    }
  }
}

function placeAlignment(grid: Uint8Array, isFunc: Uint8Array, size: number, center: number): void {
  for (let dy = -2; dy <= 2; dy += 1) {
    for (let dx = -2; dx <= 2; dx += 1) {
      const idx = (center + dy) * size + (center + dx)
      isFunc[idx] = 1
      const dist = Math.max(Math.abs(dx), Math.abs(dy))
      grid[idx] = dist !== 1 ? 1 : 0
    }
  }
}

function placeFunctionPatterns(spec: QrVersionSpec): {
  readonly grid: Uint8Array
  readonly isFunc: Uint8Array
} {
  const { size, version } = spec
  const grid = new Uint8Array(size * size)
  const isFunc = new Uint8Array(size * size)

  placeFinder(grid, isFunc, size, 0, 0)
  placeFinder(grid, isFunc, size, size - FINDER_SIZE, 0)
  placeFinder(grid, isFunc, size, 0, size - FINDER_SIZE)

  if (version >= 2) {
    placeAlignment(grid, isFunc, size, size - FINDER_SIZE)
  }

  for (let i = FINDER_SIZE + 1; i < size - FINDER_SIZE - 1; i += 1) {
    const bit = (i + 1) & 1
    const hIdx = TIMING_AXIS * size + i
    const vIdx = i * size + TIMING_AXIS
    isFunc[hIdx] = 1
    grid[hIdx] = bit
    isFunc[vIdx] = 1
    grid[vIdx] = bit
  }

  // Reserve format strips and dark module before zigzag data placement.
  for (let i = 0; i <= 8; i += 1) {
    isFunc[8 * size + i] = 1
    isFunc[i * size + 8] = 1
  }
  for (let i = size - 8; i < size; i += 1) {
    isFunc[8 * size + i] = 1
    isFunc[i * size + 8] = 1
  }

  return { grid, isFunc }
}

function readCodewordBit(codewords: Uint8Array, bitIndex: number): number {
  const byteIndex = bitIndex >> 3
  if (byteIndex >= codewords.length) return 0
  const byte = codewords[byteIndex] ?? 0
  return (byte >> (7 - (bitIndex & 7))) & 1
}

function placeDataBits(
  grid: Uint8Array,
  isFunc: Uint8Array,
  size: number,
  codewords: Uint8Array,
): void {
  let bitIndex = 0
  let upward = true
  let right = size - 1
  while (right >= 1) {
    if (right === TIMING_AXIS) right = TIMING_AXIS - 1
    for (let step = 0; step < size; step += 1) {
      const y = upward ? size - 1 - step : step
      for (let offset = 0; offset < 2; offset += 1) {
        const idx = y * size + (right - offset)
        if (isFunc[idx] === 1) continue
        grid[idx] = readCodewordBit(codewords, bitIndex)
        bitIndex += 1
      }
    }
    upward = !upward
    right -= 2
  }
}

function shouldMaskModule(mask: number, x: number, y: number): boolean {
  switch (mask) {
    case 0:
      return (x + y) % 2 === 0
    case 1:
      return y % 2 === 0
    case 2:
      return x % 3 === 0
    case 3:
      return (x + y) % 3 === 0
    case 4:
      return (Math.floor(x / 3) + Math.floor(y / 2)) % 2 === 0
    case 5:
      return ((x * y) % 2) + ((x * y) % 3) === 0
    case 6:
      return (((x * y) % 2) + ((x * y) % 3)) % 2 === 0
    default:
      return (((x + y) % 2) + ((x * y) % 3)) % 2 === 0
  }
}

function writeFormatInfo(
  grid: Uint8Array,
  size: number,
  level: QrErrorCorrectionLevel,
  mask: number,
): void {
  const data = (EC_FORMAT_BITS[level] << 3) | mask
  let rem = data
  for (let i = 0; i < 10; i += 1) {
    rem = (rem << 1) ^ ((rem >> 9) * FORMAT_BCH_POLY)
  }
  const bits = ((data << 10) | rem) ^ FORMAT_XOR_MASK

  for (let i = 0; i <= 5; i += 1) {
    grid[i * size + 8] = (bits >> i) & 1
  }
  grid[7 * size + 8] = (bits >> 6) & 1
  grid[8 * size + 8] = (bits >> 7) & 1
  grid[8 * size + 7] = (bits >> 8) & 1
  for (let i = 9; i <= 14; i += 1) {
    grid[8 * size + (14 - i)] = (bits >> i) & 1
  }
  for (let i = 0; i <= 7; i += 1) {
    grid[8 * size + (size - 1 - i)] = (bits >> i) & 1
  }
  for (let i = 8; i <= 14; i += 1) {
    grid[(size - 15 + i) * size + 8] = (bits >> i) & 1
  }
  grid[(size - 8) * size + 8] = 1
}

function applyMaskCandidate(
  base: Uint8Array,
  isFunc: Uint8Array,
  size: number,
  level: QrErrorCorrectionLevel,
  mask: number,
): Uint8Array {
  const candidate = base.slice()
  for (let y = 0; y < size; y += 1) {
    const rowOffset = y * size
    for (let x = 0; x < size; x += 1) {
      const idx = rowOffset + x
      if (isFunc[idx] === 0 && shouldMaskModule(mask, x, y)) {
        candidate[idx] = (candidate[idx] ?? 0) ^ 1
      }
    }
  }
  writeFormatInfo(candidate, size, level, mask)
  return candidate
}

function scoreSingleLine(
  grid: Uint8Array,
  size: number,
  outer: number,
  horizontal: boolean,
): number {
  let penalty = 0
  let runColor = -1
  let runLen = 0
  let window = 0
  for (let inner = 0; inner < size; inner += 1) {
    const idx = horizontal ? outer * size + inner : inner * size + outer
    const color = grid[idx] ?? 0
    if (color === runColor) {
      runLen += 1
      if (runLen === 5) penalty += PENALTY_N1
      else if (runLen > 5) penalty += 1
    } else {
      runColor = color
      runLen = 1
    }
    window = ((window << 1) | color) & FINDER_WINDOW_MASK
    if (inner >= 10 && (window === FINDER_PATTERN_A || window === FINDER_PATTERN_B)) {
      penalty += PENALTY_N3
    }
  }
  return penalty
}

function scoreLineRunsAndFinders(
  grid: Uint8Array,
  size: number,
  horizontal: boolean,
): number {
  let penalty = 0
  for (let outer = 0; outer < size; outer += 1) {
    penalty += scoreSingleLine(grid, size, outer, horizontal)
  }
  return penalty
}

function scoreBlocksAndBalance(grid: Uint8Array, size: number): number {
  let penalty = 0
  for (let y = 0; y < size - 1; y += 1) {
    const row = y * size
    const nextRow = row + size
    for (let x = 0; x < size - 1; x += 1) {
      const color = grid[row + x] ?? 0
      if (
        color === grid[row + x + 1]
        && color === grid[nextRow + x]
        && color === grid[nextRow + x + 1]
      ) {
        penalty += PENALTY_N2
      }
    }
  }

  let darkCount = 0
  for (const module of grid) {
    darkCount += module
  }
  const total = size * size
  const deviationSteps = Math.floor(Math.abs(darkCount * 20 - total * 10) / total)
  return penalty + deviationSteps * PENALTY_N4
}

function evaluatePenalty(grid: Uint8Array, size: number): number {
  return (
    scoreLineRunsAndFinders(grid, size, true)
    + scoreLineRunsAndFinders(grid, size, false)
    + scoreBlocksAndBalance(grid, size)
  )
}

export function encodeQrMatrix(
  text: string,
  level: QrErrorCorrectionLevel = 'M',
): QrMatrix {
  const bytes = textEncoder.encode(text)
  const spec = selectQrSpec(bytes.length, level)
  const data = encodeByteCodewords(bytes, spec.blockCount * spec.dataPerBlock)
  const codewords = buildInterleavedCodewords(data, spec)
  const { grid, isFunc } = placeFunctionPatterns(spec)
  placeDataBits(grid, isFunc, spec.size, codewords)

  let bestModules = applyMaskCandidate(grid, isFunc, spec.size, level, 0)
  let bestPenalty = evaluatePenalty(bestModules, spec.size)
  for (let mask = 1; mask < MASK_COUNT; mask += 1) {
    const candidate = applyMaskCandidate(grid, isFunc, spec.size, level, mask)
    const penalty = evaluatePenalty(candidate, spec.size)
    if (penalty < bestPenalty) {
      bestPenalty = penalty
      bestModules = candidate
    }
  }

  return {
    version: spec.version,
    size: spec.size,
    modules: bestModules,
  }
}

export function qrMatrixToSvgPath(
  matrix: QrMatrix,
  quietZone: number = DEFAULT_QR_QUIET_ZONE,
): string {
  const { size, modules } = matrix
  const parts: string[] = []
  for (let y = 0; y < size; y += 1) {
    const rowOffset = y * size
    let x = 0
    while (x < size) {
      if (modules[rowOffset + x] === 1) {
        const start = x
        while (x < size && modules[rowOffset + x] === 1) {
          x += 1
        }
        const run = x - start
        parts.push(`M${start + quietZone} ${y + quietZone}h${run}v1h-${run}z`)
      } else {
        x += 1
      }
    }
  }
  return parts.join('')
}
