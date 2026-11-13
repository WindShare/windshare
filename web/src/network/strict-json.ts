const MAXIMUM_DEPTH = 16

/** Reject ambiguous member names before interpreting signed deployment policy. */
export function strictJSON(text: string): unknown {
  const stack: (Set<string> | null)[] = []
  let index = 0
  while (index < text.length) {
    const token = text[index++]
    if (token === '{' || token === '[') {
      stack.push(token === '{' ? new Set() : null)
      if (stack.length > MAXIMUM_DEPTH) throw new TypeError('Network directory nesting limit')
    } else if (token === '}' || token === ']') stack.pop()
    else if (token === '"') index = inspectString(text, index - 1, stack.at(-1))
    else if (token === '-' || /\d/.test(token!)) index = inspectInteger(text, index - 1)
  }
  return JSON.parse(text) as unknown
}

function inspectInteger(source: string, start: number): number {
  const token = /^-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/.exec(source.slice(start))?.[0]
  // Go's integer fields reject fractional and exponential spellings even when
  // their numerical value happens to be integral.
  if (!token || /[.eE]/.test(token)) throw new TypeError('Invalid network integer spelling')
  return start + token.length
}

function inspectString(source: string, start: number, names: Set<string> | null | undefined): number {
  let end = start + 1
  while (end < source.length && source[end] !== '"') {
    if (source[end] === '\\') end++
    end++
  }
  end++
  const value = JSON.parse(source.slice(start, end)) as string
  let next = end
  while (next < source.length && /\s/.test(source[next]!)) next++
  if (source[next] === ':' && names) {
    if (names.has(value)) throw new TypeError('Duplicate network JSON member')
    names.add(value)
  }
  return end
}

export function object(value: unknown, keys: readonly string[]): Record<string, unknown> {
  if (value === null || typeof value !== 'object' || Array.isArray(value) || Object.keys(value).some(key => !keys.includes(key))) {
    throw new TypeError('Invalid network directory fields')
  }
  return value as Record<string, unknown>
}

export function text(value: unknown): string {
  if (typeof value !== 'string' || value === '' || new TextEncoder().encode(value).length > 128 || value.trim() !== value || /[\0\r\n\t]/.test(value)) throw new TypeError('Invalid network directory text')
  return value
}

export function counter(value: unknown): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 1) throw new TypeError('Invalid network directory counter')
  return value
}

export function timestamp(value: unknown): number {
  if (typeof value !== 'string' || !/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?(?:Z|[+-]\d\d:\d\d)$/.test(value)) throw new TypeError('Invalid network timestamp')
  const year = Number(value.slice(0, 4)), month = Number(value.slice(5, 7)), day = Number(value.slice(8, 10))
  const endOfMonth = new Date(0)
  endOfMonth.setUTCFullYear(year, month, 0)
  if (month < 1 || month > 12 || day < 1 || day > endOfMonth.getUTCDate() || Number(value.slice(11, 13)) > 23) throw new TypeError('Invalid network timestamp')
  const result = Date.parse(value)
  if (!Number.isFinite(result) || value === '0001-01-01T00:00:00Z') throw new TypeError('Invalid network timestamp')
  return result
}

export function array(value: unknown, maximum: number): readonly unknown[] {
  if (value === null || value === undefined) return []
  if (!Array.isArray(value) || value.length > maximum) throw new TypeError('Invalid network directory array')
  return value as unknown[]
}
