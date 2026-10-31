const encoder = new TextEncoder()
const JSON_SHORT_ESCAPES = new Set([8, 9, 10, 12, 13])

/** Bound both UTF-8 storage and JSON escaping without allocating once per character. */
export function boundedDiagnosticText(value: string, maximumBytes: number): string {
  let bytes = 0
  let end = 0
  while (end < value.length) {
    const scalar = value.codePointAt(end)!
    const size = encodedScalarBytes(scalar)
    if (bytes + size > maximumBytes) break
    bytes += size
    end += scalar > 0xffff ? 2 : 1
  }
  return value.slice(0, end)
}

export function isBoundedDiagnosticText(value: unknown, maximumBytes: number): value is string {
  // Capture and validation must agree on UTF-8 storage and JSON escaping.
  return typeof value === 'string' && boundedDiagnosticText(value, maximumBytes) === value
}

function encodedScalarBytes(scalar: number): number {
  if (scalar === 34 || scalar === 92 || JSON_SHORT_ESCAPES.has(scalar)) return 2
  if (scalar < 32 || (scalar >= 0xd800 && scalar <= 0xdfff)) return 6
  if (scalar < 0x80) return 1
  if (scalar < 0x800) return 2
  return scalar <= 0xffff ? 3 : 4
}

export function diagnosticBytes(value: unknown): number {
  return encoder.encode(JSON.stringify(value)).byteLength
}
