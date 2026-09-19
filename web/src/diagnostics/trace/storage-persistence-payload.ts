export const STORAGE_PERSISTENCE_TRANSITIONS = Object.freeze([
  'requested',
  'already_pending',
  'granted',
  'not_granted',
  'failed',
  'unavailable',
] as const)

export interface StoragePersistencePayloadV1 {
  readonly operation_id: string
  readonly request_operation_id: string
  readonly transition: (typeof STORAGE_PERSISTENCE_TRANSITIONS)[number]
}
