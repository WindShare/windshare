export type PersistentOutputErrorKind =
  | 'authorization'
  | 'exclusive-create'
  | 'incomplete-file'
  | 'journal-binding'
  | 'output-identity'
  | 'output-state'
  | 'resource-limit'

export class PersistentOutputError extends Error {
  readonly kind: PersistentOutputErrorKind
  readonly cause: unknown

  constructor(kind: PersistentOutputErrorKind, message: string, cause?: unknown) {
    super(message, { cause })
    this.name = 'PersistentOutputError'
    this.kind = kind
    this.cause = cause
  }
}
