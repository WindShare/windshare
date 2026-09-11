export const BROWSER_DELIVERY_TRANSITIONS = Object.freeze([
  'placement', 'receiving', 'copy-started', 'copy-failed', 'target-saved',
  'cleanup-failed', 'cleaned', 'discard-started', 'discarded', 'discard-failed',
  'stop-staging-preserved', 'stop-cleanup-pending', 'checkpoint',
] as const)

export interface BrowserDeliveryPayloadV1 {
  readonly operation_id: string
  readonly file_id: string
  readonly transition: (typeof BROWSER_DELIVERY_TRANSITIONS)[number]
  readonly placement?: 'direct' | 'staged'
  readonly placement_reason?: string
  readonly received_bytes?: string
  readonly recoverable_bytes?: string
  readonly copy_milliseconds?: number
  readonly failure_name?: string
  readonly object_id?: string
  readonly checkpoint_stage?: 'started' | 'advanced' | 'deferred' | 'finished' | 'failed'
  readonly pending_bytes?: string
  readonly at_milliseconds?: number
  readonly last_checkpoint_milliseconds?: number
  readonly checkpoint_milliseconds?: number
}
