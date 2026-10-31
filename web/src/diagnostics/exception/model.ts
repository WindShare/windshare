export const JAVASCRIPT_EXCEPTION_KINDS = Object.freeze(['type-error', 'dom-exception', 'unknown'] as const)
export type JavaScriptExceptionKind = (typeof JAVASCRIPT_EXCEPTION_KINDS)[number]
export const NATIVE_ERROR_CLASSES = Object.freeze([
  'abort', 'data', 'invalid_state', 'no_modification_allowed', 'not_allowed',
  'not_found', 'not_supported', 'quota_exceeded', 'security', 'timeout',
  'type_error', 'type_mismatch', 'unknown',
] as const)
export type NativeErrorClass = (typeof NATIVE_ERROR_CLASSES)[number]

export interface DiagnosticExceptionProjection {
  readonly javascriptKind: JavaScriptExceptionKind
  readonly nativeClass: NativeErrorClass
  readonly thrownType: string
  readonly constructorName: string | null
  readonly errorName: string | null
  readonly message: string | null
  readonly stack: string | null
  readonly thrownValue: string | null
  readonly cause: DiagnosticExceptionProjection | null
  readonly errors: readonly DiagnosticExceptionProjection[]
  readonly omittedBranches: number
  readonly truncated: readonly string[]
  readonly unavailable: readonly string[]
}
export type DiagnosticExceptionTextProjector = (value: string) => string
export const MAX_EXCEPTION_TEXT_BYTES = 2_048
export const MAX_EXCEPTION_BYTES = 8_192
export const MAX_EXCEPTION_DEPTH = 4
export const MAX_EXCEPTION_BRANCHES = 4
export const MAX_EXCEPTION_NODES = 12
