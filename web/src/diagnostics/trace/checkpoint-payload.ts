export type CheckpointPayloadV1 =
  | Readonly<{
      backend: 'file_system_access' | 'origin_private'
      transition: 'restored' | 'persisted' | 'quarantined' | 'failed'
      decision?:
        | 'absent'
        | 'installed'
        | 'exact'
        | 'revision_conflict'
        | 'ownership_conflict'
        | 'invalid'
    }>
  | Readonly<{
      backend: 'file_system_access'
      transition: 'authority_decision'
      authority: 'automatic_admission' | 'preserving_capacity'
      receive_operation_id: string
      transfer_job_id: string
      output_session_id: string
      materialization_relative_path: readonly string[]
      trigger: 'pending_bytes' | 'pending_time' | 'paused_file_recovery'
      checkpoint_ordinal?: number
      prefix_copy_bytes: string
      write_amplification_bytes: string
      temporary_bytes: string
      remaining_automatic_write_amplification_bytes?: string
      decision:
        | 'admitted'
        | 'checkpoint_priority'
        | 'prefix_copy_budget'
        | 'cumulative_write_amplification_budget'
        | 'capacity_unavailable'
        | 'paused_recovery_queued'
        | 'paused_recovery_admitted'
        | 'committed'
        | 'released'
      release_reason?:
        | 'unused'
        | 'capacity_unavailable'
        | 'replacement_open_failed'
        | 'writer_closed'
        | 'writer_aborted'
        | 'file_committed'
        | 'file_paused'
        | 'file_retired'
        | 'cancelled'
        | 'terminal_drain'
        | 'automatic_handoff'
    }>
