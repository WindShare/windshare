export type CapacityWaitTransitionPayloadV1 = Readonly<{
  transition:
    | 'capacity_retry_scheduled'
    | 'capacity_retry_succeeded'
    | 'capacity_wait_budget_paused'
    | 'capacity_wait_cancelled'
    | 'capacity_generation_replaced'
  capacity_wait_id: string
  capacity_surface: 'revision_open' | 'block_range'
  receive_operation_id: string
  transfer_job_id: string
  protocol_session_id: string
  protocol_operation_id: string
  attempt: string
  sender_hint_ms: number
  jitter_ms: number
  delay_ms: number
  accumulated_wait_ms: number
  active_waiters: number
}>

export type TransferProgressPayloadV1 = Readonly<{
  discovered_files: string
  discovered_bytes: string
  written_bytes: string
  completed_files: string
  completed_bytes: string
  file_errors: string
  selection_errors: string
  failed_directories: string
  content_lanes: number
  capacity_waiting_files: string
  capacity_accumulated_wait_ms: string
  capacity_wait_attempts: string
  capacity_wait_visible: boolean
  discovery: 'open' | 'complete' | 'failed'
  partial: boolean
}>
