import {
  createRecursiveDiagnosticFormatter,
  type RecursiveDiagnosticFormatter,
} from './diagnostic-formatter'

const REDACTED_COMPLETE_URL = '[capability-url redacted]'
const REDACTED_FRAGMENT = '[capability-fragment redacted]'
const REDACTED_SEPARATE_KEY = '[separate-key redacted]'

export const CAPABILITY_REDACTION_MARKERS = Object.freeze({
  completeUrl: REDACTED_COMPLETE_URL,
  fragment: REDACTED_FRAGMENT,
  separateKey: REDACTED_SEPARATE_KEY,
})

export interface CapabilityRedactionValues {
  /** The exact URL handed to a browser navigation or transport boundary. */
  readonly completeUrl?: string
  /** The exact URL fragment, with or without its leading `#`. */
  readonly fragment?: string
  /** A separately entered suite-02 key. */
  readonly separateKey?: string
}

export interface CapabilityRedactor extends RecursiveDiagnosticFormatter {
  readonly redactText: (value: string) => string
  readonly clear: () => void
}

/**
 * Builds a per-invocation redactor from the actual capability values. A global
 * canary is intentionally not part of this API: tests must prove that the
 * values used by the invocation are the values that are removed.
 */
export function createCapabilityRedactor(
  values: CapabilityRedactionValues,
): CapabilityRedactor {
  let completeUrl = nonEmpty(values.completeUrl)
  let fragment = nonEmpty(values.fragment)
  let separateKey = nonEmpty(values.separateKey)
  const formatter = createRecursiveDiagnosticFormatter({
    redactText,
  })

  const redactor: CapabilityRedactor = {
    redactText,
    value: (input) => formatter.value(input),
    text: (input) => formatter.text(input),
    clear: () => {
      // Dropping references at the lifecycle boundary prevents later
      // asynchronous diagnostics from accidentally reusing a stale secret.
      completeUrl = undefined
      fragment = undefined
      separateKey = undefined
    },
  }
  return Object.freeze(redactor)

  function redactText(value: string): string {
    let result = value
    // Replace the complete URL first. It contains the fragment, so doing the
    // shorter replacements first would leave a partially capability-bearing
    // URL in Playwright or attachment diagnostics.
    if (completeUrl !== undefined) result = replaceActual(result, completeUrl, REDACTED_COMPLETE_URL)
    if (fragment !== undefined) {
      result = replaceActual(result, fragment, REDACTED_FRAGMENT)
      const withoutHash = fragment.startsWith('#') ? fragment.slice(1) : fragment
      if (withoutHash.length > 0) result = replaceActual(result, withoutHash, REDACTED_FRAGMENT)
      if (!fragment.startsWith('#')) result = replaceActual(result, `#${fragment}`, REDACTED_FRAGMENT)
    }
    if (separateKey !== undefined) result = replaceActual(result, separateKey, REDACTED_SEPARATE_KEY)
    return result
  }
}

/** Redacts a string using actual values for one capability invocation. */
export function redactCapabilityText(
  value: string,
  values: CapabilityRedactionValues,
): string {
  const redactor = createCapabilityRedactor(values)
  try {
    return redactor.redactText(value)
  } finally {
    redactor.clear()
  }
}

/** Returns an immutable, recursively redacted diagnostic value. */
export function redactCapabilityValue(
  value: unknown,
  values: CapabilityRedactionValues,
): unknown {
  const redactor = createCapabilityRedactor(values)
  try {
    return redactor.value(value)
  } finally {
    redactor.clear()
  }
}

/** Returns a recursively formatted, redacted diagnostic JSON string. */
export function formatCapabilityDiagnostic(
  value: unknown,
  values: CapabilityRedactionValues,
): string {
  const redactor = createCapabilityRedactor(values)
  try {
    return redactor.text(value)
  } finally {
    redactor.clear()
  }
}

/**
 * Runs a capability-bearing operation and converts any thrown value to a
 * redacted Error. The operation still receives the real value; only its
 * failure boundary is sanitized.
 */
export async function withCapabilityRedaction<T>(
  operation: () => Promise<T>,
  values: CapabilityRedactionValues,
): Promise<T> {
  const redactor = createCapabilityRedactor(values)
  try {
    return await operation()
  } catch (error) {
    const message = redactor.redactText(publicErrorText(error))
    // The original error is intentionally replaced by an immutable redacted
    // snapshot; retaining the raw cause would recreate the capability leak.
    // eslint-disable-next-line preserve-caught-error -- safe cause is the only permitted boundary value
    throw new Error(message, { cause: redactor.value(error) })
  } finally {
    redactor.clear()
  }
}

export function capabilityRedactionValuesFromInput(
  input: string,
  pageUrl?: string,
): CapabilityRedactionValues {
  const trimmed = input.trim()
  const result: CapabilityRedactionValues = { separateKey: trimmed }
  try {
    const url = new URL(trimmed.includes('://') ? trimmed : `${pageUrl ?? ''}${trimmed}`)
    return Object.freeze({
      ...(trimmed.includes('://') ? { completeUrl: trimmed } : {}),
      fragment: url.hash,
      separateKey: trimmed,
    })
  } catch {
    return Object.freeze(result)
  }
}

function nonEmpty(value: string | undefined): string | undefined {
  return value === undefined || value.length === 0 ? undefined : value
}

function replaceActual(value: string, actual: string, replacement: string): string {
  return actual.length === 0 ? value : value.split(actual).join(replacement)
}

function publicErrorText(error: unknown): string {
  try {
    if (error instanceof Error && error.message.length > 0) return error.message
  } catch {
    // A hostile Error getter must not escape the capability redaction boundary.
  }
  if (typeof error === 'string' && error.length > 0) return error
  return 'An unexpected capability operation error occurred.'
}
