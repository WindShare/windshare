import { describe, expect, it } from 'vitest'
import {
  AFDIAN_ORDER_URL,
  DEFAULT_DIRECT_WALLET_ID,
  DIRECT_CRYPTO_WALLETS,
  OFFICIAL_DONATE_ORIGINS,
  OFFICIAL_DONATE_PAGE_URL,
  canCreateCryptoIntent,
} from '../../src/ui/donate/config'
import {
  MAX_DISPLAY_NAME_CODEPOINTS,
  MAX_EPOCH_SECONDS,
  MAX_MESSAGE_CODEPOINTS,
  countCodePoints,
  formatDonationAmount,
  formatIsoTimestamp,
  formatRelativeTime,
  parseCryptoAmountInput,
  validateCryptoCheckoutInput,
} from '../../src/ui/donate/format'
import { encodeQrMatrix, qrMatrixToSvgPath, type QrErrorCorrectionLevel, type QrMatrix } from '../../src/ui/donate/qr-encode'

// Independent ISO/IEC 18004 verifier for test round-tripping: reads both format
// copies, checks BCH parity, unmasks codewords, verifies Reed-Solomon syndromes,
// and decodes the UTF-8 Byte-mode payload back to text.
const TEST_RS_LAYOUT: Readonly<Record<string, { readonly blocks: number; readonly data: number; readonly ec: number }>> = {
  '1-L': { blocks: 1, data: 19, ec: 7 },
  '1-M': { blocks: 1, data: 16, ec: 10 },
  '2-L': { blocks: 1, data: 34, ec: 10 },
  '2-M': { blocks: 1, data: 28, ec: 16 },
  '3-L': { blocks: 1, data: 55, ec: 15 },
  '3-M': { blocks: 1, data: 44, ec: 26 },
  '4-L': { blocks: 1, data: 80, ec: 20 },
  '4-M': { blocks: 2, data: 32, ec: 18 },
  '5-L': { blocks: 1, data: 108, ec: 26 },
  '5-M': { blocks: 2, data: 43, ec: 24 },
  '6-L': { blocks: 2, data: 68, ec: 18 },
  '6-M': { blocks: 4, data: 27, ec: 16 },
}

function gfMul256(a: number, b: number): number {
  let out = 0
  let x = a
  let y = b
  for (let i = 0; i < 8; i += 1) {
    if ((y & 1) !== 0) out ^= x
    const hi = x & 0x80
    x = (x << 1) & 0xff
    if (hi !== 0) x ^= 0x1d
    y >>= 1
  }
  return out
}

function evalMask(mask: number, x: number, y: number): boolean {
  switch (mask) {
    case 0: return (x + y) % 2 === 0
    case 1: return y % 2 === 0
    case 2: return x % 3 === 0
    case 3: return (x + y) % 3 === 0
    case 4: return (Math.floor(x / 3) + Math.floor(y / 2)) % 2 === 0
    case 5: return ((x * y) % 2) + ((x * y) % 3) === 0
    case 6: return (((x * y) % 2) + ((x * y) % 3)) % 2 === 0
    default: return (((x + y) % 2) + ((x * y) % 3)) % 2 === 0
  }
}

interface TestRsLayout {
  readonly blocks: number
  readonly data: number
  readonly ec: number
}

function readVerifiedFormatInfo(matrix: QrMatrix): {
  readonly level: QrErrorCorrectionLevel
  readonly mask: number
} {
  const { size, modules } = matrix
  const at = (x: number, y: number) => modules[y * size + x] ?? 0
  expect(at(8, size - 8)).toBe(1)

  let fmtA = 0
  for (let i = 0; i <= 5; i += 1) fmtA |= at(8, i) << i
  fmtA |= at(8, 7) << 6
  fmtA |= at(8, 8) << 7
  fmtA |= at(7, 8) << 8
  for (let i = 9; i <= 14; i += 1) fmtA |= at(14 - i, 8) << i

  let fmtB = 0
  for (let i = 0; i <= 7; i += 1) fmtB |= at(size - 1 - i, 8) << i
  for (let i = 8; i <= 14; i += 1) fmtB |= at(8, size - 15 + i) << i
  expect(fmtA).toBe(fmtB)

  const unmaskedFmt = fmtA ^ 0x5412
  let rem = unmaskedFmt
  for (let bit = 14; bit >= 10; bit -= 1) {
    if (((rem >> bit) & 1) !== 0) rem ^= 0x537 << (bit - 10)
  }
  expect(rem).toBe(0)

  const ecBits = (unmaskedFmt >> 13) & 3
  return {
    level: ecBits === 0b01 ? 'L' : 'M',
    mask: (unmaskedFmt >> 10) & 7,
  }
}

function isQrFunctionModule(version: number, size: number, x: number, y: number): boolean {
  if ((x <= 8 && y <= 8) || (x >= size - 8 && y <= 8) || (x <= 8 && y >= size - 8)) return true
  if (x === 6 || y === 6) return true
  const align = size - 7
  return version >= 2 && Math.abs(x - align) <= 2 && Math.abs(y - align) <= 2
}

function extractUnmaskedCodewords(matrix: QrMatrix, mask: number, totalCw: number): Uint8Array {
  const { size, modules, version } = matrix
  const bits: number[] = []
  let upward = true
  let right = size - 1
  while (right >= 1) {
    if (right === 6) right = 5
    for (let step = 0; step < size; step += 1) {
      const y = upward ? size - 1 - step : step
      for (let off = 0; off < 2; off += 1) {
        const x = right - off
        if (isQrFunctionModule(version, size, x, y)) continue
        bits.push((modules[y * size + x] ?? 0) ^ (evalMask(mask, x, y) ? 1 : 0))
      }
    }
    upward = !upward
    right -= 2
  }

  const cw = new Uint8Array(totalCw)
  for (let i = 0; i < totalCw; i += 1) {
    let val = 0
    for (let b = 0; b < 8; b += 1) val = (val << 1) | (bits[i * 8 + b] ?? 0)
    cw[i] = val
  }
  return cw
}

function verifyRsAndDecodeBytes(cw: Uint8Array, layout: TestRsLayout): string {
  const dataBytes: number[] = []
  const ecStart = layout.blocks * layout.data
  for (let blk = 0; blk < layout.blocks; blk += 1) {
    const blockCw: number[] = []
    for (let i = 0; i < layout.data; i += 1) {
      const byte = cw[i * layout.blocks + blk] ?? 0
      blockCw.push(byte)
      dataBytes.push(byte)
    }
    for (let i = 0; i < layout.ec; i += 1) {
      blockCw.push(cw[ecStart + i * layout.blocks + blk] ?? 0)
    }
    let root = 1
    for (let r = 0; r < layout.ec; r += 1) {
      let syndrome = 0
      for (const coeff of blockCw) syndrome = gfMul256(syndrome, root) ^ coeff
      expect(syndrome).toBe(0)
      root = gfMul256(root, 2)
    }
  }

  const first = dataBytes[0] ?? 0
  const second = dataBytes[1] ?? 0
  expect(first >> 4).toBe(0b0100)
  const payloadLen = ((first & 0x0f) << 4) | (second >> 4)
  const payload = new Uint8Array(payloadLen)
  for (let i = 0; i < payloadLen; i += 1) {
    payload[i] = (((dataBytes[i + 1] ?? 0) & 0x0f) << 4) | ((dataBytes[i + 2] ?? 0) >> 4)
  }
  return new TextDecoder().decode(payload)
}

function decodeAndVerifyQr(matrix: QrMatrix): {
  readonly level: QrErrorCorrectionLevel
  readonly mask: number
  readonly text: string
} {
  const { level, mask } = readVerifiedFormatInfo(matrix)
  const layout = TEST_RS_LAYOUT[`${matrix.version}-${level}`]
  expect(layout).toBeDefined()
  if (layout === undefined) throw new Error(`Unexpected layout ${matrix.version}-${level}`)
  const cw = extractUnmaskedCodewords(matrix, mask, layout.blocks * (layout.data + layout.ec))
  return { level, mask, text: verifyRsAndDecodeBytes(cw, layout) }
}

describe('donate pure formatting and validation helpers', () => {
  it('formats fixed-point minor amounts accurately across currencies', () => {
    expect(formatDonationAmount(1000, 'CNY', 2)).toBe('¥10')
    expect(formatDonationAmount(1250, 'CNY', 2)).toBe('¥12.50')
    expect(formatDonationAmount(500, 'USD', 2)).toBe('$5')
    expect(formatDonationAmount(100500, 'USD', 2)).toBe('$1,005')
    expect(formatDonationAmount(1000, 'JPY', 0)).toBe('JPY 1,000')
    expect(formatDonationAmount(12345, 'CAD', 3)).toBe('CAD 12.345')
  })

  it('clamps negative and non-finite numeric inputs in formatDonationAmount', () => {
    expect(formatDonationAmount(-500, 'USD', 2)).toBe('$0')
    expect(formatDonationAmount(Number.NaN, 'CNY', 2)).toBe('¥0')
    expect(formatDonationAmount(Number.POSITIVE_INFINITY, 'USD', 2)).toBe('$0')
    expect(formatDonationAmount(Number.NEGATIVE_INFINITY, 'USD', 2)).toBe('$0')
    expect(formatDonationAmount(1000, 'CNY', Number.NaN)).toBe('¥1,000')
    expect(formatDonationAmount(1000, 'CNY', Number.POSITIVE_INFINITY)).toBe('¥1,000')
  })

  it('validates crypto USD inputs against minimum and maximum bounds', () => {
    expect(parseCryptoAmountInput('10')).toEqual({ ok: true, normalized: '10', amountMinor: 1000 })
    expect(parseCryptoAmountInput('5.50')).toEqual({ ok: true, normalized: '5.50', amountMinor: 550 })
    expect(parseCryptoAmountInput('4.99')).toEqual({ ok: false, reason: 'below_min' })
    expect(parseCryptoAmountInput('100000.01')).toEqual({ ok: false, reason: 'above_max' })
    expect(parseCryptoAmountInput('10.123')).toEqual({ ok: false, reason: 'invalid' })
    expect(parseCryptoAmountInput('')).toEqual({ ok: false, reason: 'invalid' })
  })

  it('validates crypto checkout fields against amount and Unicode code-point caps', () => {
    const maxName = '😀'.repeat(MAX_DISPLAY_NAME_CODEPOINTS)
    const maxMessage = '风'.repeat(MAX_MESSAGE_CODEPOINTS)

    expect(validateCryptoCheckoutInput({
      rawAmount: '10',
      displayName: `  ${'a'.repeat(MAX_DISPLAY_NAME_CODEPOINTS - 4)}  `,
      message: '   ',
    })).toEqual({
      ok: true,
      amountUsd: '10',
      displayName: 'a'.repeat(MAX_DISPLAY_NAME_CODEPOINTS - 4),
      message: null,
    })

    expect(validateCryptoCheckoutInput({
      rawAmount: '5.50',
      displayName: maxName,
      message: maxMessage,
    })).toEqual({
      ok: true,
      amountUsd: '5.50',
      displayName: maxName,
      message: maxMessage,
    })

    expect(validateCryptoCheckoutInput({
      rawAmount: '10',
      displayName: `${maxName}x`,
      message: '',
    })).toEqual({ ok: false, reason: 'display_name_too_long' })

    expect(validateCryptoCheckoutInput({
      rawAmount: '10',
      displayName: '',
      message: `${maxMessage}x`,
    })).toEqual({ ok: false, reason: 'message_too_long' })

    expect(validateCryptoCheckoutInput({
      rawAmount: '4',
      displayName: `${maxName}x`,
      message: `${maxMessage}x`,
    })).toEqual({ ok: false, reason: 'amount_below_min' })
  })

  it('formats bilingual relative timestamps and counts Unicode code points', () => {
    const now = 1_700_000_000
    expect(formatRelativeTime(now - 20, now, 'zh-CN')).toBe('刚刚')
    expect(formatRelativeTime(now - 20, now, 'en')).toBe('just now')
    expect(formatRelativeTime(now - 300, now, 'zh-CN')).toBe('5 分钟前')
    expect(formatRelativeTime(now - 7200, now, 'en')).toBe('2h ago')
    expect(formatRelativeTime(now - 3 * 86_400, now, 'en')).toBe('3d ago')
    expect(formatRelativeTime(now - 40 * 86_400, now, 'en')).toMatch(/^\d{4}-\d{2}-\d{2}$/)
    expect(formatRelativeTime(Number.NaN, now, 'en')).toBe('')
    expect(formatRelativeTime(MAX_EPOCH_SECONDS + 1, now, 'en')).toBe('')
    expect(formatIsoTimestamp(1_700_000_000)).toBe('2023-11-14T22:13:20.000Z')
    expect(formatIsoTimestamp(Number.NaN)).toBeUndefined()
    expect(formatIsoTimestamp(Number.POSITIVE_INFINITY)).toBeUndefined()
    expect(formatIsoTimestamp(MAX_EPOCH_SECONDS + 1)).toBeUndefined()
    expect(formatIsoTimestamp(-(MAX_EPOCH_SECONDS + 1))).toBeUndefined()
    expect(countCodePoints('风😀')).toBe(2)
  })

  it('encodes AFDIAN_ORDER_URL and V1-V6 boundary payloads into valid ISO/IEC 18004 QR matrices', () => {
    for (const level of ['L', 'M'] as const) {
      const matrix = encodeQrMatrix(AFDIAN_ORDER_URL, level)
      expect(matrix.size).toBe(17 + matrix.version * 4)
      expect(qrMatrixToSvgPath(matrix).startsWith('M4 4h7v1h-7z')).toBe(true)
      const decoded = decodeAndVerifyQr(matrix)
      expect(decoded.level).toBe(level)
      expect(decoded.text).toBe(AFDIAN_ORDER_URL)
    }

    // Exercise Versions 1 through 6 across single-block and multi-block RS layouts.
    for (const sample of ['a', 'x'.repeat(14), 'x'.repeat(26), 'x'.repeat(42), 'x'.repeat(62), 'x'.repeat(84), 'x'.repeat(106)]) {
      const matrix = encodeQrMatrix(sample, 'M')
      expect(decodeAndVerifyQr(matrix).text).toBe(sample)
    }
    expect(() => encodeQrMatrix('x'.repeat(107), 'M')).toThrow(RangeError)
  })

  it('encodes every direct crypto wallet address into a decodable EC-M QR matrix', () => {
    const ids = new Set(DIRECT_CRYPTO_WALLETS.map(wallet => wallet.id))
    expect(ids.size).toBe(6)
    expect(ids.has(DEFAULT_DIRECT_WALLET_ID)).toBe(true)
    expect(DIRECT_CRYPTO_WALLETS.map(wallet => wallet.ticker)).toEqual([
      'BTC',
      'ETH',
      'SOL',
      'SUI',
      'BCH',
      'USDT',
    ])

    for (const wallet of DIRECT_CRYPTO_WALLETS) {
      expect(wallet.address.length).toBeGreaterThan(20)
      expect(wallet.network.length).toBeGreaterThan(0)
      const matrix = encodeQrMatrix(wallet.address, 'M')
      expect(matrix.size).toBe(17 + matrix.version * 4)
      expect(matrix.modules).toHaveLength(matrix.size * matrix.size)
      // Top-left 7x7 finder border is all dark on row 0, cols 0..6.
      for (let x = 0; x < 7; x += 1) {
        expect(matrix.modules[x]).toBe(1)
      }
      // Alternating timing pattern along row 6 between separators.
      for (let x = 8; x < matrix.size - 8; x += 1) {
        expect(matrix.modules[6 * matrix.size + x]).toBe((x + 1) & 1)
      }
      expect(qrMatrixToSvgPath(matrix).startsWith('M4 4h7v1h-7z')).toBe(true)
      const decoded = decodeAndVerifyQr(matrix)
      expect(decoded.level).toBe('M')
      expect(decoded.text).toBe(wallet.address)
    }
  })
})

describe('donate crypto intent origin policy', () => {
  it('allows only hosted official origins by default and rejects localhost in production builds', () => {
    expect(OFFICIAL_DONATE_PAGE_URL).toBe('https://windshare.cc/donate')
    expect([...OFFICIAL_DONATE_ORIGINS]).toEqual([
      'https://ws.ourworld.top',
      'https://windshare.cc',
    ])

    expect(canCreateCryptoIntent('https://ws.ourworld.top')).toBe(true)
    expect(canCreateCryptoIntent('https://windshare.cc')).toBe(true)
    expect(canCreateCryptoIntent('http://localhost:5173')).toBe(false)
    expect(canCreateCryptoIntent('http://127.0.0.1:4173')).toBe(false)
    expect(canCreateCryptoIntent('https://share.example.com')).toBe(false)
    expect(canCreateCryptoIntent('https://ws.ourworld.top.evil.example')).toBe(false)
    expect(canCreateCryptoIntent('not-a-url')).toBe(false)
  })

  it('allows HTTP(S) loopback origins only when explicitly enabled for local development', () => {
    expect(canCreateCryptoIntent('http://localhost:5173', { allowLocalhost: true })).toBe(true)
    expect(canCreateCryptoIntent('http://127.0.0.1:4173', { allowLocalhost: true })).toBe(true)
    expect(canCreateCryptoIntent('https://localhost:8443', { allowLocalhost: true })).toBe(true)
    expect(canCreateCryptoIntent('file://localhost', { allowLocalhost: true })).toBe(false)
    expect(canCreateCryptoIntent('https://share.example.com', { allowLocalhost: true })).toBe(false)
  })
})
