/**
 * A deliberately small, side-effect-free formatter for values crossing a
 * diagnostic boundary. Error objects are not JSON data: their causes and
 * AggregateError members are non-enumerable and can themselves form cycles.
 * Walking them here keeps diagnostic consumers from falling back to
 * `String(error)` and accidentally publishing an opaque nested capability.
 */

const DEFAULT_MAXIMUM_DEPTH = 8
const DEFAULT_MAXIMUM_ENTRIES = 64
const DEFAULT_MAXIMUM_STRING_CHARACTERS = 16 * 1024
const UNREADABLE_DIAGNOSTIC_VALUE = '[unreadable diagnostic value]'

export interface RecursiveDiagnosticOptions {
  readonly maxDepth?: number
  readonly maxEntries?: number
  readonly maxStringCharacters?: number
  readonly redactText?: (value: string) => string
}

export interface RecursiveDiagnosticFormatter {
  readonly value: (input: unknown) => unknown
  readonly text: (input: unknown) => string
}

/**
 * Creates a formatter whose output is detached and deeply frozen. The input
 * graph is only read; no `cause`, `errors`, or enumerable property is mutated.
 */
export function createRecursiveDiagnosticFormatter(
  options: RecursiveDiagnosticOptions = {},
): RecursiveDiagnosticFormatter {
  const maxDepth = positiveBound(options.maxDepth, DEFAULT_MAXIMUM_DEPTH)
  const maxEntries = positiveBound(options.maxEntries, DEFAULT_MAXIMUM_ENTRIES)
  const maxStringCharacters = positiveBound(
    options.maxStringCharacters,
    DEFAULT_MAXIMUM_STRING_CHARACTERS,
  )
  const redactText = options.redactText ?? identity

  const value = (input: unknown): unknown => {
    const seen = new WeakSet<object>()
    return formatValue(input, 0, seen, maxDepth, maxEntries, maxStringCharacters, redactText)
  }

  return Object.freeze({
    value,
    text: (input: unknown): string => stringifyDiagnostic(value(input)),
  })
}

/** Formats one value using the default bounded, immutable traversal policy. */
export function formatDiagnosticValue(
  input: unknown,
  options: RecursiveDiagnosticOptions = {},
): unknown {
  return createRecursiveDiagnosticFormatter(options).value(input)
}

/** Formats one value as bounded JSON suitable for structured test output. */
export function formatDiagnosticText(
  input: unknown,
  options: RecursiveDiagnosticOptions = {},
): string {
  return createRecursiveDiagnosticFormatter(options).text(input)
}

function formatValue(
  input: unknown,
  depth: number,
  seen: WeakSet<object>,
  maxDepth: number,
  maxEntries: number,
  maxStringCharacters: number,
  redactText: (value: string) => string,
): unknown {
  try {
    return inspectValue(
      input,
      depth,
      seen,
      maxDepth,
      maxEntries,
      maxStringCharacters,
      redactText,
    )
  } catch {
    // Unknown values can be hostile proxies, revoked cross-realm objects, or
    // redaction callbacks supplied by a diagnostic boundary. Their inspection
    // must fail closed: propagating the thrown value would bypass the formatter
    // and could publish the original capability-bearing error.
    return UNREADABLE_DIAGNOSTIC_VALUE
  }
}

function inspectValue(
  input: unknown,
  depth: number,
  seen: WeakSet<object>,
  maxDepth: number,
  maxEntries: number,
  maxStringCharacters: number,
  redactText: (value: string) => string,
): unknown {
  if (depth > maxDepth) return '[diagnostic depth limit]'
  const primitive = formatPrimitive(input, maxStringCharacters, redactText)
  if (primitive.handled) return primitive.value
  if (typeof input !== 'object' || input === null) return '[unprintable diagnostic value]'

  if (seen.has(input)) return '[diagnostic cycle]'
  seen.add(input)
  try {
    return formatObject(
      input,
      depth,
      seen,
      maxDepth,
      maxEntries,
      maxStringCharacters,
      redactText,
    )
  } finally {
    seen.delete(input)
  }
}

function formatPrimitive(
  input: unknown,
  maxStringCharacters: number,
  redactText: (value: string) => string,
): { readonly handled: boolean; readonly value?: unknown } {
  if (input === null || typeof input === 'boolean' || typeof input === 'number') {
    return { handled: true, value: input }
  }
  if (typeof input === 'string') {
    return { handled: true, value: boundedText(redactText(input), maxStringCharacters) }
  }
  if (typeof input === 'bigint') return { handled: true, value: `${input}n` }
  if (typeof input === 'undefined') return { handled: true, value: '[undefined]' }
  if (typeof input === 'symbol') {
    return { handled: true, value: boundedText(redactText(String(input)), maxStringCharacters) }
  }
  if (typeof input === 'function') {
    const name = input.name || 'anonymous'
    return { handled: true, value: boundedText(redactText(`[function ${name}]`), maxStringCharacters) }
  }
  return { handled: false }
}

function formatObject(
  input: object,
  depth: number,
  seen: WeakSet<object>,
  maxDepth: number,
  maxEntries: number,
  maxStringCharacters: number,
  redactText: (value: string) => string,
): unknown {
  if (input instanceof Error) {
    return formatError(input, depth, seen, maxDepth, maxEntries, maxStringCharacters, redactText)
  }
  if (Array.isArray(input)) {
    const items: unknown[] = []
    for (const item of input.slice(0, maxEntries)) {
      items.push(formatValue(item, depth + 1, seen, maxDepth, maxEntries, maxStringCharacters, redactText))
    }
    if (input.length > maxEntries) items.push(`[${input.length - maxEntries} entries omitted]`)
    return Object.freeze(items)
  }
  if (isPlainObject(input)) return formatPlainObject(
    input,
    depth,
    seen,
    maxDepth,
    maxEntries,
    maxStringCharacters,
    redactText,
  )
  // Objects such as URL, Buffer, and DOM exceptions can expose sensitive
  // state through custom inspection hooks. Keep only their bounded string
  // representation instead of invoking arbitrary enumerable getters.
  return boundedText(redactText(safeObjectString(input)), maxStringCharacters)
}

function formatPlainObject(
  input: Record<string, unknown>,
  depth: number,
  seen: WeakSet<object>,
  maxDepth: number,
  maxEntries: number,
  maxStringCharacters: number,
  redactText: (value: string) => string,
): Readonly<Record<string, unknown>> {
  const result: Record<string, unknown> = {}
  const keys = Object.keys(input).sort()
  for (const key of keys.slice(0, maxEntries)) {
    result[boundedText(redactText(key), maxStringCharacters)] = formatValue(
      readProperty(input, key),
      depth + 1,
      seen,
      maxDepth,
      maxEntries,
      maxStringCharacters,
      redactText,
    )
  }
  if (keys.length > maxEntries) result['…'] = `[${keys.length - maxEntries} properties omitted]`
  return Object.freeze(result)
}

function formatError(
  error: Error,
  depth: number,
  seen: WeakSet<object>,
  maxDepth: number,
  maxEntries: number,
  maxStringCharacters: number,
  redactText: (value: string) => string,
): Readonly<Record<string, unknown>> {
  const result: Record<string, unknown> = {
    name: boundedText(redactText(safeErrorName(error)), maxStringCharacters),
    message: boundedText(redactText(safeErrorMessage(error)), maxStringCharacters),
  }
  const stack = readProperty(error, 'stack')
  if (typeof stack === 'string' && stack.length > 0) {
    result.stack = boundedText(redactText(stack), maxStringCharacters)
  }
  const cause = readProperty(error, 'cause')
  if (cause !== undefined) {
    result.cause = formatValue(cause, depth + 1, seen, maxDepth, maxEntries, maxStringCharacters, redactText)
  }
  if (error instanceof AggregateError) {
    result.errors = formatValue(
      error.errors,
      depth + 1,
      seen,
      maxDepth,
      maxEntries,
      maxStringCharacters,
      redactText,
    )
  }
  return Object.freeze(result)
}

function isPlainObject(value: object): value is Record<string, unknown> {
  const prototype = Object.getPrototypeOf(value)
  return prototype === Object.prototype || prototype === null
}

function readProperty(value: object, key: string): unknown {
  try {
    return Reflect.get(value, key)
  } catch {
    return '[unreadable diagnostic property]'
  }
}

function safeErrorName(error: Error): string {
  try {
    return typeof error.name === 'string' && error.name.length > 0 ? error.name : 'Error'
  } catch {
    return 'Error'
  }
}

function safeErrorMessage(error: Error): string {
  try {
    return typeof error.message === 'string' ? error.message : String(error.message)
  } catch {
    return 'Error without a readable message'
  }
}

function safeObjectString(value: object): string {
  try {
    return Object.prototype.toString.call(value)
  } catch {
    return '[unprintable diagnostic object]'
  }
}

function boundedText(value: string, maximum: number): string {
  return value.length <= maximum ? value : `${value.slice(0, maximum)}…`
}

function positiveBound(value: number | undefined, fallback: number): number {
  if (value === undefined) return fallback
  if (Number.isSafeInteger(value) && value > 0) return value
  throw new RangeError('Diagnostic formatter bounds must be positive safe integers')
}

function stringifyDiagnostic(value: unknown): string {
  try {
    const encoded = JSON.stringify(value)
    return encoded === undefined ? '"[unserializable diagnostic]"' : encoded
  } catch {
    return '"[unserializable diagnostic]"'
  }
}

function identity(value: string): string {
  return value
}
