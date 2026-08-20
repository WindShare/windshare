export const OUTPUT_JAVASCRIPT_EXCEPTION_KINDS = Object.freeze([
  'type-error',
  'dom-exception',
  'unknown',
] as const)

export type OutputJavaScriptExceptionKind =
  (typeof OUTPUT_JAVASCRIPT_EXCEPTION_KINDS)[number]

export const OUTPUT_NATIVE_ERROR_CLASSES = Object.freeze([
  'abort',
  'data',
  'invalid_state',
  'no_modification_allowed',
  'not_allowed',
  'not_found',
  'not_supported',
  'quota_exceeded',
  'security',
  'timeout',
  'type_error',
  'type_mismatch',
  'unknown',
] as const)

export type OutputNativeErrorClass = (typeof OUTPUT_NATIVE_ERROR_CLASSES)[number]

export interface NormalizedOutputException {
  readonly javascriptKind: OutputJavaScriptExceptionKind
  readonly nativeClass: OutputNativeErrorClass
  readonly thrownType: string
  readonly constructorName: string | null
  readonly errorName: string | null
  readonly domExceptionName: string | null
  readonly message: string
  readonly stack: string | null
  readonly thrownValue: string
  readonly cause: string | null
}

/**
 * Only genuine platform objects select an error class. Name-like properties on
 * arbitrary thrown values remain diagnostic text and acquire no recovery meaning.
 */
export function normalizeOutputException(thrown: unknown): NormalizedOutputException {
  const domException = isNativeDOMException(thrown) ? thrown : undefined
  const typeError = domException === undefined && isNativeTypeError(thrown)
    ? thrown
    : undefined
  const error = domException ?? typeError ?? (isNativeError(thrown) ? thrown : undefined)

  return Object.freeze({
    javascriptKind: javascriptExceptionKind(domException, typeError),
    nativeClass: nativeErrorClass(domException, typeError),
    thrownType: thrown === null ? 'null' : typeof thrown,
    constructorName: safeConstructorName(error ?? thrown),
    errorName: error === undefined ? null : safeErrorName(error),
    domExceptionName: domException === undefined ? null : safeErrorName(domException),
    message: error === undefined ? safeString(thrown) : safeErrorMessage(error),
    stack: error === undefined ? null : safeErrorStack(error),
    thrownValue: safeString(thrown),
    cause: error === undefined ? null : safeErrorCause(error),
  })
}

function javascriptExceptionKind(
  domException: DOMException | undefined,
  typeError: TypeError | undefined,
): OutputJavaScriptExceptionKind {
  if (domException !== undefined) return 'dom-exception'
  if (typeError !== undefined) return 'type-error'
  return 'unknown'
}

function nativeErrorClass(
  domException: DOMException | undefined,
  typeError: TypeError | undefined,
): OutputNativeErrorClass {
  if (domException !== undefined) return classForDOMException(safeErrorName(domException) ?? '')
  if (typeError !== undefined) return 'type_error'
  return 'unknown'
}

function classForDOMException(name: string): OutputNativeErrorClass {
  switch (name) {
    case 'AbortError': return 'abort'
    case 'DataError': return 'data'
    case 'InvalidStateError': return 'invalid_state'
    case 'NoModificationAllowedError': return 'no_modification_allowed'
    case 'NotAllowedError': return 'not_allowed'
    case 'NotFoundError': return 'not_found'
    case 'NotSupportedError': return 'not_supported'
    case 'QuotaExceededError': return 'quota_exceeded'
    case 'SecurityError': return 'security'
    case 'TimeoutError': return 'timeout'
    case 'TypeMismatchError': return 'type_mismatch'
    default: return 'unknown'
  }
}

function isNativeDOMException(value: unknown): value is DOMException {
  try {
    return typeof DOMException === 'function' && value instanceof DOMException
  } catch {
    return false
  }
}

function isNativeTypeError(value: unknown): value is TypeError {
  try {
    return value instanceof TypeError
  } catch {
    return false
  }
}

function isNativeError(value: unknown): value is Error {
  try {
    return value instanceof Error
  } catch {
    return false
  }
}

function safeConstructorName(value: unknown): string | null {
  if ((typeof value !== 'object' || value === null) && typeof value !== 'function') return null
  try {
    const constructor = Reflect.get(value, 'constructor')
    if (typeof constructor !== 'function') return null
    const name = Reflect.get(constructor, 'name')
    return typeof name === 'string' && name.length > 0 ? name : null
  } catch {
    return null
  }
}

function safeErrorName(error: Error): string | null {
  try {
    return typeof error.name === 'string' && error.name.length > 0 ? error.name : null
  } catch {
    return null
  }
}

function safeErrorMessage(error: Error): string {
  try {
    return typeof error.message === 'string' ? error.message : safeString(error.message)
  } catch {
    return '[unreadable exception message]'
  }
}

function safeErrorStack(error: Error): string | null {
  try {
    return typeof error.stack === 'string' && error.stack.length > 0 ? error.stack : null
  } catch {
    return null
  }
}

function safeErrorCause(error: Error): string | null {
  try {
    const cause = Reflect.get(error, 'cause')
    return cause === undefined ? null : safeString(cause)
  } catch {
    return '[unreadable exception cause]'
  }
}

function safeString(value: unknown): string {
  try {
    return String(value)
  } catch {
    return '[unprintable thrown value]'
  }
}
