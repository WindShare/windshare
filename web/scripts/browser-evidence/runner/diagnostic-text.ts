export function normalizedViolations(violations: readonly string[]): readonly string[] {
  return Object.freeze([...new Set(violations.map((violation) => boundedText(violation, 1_024)))].sort())
}

export function boundedMessage(value: unknown): string {
  const message = value instanceof Error ? value.message : String(value)
  return boundedText(message || 'unknown runner error', 512)
}

export function boundedText(value: string, maximumBytes: number): string {
  const normalized = value.normalize('NFC')
  let result = ''
  let bytes = 0
  for (const character of normalized) {
    const width = Buffer.byteLength(character, 'utf8')
    if (bytes + width > maximumBytes) break
    result += character
    bytes += width
  }
  return result || 'unknown runner error'
}
