export type DownloadErrorCode =
  | 'invalid-plan'
  | 'invalid-layout'
  | 'block-not-selected'
  | 'duplicate-block'
  | 'out-of-order'
  | 'invalid-state'
  | 'output-write'
  | 'output-finalize'
  | 'cleanup-failed'
  | 'unsupported-target'

export class DownloadError extends Error {
  readonly code: DownloadErrorCode

  constructor(code: DownloadErrorCode, message: string, cause?: unknown) {
    super(message, { cause })
    this.name = 'DownloadError'
    this.code = code
  }
}
