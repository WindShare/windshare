export const RETAINED_ACTION_TRANSITIONS = Object.freeze([
  'started',
  'completed',
  'failed',
  'excluded',
] as const)

export const RETAINED_ACTIONS = Object.freeze([
  'continue',
  'catch-up',
  'save',
  'redownload',
  'discard',
  'delete',
  'save-partial',
  'forget',
] as const)

export const RETAINED_CONTINUATIONS = Object.freeze([
  'resume_receive',
  'pending_catch_up',
  'restoration_available',
  'history_only',
  'resume_package',
  'resume_local_finalization',
  'verify_direct_zip_completion',
  'save_artifact',
  'retry_download',
  'cleanup_incompatible',
  'retry_cleanup',
  'needs_attention',
] as const)

export type RetainedInventoryPayloadV1 =
  | Readonly<{ transition: 'load_started' | 'load_failed' }>
  | Readonly<{ transition: 'load_completed'; operation_count: string }>

export interface RetainedActionPayloadV1 {
  readonly transition: (typeof RETAINED_ACTION_TRANSITIONS)[number]
  readonly action: (typeof RETAINED_ACTIONS)[number]
  readonly continuation: (typeof RETAINED_CONTINUATIONS)[number]
}
