export const TRACE_FAILURE_DETAIL_MAX_CHARACTERS = 2_048

export type LaneTransitionPayloadV1 =
  | Readonly<{
      transition:
        | 'attached'
        | 'grant_requested'
        | 'grant_received'
        | 'hello_sent'
        | 'admission_accepted'
        | 'installed'
    }>
  | Readonly<{
      transition: 'admission_rejected'
      rejection_code: number
      retry_after_ms: number
    }>
  | Readonly<{
      transition: 'detached'
      detachment_class: 'closed' | 'physical_failure' | 'authenticated_failure'
      failure_detail?: string
    }>
