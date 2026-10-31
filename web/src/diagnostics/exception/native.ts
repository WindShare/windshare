import type { JavaScriptExceptionKind, NativeErrorClass } from './model'

export function javascriptExceptionKind(
  domException: DOMException | undefined,
  typeError: TypeError | undefined,
): JavaScriptExceptionKind {
  if (domException !== undefined) return 'dom-exception'
  if (typeError !== undefined) return 'type-error'
  return 'unknown'
}

export function nativeErrorClass(
  domException: DOMException | undefined,
  typeError: TypeError | undefined,
  errorName: string | null,
): NativeErrorClass {
  if (domException !== undefined) return classForDOMException(errorName ?? '')
  if (typeError !== undefined) return 'type_error'
  return 'unknown'
}

function classForDOMException(name: string): NativeErrorClass {
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

export function isNativeDOMException(value: unknown): value is DOMException {
  try {
    return typeof DOMException === 'function' && value instanceof DOMException
  } catch {
    return false
  }
}

export function isNativeTypeError(value: unknown): value is TypeError {
  try {
    return value instanceof TypeError
  } catch {
    return false
  }
}

export function isNativeError(value: unknown): value is Error {
  try {
    return value instanceof Error
  } catch {
    return false
  }
}
