// Compile-time encoded Version 4 (33x33, EC-L) QR matrix for AFDIAN_ORDER_URL:
// https://afdian.com/order/create?user_id=735de47ce57c11f1b2c95254001e7c00
// Packed as row-major MSB-first bits so the /donate chunk needs zero QR library runtime.
export const AFDIAN_QR_MODULES = 33
export const AFDIAN_QR_QUIET_ZONE = 4
export const AFDIAN_QR_VIEW_SIZE = AFDIAN_QR_MODULES + AFDIAN_QR_QUIET_ZONE * 2

const AFDIAN_QR_BITS_BASE64 =
  '/uwhP8FzSVBurDMLt1q1NdugepLsFNCBB/qqqv4AnegAzhL2l5i0hakWpntk1Yab3IU653rYoDxOAFoctdxBV9jInj7EKJqnl6oRlGB6yxaNu4Fqpe6ODn0aoJoltaxao8vg0sru+QBlgUW/m1qqUFJ/kTurLm+V0liBculklLkFjenw/rJXkIA='

function decodeBase64Bytes(encoded: string): Uint8Array {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/'
  let cleanLength = encoded.length
  while (cleanLength > 0 && encoded[cleanLength - 1] === '=') cleanLength -= 1
  const out = new Uint8Array(Math.floor((cleanLength * 3) / 4))
  let outIndex = 0
  for (let i = 0; i < cleanLength; i += 4) {
    const c0 = alphabet.indexOf(encoded[i] ?? 'A')
    const c1 = alphabet.indexOf(encoded[i + 1] ?? 'A')
    const c2 = i + 2 < cleanLength ? alphabet.indexOf(encoded[i + 2] ?? 'A') : 0
    const c3 = i + 3 < cleanLength ? alphabet.indexOf(encoded[i + 3] ?? 'A') : 0
    const bits = (c0 << 18) | (c1 << 12) | (c2 << 6) | c3
    out[outIndex++] = (bits >> 16) & 0xff
    if (i + 2 < cleanLength) out[outIndex++] = (bits >> 8) & 0xff
    if (i + 3 < cleanLength) out[outIndex++] = bits & 0xff
  }
  return out
}

export function decodeAfdianQrMatrix(): Uint8Array {
  const bytes = decodeBase64Bytes(AFDIAN_QR_BITS_BASE64)
  const total = AFDIAN_QR_MODULES * AFDIAN_QR_MODULES
  const cells = new Uint8Array(total)
  for (let i = 0; i < total; i += 1) {
    const byte = bytes[i >> 3] ?? 0
    cells[i] = (byte >> (7 - (i & 7))) & 1
  }
  return cells
}

function buildSvgPath(cells: Uint8Array): string {
  const parts: string[] = []
  for (let y = 0; y < AFDIAN_QR_MODULES; y += 1) {
    const rowOffset = y * AFDIAN_QR_MODULES
    let x = 0
    while (x < AFDIAN_QR_MODULES) {
      if (cells[rowOffset + x] === 1) {
        const start = x
        while (x < AFDIAN_QR_MODULES && cells[rowOffset + x] === 1) {
          x += 1
        }
        const run = x - start
        parts.push(`M${start + AFDIAN_QR_QUIET_ZONE} ${y + AFDIAN_QR_QUIET_ZONE}h${run}v1h-${run}z`)
      } else {
        x += 1
      }
    }
  }
  return parts.join('')
}

export const AFDIAN_QR_PATH = buildSvgPath(decodeAfdianQrMatrix())
