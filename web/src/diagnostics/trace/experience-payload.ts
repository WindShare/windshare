export type ReceiverExperiencePayloadV1 =
  | Readonly<{
      transition: 'task'
      operation_id: string
      generation: string
      stage: string
      reason: string
      attention: boolean
      completeness: string
      publication: string
    }>
  | Readonly<{
      transition: 'saving'
      projection_epoch: string | null
      choice_id: string | null
      outcome: string | null
      reason: string
    }>
  | Readonly<{
      transition: 'intent'
      action: string
      operation_id: string | null
      generation: string
    }>
