const FAILURE_KINDS = Object.freeze([
  'usage',
  'build',
  'artifact-policy',
  'stale-output',
  'publication',
  'cleanup',
])

const FAILURE_CODE_PATTERN = /^[a-z][a-z0-9-]{0,127}$/u
const MAXIMUM_FAILURE_MESSAGE_LENGTH = 4_096

export class GeneratedSemanticFailureError extends Error {
  constructor(kind, code, message, options = undefined) {
    const failure = createGeneratedSemanticFailure(kind, code, message)
    super(failure.message, options)
    this.name = 'GeneratedSemanticFailureError'
    this.failure = failure
  }
}

export function createGeneratedSemanticFailure(kind, code, message) {
  if (!FAILURE_KINDS.includes(kind)) {
    throw new TypeError(`unsupported generated semantic failure kind ${JSON.stringify(kind)}`)
  }
  if (typeof code !== 'string' || !FAILURE_CODE_PATTERN.test(code)) {
    throw new TypeError('generated semantic failure code is invalid')
  }
  if (typeof message !== 'string' || message.length === 0) {
    throw new TypeError('generated semantic failure message is required')
  }
  return Object.freeze({
    kind,
    code,
    message: message.slice(0, MAXIMUM_FAILURE_MESSAGE_LENGTH),
  })
}

export function throwGeneratedSemanticFailure(kind, code, message, cause = undefined) {
  throw new GeneratedSemanticFailureError(
    kind,
    code,
    message,
    cause === undefined ? undefined : { cause },
  )
}

export function failureFromCause(cause, fallbackKind, fallbackCode, fallbackMessage) {
  try {
    if (
      cause instanceof GeneratedSemanticFailureError &&
      isGeneratedSemanticFailure(cause.failure)
    ) return cause.failure
  } catch {
    // Hostile proxy and accessor causes must collapse into the typed fallback.
  }
  return createGeneratedSemanticFailure(
    fallbackKind,
    fallbackCode,
    generatedSemanticCauseMessage(cause, fallbackMessage),
  )
}

export function generatedSemanticCauseMessage(cause, fallbackMessage) {
  if (typeof fallbackMessage !== 'string' || fallbackMessage.length === 0) {
    throw new TypeError('generated semantic fallback failure message is required')
  }
  try {
    const message = cause instanceof Error ? cause.message : undefined
    return typeof message === 'string' && message.length > 0 ? message : fallbackMessage
  } catch {
    return fallbackMessage
  }
}

export function isGeneratedSemanticFailure(value) {
  try {
    return value !== null && typeof value === 'object' &&
      FAILURE_KINDS.includes(value.kind) &&
      typeof value.code === 'string' && FAILURE_CODE_PATTERN.test(value.code) &&
      typeof value.message === 'string' && value.message.length > 0 &&
      value.message.length <= MAXIMUM_FAILURE_MESSAGE_LENGTH &&
      Object.keys(value).length === 3
  } catch {
    return false
  }
}
