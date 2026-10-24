export const MATERIALIZATION_FAILURE_REASONS = Object.freeze([
  'file-open-failed',
  'source-revision-changed',
  'content-read-failed',
  'output-write-failed',
  'output-commit-failed',
  'directory-finalize-failed',
] as const)

export type MaterializationFailureReason =
  (typeof MATERIALIZATION_FAILURE_REASONS)[number]
